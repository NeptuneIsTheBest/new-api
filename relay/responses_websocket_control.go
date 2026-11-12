package relay

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Inject and interrupt acknowledgements can follow the response's terminal. Retain only
// routing metadata and an input estimate, not the finished worker or its input.
// All entries and their byte count are protected by stateMu.
type responsesWSPendingControl struct {
	eventType, responseID, eventID, streamID string
	size                                     int64
	inputTokens                              int
}

type responsesWSInputReservation struct {
	tokens int
	err    *types.NewAPIError
}

// The caller holds stateMu. Injects do not occupy the cancel/interrupt slot.
func (s *responsesWSSession) hasPendingInterruptLocked(responseID string) bool {
	return slices.ContainsFunc(s.responseControls, func(pending *responsesWSPendingControl) bool {
		return pending.eventType == "response.interrupt" && pending.responseID == responseID
	})
}

func (s *responsesWSSession) submitResponseControl(body []byte, event responsesWSCreateEvent, streamID string) (string, *types.NewAPIError) {
	invalidMessage, limitMessage := i18n.MsgResponsesWSInvalidInterrupt, i18n.MsgResponsesWSInterruptLimit
	valid := strings.TrimSpace(event.ResponseID) != ""
	var injectInput common.RawMessage
	if event.Type == "response.inject" {
		invalidMessage, limitMessage = i18n.MsgResponsesWSInvalidInject, i18n.MsgResponsesWSInjectLimit
		var input gjson.Result
		inputCount := 0
		// The original frame is forwarded, so input must be unambiguous even
		// when equivalent member names use different JSON escapes.
		gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
			if key.Str == "input" {
				inputCount++
				input = value
			}
			return inputCount < 2
		})
		valid = valid && inputCount == 1 && input.IsArray()
		if valid {
			injectInput = common.RawMessage(input.Raw)
		}
	}
	if !valid {
		return streamID, newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, invalidMessage)))
	}
	s.stateMu.Lock()
	lane, known := s.completedResponses[event.ResponseID]
	for _, pending := range s.responseControls {
		if pending.responseID == event.ResponseID {
			// Outstanding acknowledgements can outlive completion history.
			lane, known = pending.streamID, true
			break
		}
	}
	var owner *responsesWSCallState
	for id, stream := range s.streams {
		if state := stream.current; state != nil && state.responseID == event.ResponseID {
			lane, known, owner = id, true, state
			break
		}
		if stream.lastResponseID == event.ResponseID {
			lane, known = id, true
		}
	}
	if !known || streamID != "" && streamID != lane || s.closing {
		s.stateMu.Unlock()
		return streamID, types.WithOpenAIError(types.OpenAIError{
			Type: "invalid_request_error", Code: "response_not_found", Param: "response_id",
			Message: i18n.Translate(s.lang, i18n.MsgResponsesWSResponseNotFound),
		}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if event.Type == "response.interrupt" && (s.hasPendingInterruptLocked(event.ResponseID) || owner != nil && owner.cancelPending) {
		s.stateMu.Unlock()
		return lane, newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSControlPending)))
	}
	size := int64(len(event.Type)) + int64(len(event.ResponseID)) + int64(len(event.EventID)) + int64(len(lane))
	if len(s.responseControls) >= responsesWSMaxQueued || size > s.maxQueuedBytes-s.responseControlBytes {
		s.stateMu.Unlock()
		return lane, types.NewErrorWithStatusCode(errors.New(i18n.Translate(s.lang, limitMessage)), types.ErrorCodeInvalidRequest, http.StatusTooManyRequests, types.ErrOptionWithSkipRetry())
	}
	reserveInput := event.Type == "response.inject" && constant.CountToken && owner != nil && !owner.upstreamDone
	var modelName string
	if reserveInput {
		modelName = owner.create.Request.Model
	}
	s.stateMu.Unlock()
	// Sensitive checks also apply when counting is disabled or the response has finished.
	if event.Type == "response.inject" && setting.ShouldCheckPromptSensitive() {
		request := dto.OpenAIResponsesRequest{Input: injectInput}
		if sensitive, _ := service.CheckSensitiveText(request.GetTokenCountMeta().CombineText); sensitive {
			return lane, types.NewError(errors.New("user sensitive words detected"), types.ErrorCodeSensitiveWordsDetected, types.ErrOptionWithSkipRetry())
		}
	}
	var reservation responsesWSInputReservation
	if reserveInput {
		// Media loading stays on the serial client reader so the request worker
		// can keep draining its inbox and cannot stall other upstream lanes.
		estimateContext := &gin.Context{Request: s.request.Clone(s.ctx)}
		common.SetContextKey(estimateContext, constant.ContextKeyOriginalModel, modelName)
		defer service.CleanupFileSources(estimateContext)
		request := dto.OpenAIResponsesRequest{Input: injectInput}
		tokens, err := service.EstimateRequestToken(estimateContext, request.GetTokenCountMeta(), &relaycommon.RelayInfo{
			RelayMode: relayconstant.RelayModeResponses, RelayFormat: types.RelayFormatOpenAIResponses, IsStream: true,
		})
		if err != nil {
			return lane, types.NewError(err, types.ErrorCodeCountTokenFailed, types.ErrOptionWithSkipRetry())
		}
		// Reserve on the request worker before forwarding. Only an upstream
		// acknowledgement can commit the input to its estimated usage.
		reply := make(chan responsesWSInputReservation, 1)
		control := responsesWSControl{inputTokens: tokens, reserveReply: reply}
		select {
		case owner.controls <- control:
			select {
			case reservation = <-reply:
			case <-owner.done:
				// A completed worker may already have reported a failed reserve.
				select {
				case reservation = <-reply:
				default:
				}
			case <-s.ctx.Done():
				return lane, nil
			}
		case <-owner.done:
		case <-s.ctx.Done():
			return lane, nil
		}
	}
	s.stateMu.Lock()
	if s.closing {
		s.stateMu.Unlock()
		return lane, nil
	}
	if reservation.err != nil {
		s.stateMu.Unlock()
		return lane, reservation.err
	}
	// Only the client reader submits controls, so while counting, other goroutines
	// can release pending capacity but cannot consume the slot checked above.
	s.responseControls = append(s.responseControls, &responsesWSPendingControl{
		eventType: event.Type, responseID: event.ResponseID, eventID: event.EventID, streamID: lane, size: size, inputTokens: reservation.tokens,
	})
	s.responseControlBytes += size
	s.stateMu.Unlock()
	// A known response ID proves create already reached this connection. The
	// worker may finish meanwhile; the control still targets that exact ID.
	if err := s.writeTarget(websocket.TextMessage, body); err != nil {
		s.shutdown(websocket.CloseInternalServerErr, "")
	}
	return lane, nil
}

