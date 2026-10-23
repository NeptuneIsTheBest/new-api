package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type responsesWSSteerSubmission struct {
	id    string
	input common.RawMessage
}

// A lane keeps accepted inputs until a successor is actually created. The
// reservation may be released while client-owned tool results are outstanding.
// All ledger changes hold stateMu; steerMu serializes client submissions with
// upstream acknowledgements and commits without blocking ordinary generation.
type responsesWSSteering struct {
	parentID        string
	submissions     []*responsesWSSteerSubmission
	reservation     *responsesWSCallState
	waitingForInput bool
}

func (p *responsesWSSteering) input() []common.RawMessage {
	var items []common.RawMessage
	for _, submission := range p.submissions {
		items = append(items, responsesWSInputItems(submission.input)...)
	}
	return items
}

type responsesWSSteerUpdate struct {
	create responsesWSCreateRequest
	reply  chan *types.NewAPIError
}

func (s *responsesWSSession) sendSteerError(body []byte, apiErr *types.NewAPIError) {
	parentID := gjson.GetBytes(body, "previous_response_id").String()
	streamID := ""
	s.stateMu.Lock()
	for lane, stream := range s.streams {
		if stream.current != nil && stream.current.responseID == parentID || stream.steering != nil && stream.steering.parentID == parentID {
			streamID = lane
			break
		}
	}
	s.stateMu.Unlock()
	openaiErr := apiErr.ToOpenAIError()
	payload, err := common.Marshal(responsesWSErrorEvent{Type: "error", Status: apiErr.StatusCode, StreamID: streamID, ResponseID: parentID, Error: &openaiErr})
	if err == nil {
		_ = s.writeClient(websocket.TextMessage, payload)
	}
}

func responsesWSInputItems(input common.RawMessage) []common.RawMessage {
	var text string
	if common.Unmarshal(input, &text) == nil && !bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		message, _ := common.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: "user", Content: text})
		return []common.RawMessage{message}
	}
	var items []common.RawMessage
	_ = common.Unmarshal(input, &items)
	return items
}

func parseResponsesWSSteer(body []byte) (string, common.RawMessage, bool) {
	var fields map[string]common.RawMessage
	if common.Unmarshal(body, &fields) != nil || len(fields) != 3 {
		return "", nil, false
	}
	for key := range fields {
		if key != "type" && key != "previous_response_id" && key != "input" {
			return "", nil, false
		}
	}
	var parentID string
	if common.Unmarshal(fields["previous_response_id"], &parentID) != nil || strings.TrimSpace(parentID) == "" {
		return "", nil, false
	}
	input := fields["input"]
	if len(bytes.TrimSpace(input)) == 0 || bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		return "", nil, false
	}
	var text string
	if common.Unmarshal(input, &text) == nil {
		return parentID, input, true
	}
	var messages []map[string]common.RawMessage
	if common.Unmarshal(input, &messages) != nil || len(messages) == 0 {
		return "", nil, false
	}
	for _, message := range messages {
		for key := range message {
			if key != "type" && key != "role" && key != "content" {
				return "", nil, false
			}
		}
		var role, messageType string
		if common.Unmarshal(message["role"], &role) != nil || role != "user" || len(message["type"]) > 0 && (common.Unmarshal(message["type"], &messageType) != nil || messageType != "message") {
			return "", nil, false
		}
		content := bytes.TrimSpace(message["content"])
		if len(content) == 0 || bytes.Equal(content, []byte("null")) {
			return "", nil, false
		}
		if common.Unmarshal(content, &text) == nil {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if common.Unmarshal(content, &parts) != nil || len(parts) == 0 {
			return "", nil, false
		}
		for _, part := range parts {
			if !slices.Contains([]string{"input_text", "input_image", "input_file"}, part.Type) {
				return "", nil, false
			}
		}
	}
	return parentID, input, true
}

// Build a logical continuation for billing. Explicit creates keep their wire
// input: accepted steering is prepended by upstream, never sent twice here.
func responsesWSContinuationRequest(base responsesWSCreateRequest, items []common.RawMessage, explicit bool) (responsesWSCreateRequest, *types.NewAPIError) {
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(base.Body, &fields); err != nil {
		return base, newResponsesWSInvalidRequestError(err)
	}
	if explicit {
		items = append(items, responsesWSInputItems(base.Request.Input)...)
	}
	input, err := common.Marshal(items)
	if err != nil {
		return base, newResponsesWSInvalidRequestError(err)
	}
	fields["input"] = input
	fields["previous_response_id"], _ = common.Marshal(base.Request.PreviousResponseID)
	body, err := common.Marshal(fields)
	if err != nil {
		return base, newResponsesWSInvalidRequestError(err)
	}
	create := responsesWSCreateRequest{Body: body, StreamID: base.StreamID, Generate: base.Generate}
	if err := common.Unmarshal(body, &create.Request); err != nil {
		return base, newResponsesWSInvalidRequestError(err)
	}
	return create, nil
}