// The request worker owns both the accepted estimate and pending input. Price
// and request settings stay frozen, and rejected inputs can reuse a reservation
// until final settlement refunds the unused quota.
func (s *responsesWSSession) reserveInjectedInput(c *gin.Context, info *relaycommon.RelayInfo, state *responsesWSCallState, tokens, pendingTokens int) (int, *types.NewAPIError) {
	s.stateMu.Lock()
	finished := state.upstreamDone || s.closing
	s.stateMu.Unlock()
	if finished {
		return 0, nil
	}
	totalTokens, err := common.QuotaFromFloatStrict(float64(info.GetEstimatePromptTokens()) + float64(pendingTokens) + float64(tokens))
	if err != nil {
		return 0, types.NewErrorWithStatusCode(err, types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	quota := info.PriceData.QuotaToPreConsume
	var snapshot *billingexpr.BillingSnapshot
	if info.TieredBillingSnapshot != nil {
		copy := *info.TieredBillingSnapshot
		snapshot = &copy
		cost, trace, runErr := billingexpr.RunExprByHashWithRequest(snapshot.ExprString, snapshot.ExprHash, billingexpr.TokenParams{P: float64(totalTokens), Len: float64(totalTokens)}, *info.BillingRequestInput)
		if runErr != nil {
			return 0, types.NewErrorWithStatusCode(runErr, types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		beforeGroup := cost / 1_000_000 * snapshot.QuotaPerUnit
		if trace.BillingUnit != billingexpr.BillingUnitRequest {
			beforeGroup *= snapshot.PreConsumeMultiplier
		}
		quota, err = billingexpr.QuotaRoundStrict(beforeGroup * snapshot.GroupRatio)
		snapshot.EstimatedPromptTokens = totalTokens
		snapshot.EstimatedQuotaBeforeGroup, snapshot.EstimatedQuotaAfterGroup = beforeGroup, quota
		snapshot.EstimatedTier, snapshot.EstimatedBillingUnit, snapshot.EstimatedFixedPrice = trace.MatchedTier, trace.BillingUnit, trace.FixedPrice
	} else if !info.PriceData.UsePrice {
		quota, err = common.QuotaFromFloatStrict(float64(totalTokens) * state.reserveMultiplier * info.PriceData.ModelRatio * info.PriceData.GroupRatioInfo.GroupRatio)
	}
	if err != nil {
		return 0, types.NewErrorWithStatusCode(err, types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	// The upstream reader may mark a terminal while quota is calculated.
	// A finished response no longer needs a top-up.
	s.stateMu.Lock()
	finished = state.upstreamDone || s.closing
	s.stateMu.Unlock()
	if finished {
		return 0, nil
	}
	if !info.PriceData.FreeModel {
		if apiErr := service.PreConsumeBilling(c, quota, info); apiErr != nil {
			return 0, apiErr
		}
	}
	info.PriceData.QuotaToPreConsume = quota
	info.TieredBillingSnapshot = snapshot
	return tokens, nil
}

func (s *responsesWSSession) handleResponseControlEvent(incoming responsesWSMessage) bool {
	eventType := gjson.GetBytes(incoming.body, "type").String()
	controlType := ""
	switch eventType {
	case "response.interrupt.accepted", "response.interrupt.failed":
		controlType = "response.interrupt"
	case "response.inject.created", "response.inject.failed":
		controlType = "response.inject"
	}
	ack := controlType != ""
	if !ack && eventType != "error" {
		return false
	}
	var event responsesWSErrorEvent
	if err := common.Unmarshal(incoming.body, &event); err != nil && !ack {
		return false
	}
	if event.Error != nil && event.Error.Code == "websocket_connection_limit_reached" {
		return false
	}
	s.stateMu.Lock()
	if s.ctx.Err() != nil {
		s.stateMu.Unlock()
		return true
	}
	responseID := event.ResponseID
	pendingIndex := -1
	if ack {
		// Inject acknowledgements have no per-submission identifier. Release
		// one matching submission in wire order; keep all other controls pending.
		for i, pending := range s.responseControls {
			if pending.eventType == controlType && pending.responseID == responseID && (event.StreamID == "" || event.StreamID == pending.streamID) {
				pendingIndex = i
				break
			}
		}
	}
	if !ack {
		// Generic errors need unambiguous correlation or a control-specific
		// rejection code. Explicit generation failures and cancel errors retain
		// their ordinary handling; unresolved control errors must not end them.
		ambiguous := false
		possibleControl := false
		matchedEventID := false
		for i, candidate := range s.responseControls {
			id := candidate.responseID
			if event.ResponseID != "" && event.ResponseID != id || event.StreamID != "" && event.StreamID != candidate.streamID {
				continue
			}
			possibleControl = true
			exactEventID := event.EventID != "" && event.EventID == candidate.eventID
			if matchedEventID && !exactEventID {
				continue
			}
			matches := exactEventID
			if event.EventID == "" && event.Error != nil && event.Error.Type == "invalid_request_error" {
				switch event.Error.Code {
				case "response_not_found", "response_not_active", "response_already_completed":
					matches = true
				}
			}
			if !matches && event.ResponseID == id && (event.EventID == "" || candidate.eventID == "") {
				// A finished response cannot have another generation failure.
				// Its control can outlive the bounded completion history.
				stream := s.streams[candidate.streamID]
				matches = stream == nil || stream.current == nil || stream.current.responseID != id || stream.current.upstreamDone
			}
			if !matches {
				continue
			}
			if exactEventID && !matchedEventID {
				// An echoed event ID outranks response-only attribution for
				// unnamed controls that outlived the same generation.
				ambiguous = false
			} else if pendingIndex >= 0 {
				ambiguous = true
			}
			responseID, pendingIndex = id, i
			matchedEventID = exactEventID
		}
		if pendingIndex < 0 && (!possibleControl || event.ResponseID != "") {
			s.stateMu.Unlock()
			return false
		}
		for _, stream := range s.streams {
			state := stream.current
			if state == nil || state.upstreamDone || len(state.sentControl) == 0 || event.StreamID != "" && event.StreamID != state.streamID {
				continue
			}
			if event.ResponseID != "" && event.ResponseID != gjson.GetBytes(state.sentControl, "response_id").String() && event.ResponseID != state.responseID {
				continue
			}
			if event.EventID != "" && event.EventID != gjson.GetBytes(state.sentControl, "event_id").String() {
				continue
			}
			_, _, controlError := responsesWSErrorEndsRequest(event, state.streamID, state.responseID, state.sentControl)
			if !matchedEventID && event.EventID != "" && controlError {
				// An exact cancel event ID belongs to the ordinary control router.
				s.stateMu.Unlock()
				return false
			}
			ambiguous = ambiguous || controlError
		}
		if pendingIndex < 0 {
			// A lane or unknown server event ID cannot distinguish a late
			// control rejection from a newer generation's failure.
			ambiguous = true
		}
		if ambiguous {
			s.stateMu.Unlock()
			_ = s.writeClient(incoming.kind, incoming.body)
			s.shutdown(websocket.CloseInternalServerErr, "")
			return true
		}
	}
	if pendingIndex >= 0 {
		incoming.resolvedInputTokens = s.responseControls[pendingIndex].inputTokens
		if eventType == "response.inject.created" {
			incoming.injectedInputTokens = s.responseControls[pendingIndex].inputTokens
		}
		s.responseControlBytes -= s.responseControls[pendingIndex].size
		s.responseControls = slices.Delete(s.responseControls, pendingIndex, pendingIndex+1)
	}
	var owner *responsesWSCallState
	for _, stream := range s.streams {
		state := stream.current
		if state == nil || responseID == "" || state.responseID != responseID || event.StreamID != "" && event.StreamID != state.streamID {
			continue
		}
		if state.upstreamDone {
			incoming.injectedInputTokens = 0
			incoming.resolvedInputTokens = 0
			// The request worker publishes these after its terminal. Waiting
			// here would stop the sole upstream reader from serving other lanes.
			size := int64(len(incoming.body))
			if s.controlReplyCount >= responsesWSMaxQueued || size > s.maxQueuedBytes-s.controlReplyBytes {
				s.stateMu.Unlock()
				s.shutdown(websocket.CloseInternalServerErr, "")
				return true
			}
			state.controlReplies = append(state.controlReplies, incoming)
			s.controlReplyCount++
			s.controlReplyBytes += size
			s.stateMu.Unlock()
			return true
		}
		owner = state
		break
	}
	s.stateMu.Unlock()
	if owner != nil {
		incoming.control = true
		select {
		case owner.inbox <- incoming:
			return true
		case <-owner.done:
		case <-s.ctx.Done():
			return true
		}
	}
	if err := s.writeClient(incoming.kind, incoming.body); err != nil {
		s.shutdown(websocket.CloseInternalServerErr, "")
	}
	return true
}