func (s *responsesWSSession) rememberResponsesRequest(state *responsesWSCallState, create responsesWSCreateRequest, info *relaycommon.RelayInfo) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	state.create = create
	state.channelMeta = *info.ChannelMeta
	state.reserveMultiplier, _ = operation_setting.InputPreConsumeMultiplier()
}

func (s *responsesWSSession) submitSteer(body []byte) *types.NewAPIError {
	parentID, input, valid := parseResponsesWSSteer(body)
	if !valid {
		return newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSInvalidSteer)))
	}
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	s.stateMu.Lock()
	var stream *responsesWSStream
	for _, candidate := range s.streams {
		if state := candidate.current; state != nil && state.responseID == parentID && !state.upstreamDone {
			stream = candidate
			break
		}
	}
	if stream == nil || s.closing {
		s.stateMu.Unlock()
		return newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSSteerUnavailable)))
	}
	if s.steerCount >= responsesWSMaxQueued || int64(len(input)) > s.maxQueuedBytes-s.steerBytes {
		s.stateMu.Unlock()
		return types.NewErrorWithStatusCode(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSSteerLimit)), types.ErrorCodeInvalidRequest, http.StatusTooManyRequests, types.ErrOptionWithSkipRetry())
	}
	parent := stream.current
	pending := stream.steering
	if pending != nil && pending.parentID != parentID {
		s.stateMu.Unlock()
		return newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSSteerUnavailable)))
	}
	if pending == nil {
		pending = &responsesWSSteering{parentID: parentID}
		stream.steering = pending
	}
	submission := &responsesWSSteerSubmission{input: input}
	pending.submissions = append(pending.submissions, submission)
	s.steerCount++
	s.steerBytes += int64(len(input))
	base := parent.create
	base.Request.PreviousResponseID = parentID
	create, apiErr := responsesWSContinuationRequest(base, pending.input(), false)
	reservation := pending.reservation
	first := reservation == nil
	var requestID string
	var message []byte
	if apiErr == nil && first {
		var err error
		message, err = buildResponsesWSCreateEvent(create.Body, create.Generate, create.StreamID)
		if err != nil {
			apiErr = newResponsesWSInvalidRequestError(err)
		}
	}
	if apiErr == nil && first {
		reservation = &responsesWSCallState{streamID: parent.streamID, previousResponseID: parentID,
			automatic: true, parentID: parentID, parentDone: parent.done, create: create, channelMeta: parent.channelMeta,
			reserveMultiplier: parent.reserveMultiplier, inbox: make(chan responsesWSMessage, 1),
			controls: make(chan responsesWSControl, 1), done: make(chan struct{}), ready: make(chan *types.NewAPIError, 1),
			updates: make(chan responsesWSSteerUpdate), abort: make(chan struct{})}
		pending.reservation = reservation
		requestID = fmt.Sprintf("%s-ws-%d", s.requestID, s.nextEventIndex)
		s.nextEventIndex++
	}
	s.stateMu.Unlock()
	if apiErr == nil {
		if first {
			s.workers.Go(func() { s.runRequest(reservation, message, requestID) })
			select {
			case apiErr = <-reservation.ready:
			case <-s.ctx.Done():
				return types.NewError(s.ctx.Err(), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			}
		} else {
			apiErr = s.updateSteerReservation(reservation, create)
		}
	}
	if apiErr != nil {
		s.stateMu.Lock()
		s.removeSteerLocked(pending, submission)
		if len(pending.submissions) == 0 {
			stream.steering = nil
		}
		s.stateMu.Unlock()
		if first && reservation != nil {
			<-reservation.done
		}
		return apiErr
	}
	if err := s.writeTarget(websocket.TextMessage, body); err != nil {
		s.shutdown(websocket.CloseInternalServerErr, "")
		return types.NewError(err, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
	}
	return nil
}

func (s *responsesWSSession) updateSteerReservation(state *responsesWSCallState, create responsesWSCreateRequest) *types.NewAPIError {
	update := responsesWSSteerUpdate{create: create, reply: make(chan *types.NewAPIError, 1)}
	select {
	case state.updates <- update:
		select {
		case apiErr := <-update.reply:
			return apiErr
		case <-state.done:
		case <-s.ctx.Done():
		}
	case <-state.done:
	case <-s.ctx.Done():
	}
	return types.NewError(context.Canceled, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
}

// Clients can return tool results immediately after the parent's terminal;
// upstream need not emit steer.pending first. Release the speculative slot so
// the explicit create can reach upstream and consume the queued steering.
func (s *responsesWSSession) resumeSteeringCreate(streamID string, body []byte) {
	source := gjson.ParseBytes(body)
	if wrapped := source.Get("response"); wrapped.IsObject() {
		source = wrapped
	}
	parentID := source.Get("previous_response_id").String()
	if parentID == "" {
		return
	}
	toolResult := false
	for _, input := range source.Get("input").Array() {
		if slices.Contains([]string{"function_call_output", "custom_tool_call_output", "computer_call_output", "local_shell_call_output", "shell_call_output", "apply_patch_call_output", "mcp_approval_response"}, input.Get("type").String()) {
			toolResult = true
			break
		}
	}
	if !toolResult {
		return
	}
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	s.stateMu.Lock()
	stream := s.streams[streamID]
	var stop *responsesWSCallState
	if stream != nil && stream.steering != nil && stream.steering.parentID == parentID {
		pending := stream.steering
		if state := stream.current; state != nil && (state.upstreamDone || state == pending.reservation) {
			pending.waitingForInput = true
			stop = pending.reservation
		}
	}
	s.stateMu.Unlock()
	if stop != nil {
		stop.abortOnce.Do(func() { close(stop.abort) })
		<-stop.done
	}
}

// Re-estimate queued input with the existing price snapshot. Changing queued
// text must neither reload administrator prices nor replace its funding source.
func updateResponsesWSSteerReservation(c *gin.Context, info *relaycommon.RelayInfo, state *responsesWSCallState, create responsesWSCreateRequest) *types.NewAPIError {
	meta := &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer}
	if constant.CountToken || setting.ShouldCheckPromptSensitive() {
		meta = create.Request.GetTokenCountMeta()
	}
	if setting.ShouldCheckPromptSensitive() {
		if sensitive, _ := service.CheckSensitiveText(meta.CombineText); sensitive {
			return types.NewError(errors.New("user sensitive words detected"), types.ErrorCodeSensitiveWordsDetected)
		}
	}
	tokens, err := service.EstimateRequestToken(c, meta, info)
	if err != nil {
		return types.NewError(err, types.ErrorCodeCountTokenFailed)
	}
	request := *info.BillingRequestInput
	request.Body = create.Body
	quota := info.PriceData.QuotaToPreConsume
	var snapshot *billingexpr.BillingSnapshot
	if info.TieredBillingSnapshot != nil {
		copy := *info.TieredBillingSnapshot
		snapshot = &copy
		cost, trace, runErr := billingexpr.RunExprByHashWithRequest(snapshot.ExprString, snapshot.ExprHash, billingexpr.TokenParams{P: float64(tokens), Len: float64(tokens)}, request)
		if runErr != nil {
			return types.NewError(runErr, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		}
		beforeGroup := cost / 1_000_000 * snapshot.QuotaPerUnit
		if trace.BillingUnit != billingexpr.BillingUnitRequest {
			beforeGroup *= snapshot.PreConsumeMultiplier
		}
		quota, err = billingexpr.QuotaRoundStrict(beforeGroup * snapshot.GroupRatio)
		snapshot.EstimatedPromptTokens = tokens
		snapshot.EstimatedQuotaBeforeGroup, snapshot.EstimatedQuotaAfterGroup = beforeGroup, quota
		snapshot.EstimatedTier, snapshot.EstimatedBillingUnit, snapshot.EstimatedFixedPrice = trace.MatchedTier, trace.BillingUnit, trace.FixedPrice
	} else if !info.PriceData.UsePrice {
		quota, err = common.QuotaFromFloatStrict(float64(tokens) * state.reserveMultiplier * info.PriceData.ModelRatio * info.PriceData.GroupRatioInfo.GroupRatio)
	}
	if err != nil {
		return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
	}
	if !info.PriceData.FreeModel {
		if apiErr := service.PreConsumeBilling(c, quota, info); apiErr != nil {
			return apiErr
		}
	}
	info.Request, info.BillingRequestInput, info.TieredBillingSnapshot = &create.Request, &request, snapshot
	info.SetEstimatePromptTokens(tokens)
	info.PriceData.QuotaToPreConsume = quota
	state.create = create
	return nil
}

func (s *responsesWSSession) removeSteerLocked(pending *responsesWSSteering, submission *responsesWSSteerSubmission) {
	index := slices.Index(pending.submissions, submission)
	if index < 0 {
		return
	}
	pending.submissions = slices.Delete(pending.submissions, index, index+1)
	s.steerCount--
	s.steerBytes -= int64(len(submission.input))
}

func (s *responsesWSSession) handleSteeringEvent(incoming responsesWSMessage) bool {
	eventType := gjson.GetBytes(incoming.body, "type").String()
	if !strings.HasPrefix(eventType, "response.steer.") {
		return false
	}
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	parentID := gjson.GetBytes(incoming.body, "steer.previous_response_id").String()
	id := gjson.GetBytes(incoming.body, "steer.id").String()
	s.stateMu.Lock()
	var stream *responsesWSStream
	for _, candidate := range s.streams {
		if candidate.steering != nil && candidate.steering.parentID == parentID {
			stream = candidate
			break
		}
	}
	var stop *responsesWSCallState
	var parentDone <-chan struct{}
	var update *responsesWSCallState
	var create responsesWSCreateRequest
	if stream != nil {
		pending := stream.steering
		if current := stream.current; current != nil && current.responseID == parentID && current.upstreamDone {
			parentDone = current.done
		}
		var submission *responsesWSSteerSubmission
		for _, candidate := range pending.submissions {
			if id != "" && candidate.id == id || candidate.id == "" && (eventType == "response.steer.accepted" || id == "" && eventType == "response.steer.failed") {
				submission = candidate
				break
			}
		}
		switch eventType {
		case "response.steer.accepted":
			if submission != nil {
				submission.id = id
			}
		case "response.steer.pending":
			pending.waitingForInput = true
			stop = pending.reservation
		case "response.steer.failed":
			if submission != nil {
				s.removeSteerLocked(pending, submission)
				if len(pending.submissions) == 0 {
					stop = pending.reservation
					stream.steering = nil
				} else if pending.reservation != nil {
					update = pending.reservation
					create, _ = responsesWSContinuationRequest(update.create, pending.input(), false)
				}
			}
		}
	}
	s.stateMu.Unlock()
	if update != nil {
		if apiErr := s.updateSteerReservation(update, create); apiErr != nil {
			s.shutdown(websocket.CloseInternalServerErr, "continuation reservation failed")
		}
	}
	if stop != nil {
		stop.abortOnce.Do(func() { close(stop.abort) })
		<-stop.done
	}
	if parentDone != nil {
		select {
		case <-parentDone:
		case <-s.ctx.Done():
			return true
		}
	}
	if err := s.writeClient(incoming.kind, incoming.body); err != nil {
		s.shutdown(websocket.CloseInternalServerErr, "")
	}
	return true
}

// response.created is the commit point. Wait for the parent to finish its
// middleware and terminal write, then transfer the already reserved lane.
func (s *responsesWSSession) admitAutomaticResponse(body []byte) bool {
	if gjson.GetBytes(body, "type").String() != "response.created" {
		return true
	}
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	parentID := gjson.GetBytes(body, "response.previous_response_id").String()
	streamID := gjson.GetBytes(body, "stream_id").String()
	responseID := gjson.GetBytes(body, "response.id").String()
	if responseID == "" {
		return false
	}
	s.stateMu.Lock()
	if s.completedResponses[responseID] {
		s.stateMu.Unlock()
		return true
	}
	var stream *responsesWSStream
	for lane, candidate := range s.streams {
		if pending := candidate.steering; pending != nil && pending.parentID == parentID && (streamID == "" || lane == streamID) {
			stream = candidate
			break
		}
	}
	if stream == nil {
		admitted := false
		for lane, candidate := range s.streams {
			state := candidate.current
			if state != nil && state.submitted && (streamID == "" || lane == streamID) &&
				(state.responseID == responseID || !state.automatic && state.responseID == "") {
				admitted = true
				break
			}
		}
		s.stateMu.Unlock()
		return admitted
	}
	pending := stream.steering
	reservation := pending.reservation
	if reservation == nil {
		// An explicit tool-result continuation owns its ordinary request.
		if stream.current == nil || !stream.current.submitted || stream.current.create.Request.PreviousResponseID != parentID {
			s.stateMu.Unlock()
			return false
		}
	} else {
		s.stateMu.Unlock()
		select {
		case <-reservation.parentDone:
		case <-s.ctx.Done():
			return false
		}
		s.stateMu.Lock()
		if stream.current != reservation {
			s.stateMu.Unlock()
			return false
		}
		reservation.submitted = true
		reservation.responseID = responseID
	}
	for len(pending.submissions) > 0 {
		s.removeSteerLocked(pending, pending.submissions[0])
	}
	stream.steering = nil
	s.stateMu.Unlock()
	return true
}
