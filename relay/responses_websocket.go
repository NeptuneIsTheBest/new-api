package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/QuantumNous/new-api/common"
	appconstant "github.com/QuantumNous/new-api/constant"
	appdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	appmodel "github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const responsesWSEventTypeResponseCreate = "response.create"
const responsesWSWriteTimeout = 30 * time.Second

const (
	responsesWSMaxActive  = 16
	responsesWSMaxStreams = 32
	responsesWSMaxQueued  = 128
)

var errResponsesWSInvalidStreamID = errors.New("stream_id must contain 1-256 ASCII letters, digits, underscores, hyphens, or periods")

// ResponsesWSRequestRunner executes the existing authentication and rate-limit
// middleware around one complete request, without a second HTTP connection.
type ResponsesWSRequestRunner func(*http.Request, string, func(*gin.Context) *types.NewAPIError) *types.NewAPIError

type responsesWSCreateEvent struct {
	Type       string            `json:"type"`
	EventID    string            `json:"event_id,omitempty"`
	ResponseID string            `json:"response_id,omitempty"`
	StreamID   common.RawMessage `json:"stream_id,omitempty"`
	Generate   common.RawMessage `json:"generate,omitempty"`
	Request    common.RawMessage `json:"response,omitempty"`
}

type responsesWSCreateRequest struct {
	Request  dto.OpenAIResponsesRequest
	Body     []byte
	Generate common.RawMessage
	StreamID string
}

type responsesWSErrorEvent struct {
	Type       string             `json:"type"`
	Status     int                `json:"status"`
	EventID    string             `json:"event_id,omitempty"`
	StreamID   string             `json:"stream_id,omitempty"`
	ResponseID string             `json:"response_id,omitempty"`
	Error      *types.OpenAIError `json:"error"`
}

type responsesWSMessage struct {
	kind int
	body []byte
	err  error
}

// responsesWSControl is a client control event (response.cancel) whose
// envelope the read loop already parsed.
type responsesWSControl struct {
	body              []byte
	eventID, streamID string
}

// Only the request worker reads or changes billing state. Socket readers pass
// bounded messages to it; cancellation never performs an independent refund.
type responsesWSCallState struct {
	inbox    chan responsesWSMessage
	controls chan responsesWSControl
	done     chan struct{}
	terminal *responsesWSMessage
	// A nonzero closeCode ends the connection after this request.
	closeCode          int
	closeReason        string
	upstreamRejected   bool
	streamID           string
	previousResponseID string
	// Routing metadata is protected by the session's stateMu. Billing remains
	// exclusively owned by this request's worker.
	responseID   string
	sentControl  []byte
	submitted    bool
	upstreamDone bool
	create       responsesWSCreateRequest
	channelMeta  relaycommon.ChannelMeta
	// Automatic continuations prepare a separate request before the steer is
	// sent, but own no generation until their response.created arrives.
	automatic           bool
	parentID            string
	parentDone          <-chan struct{}
	ready               chan *types.NewAPIError
	preparationReported bool
	updates             chan responsesWSSteerUpdate
	abort               chan struct{}
	abortOnce           sync.Once
	started             bool
	reserveMultiplier   float64
}

type responsesWSPendingRequest struct {
	body  []byte
	index int
}

type responsesWSStream struct {
	current        *responsesWSCallState
	queue          []responsesWSPendingRequest
	lastResponseID string
	steering       *responsesWSSteering
}

type responsesWSSession struct {
	ctx            context.Context
	cancel         context.CancelFunc
	client         *websocket.Conn
	runner         ResponsesWSRequestRunner
	request        *http.Request
	requestID      string
	nextEventIndex int
	workers        sync.WaitGroup
	closeOnce      sync.Once

	clientWriteMu      sync.Mutex
	targetWriteMu      sync.Mutex
	connectionMu       sync.Mutex
	target             *websocket.Conn
	unregister         func()
	stateMu            sync.Mutex
	steerMu            sync.Mutex
	streams            map[string]*responsesWSStream
	namedStreams       int
	active             int
	queued             int
	queuedBytes        int64
	maxQueuedBytes     int64
	changed            chan struct{}
	connected          bool
	closing            bool
	upstreamErr        error
	lang               string
	steerCount         int
	steerBytes         int64
	completedResponses map[string]bool
	completedOrder     []string

	// The first worker publishes these immutable connection bindings before
	// connected becomes true and other request workers can start.
	lockedModel     string
	lockedChannelID int
	lockedGroup     string
	lockedKey       string
	lockedKeyIndex  int
	lockedContext   map[appconstant.ContextKey]any
	lockedRoute     dto.AdvancedCustomRoute
}

func ResponsesWebSocketHelper(c *gin.Context, client *websocket.Conn, runner ResponsesWSRequestRunner) *types.NewAPIError {
	ctx, cancel := context.WithCancel(c.Request.Context())
	s := &responsesWSSession{ctx: ctx, cancel: cancel, client: client, runner: runner,
		request: c.Request.Clone(ctx), requestID: c.GetString(common.RequestIdKey),
		streams: make(map[string]*responsesWSStream), changed: make(chan struct{}), lang: i18n.StatedLang(c)}
	if s.requestID == "" {
		s.requestID = common.NewRequestId()
	}
	maxMB := appconstant.MaxRequestBodyMB
	if maxMB <= 0 {
		maxMB = 128
	}
	client.SetReadLimit(int64(maxMB) << 20)
	s.maxQueuedBytes = int64(maxMB) << 20
	defer func() {
		// Unless an earlier shutdown ended the read, the client closed or left.
		// Gorilla already answered a client close frame, so this tells the upstream.
		s.shutdown(websocket.CloseNormalClosure, "")
		s.workers.Wait()
	}()

	for {
		_, message, err := client.ReadMessage()
		if err != nil {
			return nil
		}
		envelope, streamID, err := parseResponsesWSEnvelope(message)
		eventType := envelope.Type
		if err != nil {
			apiErr := newResponsesWSInvalidRequestError(err)
			if errors.Is(err, errResponsesWSInvalidStreamID) {
				apiErr = types.WithOpenAIError(types.OpenAIError{Type: "invalid_request_error", Code: "invalid_stream_id", Param: "stream_id", Message: i18n.Translate(s.lang, i18n.MsgResponsesWSInvalidStreamID)}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			s.sendError(envelope.EventID, streamID, apiErr)
			continue
		}
		if eventType != responsesWSEventTypeResponseCreate {
			if eventType == "response.steer" {
				if apiErr := s.submitSteer(message); apiErr != nil {
					s.sendSteerError(message, apiErr)
				}
				continue
			}
			// Controls are owned by the active request too. In particular a cancel
			// arriving during authentication must not precede its upstream create.
			state := s.controlOwner(streamID, envelope.ResponseID)
			if eventType != "response.cancel" || state == nil {
				s.sendError(envelope.EventID, streamID, newResponsesWSInvalidRequestError(fmt.Errorf("unsupported websocket event %q", eventType)))
				continue
			}
			select {
			case state.controls <- responsesWSControl{body: message, eventID: envelope.EventID, streamID: state.streamID}:
			case <-state.done:
			case <-s.ctx.Done():
				return nil
			default:
				s.sendError(envelope.EventID, streamID, newResponsesWSInvalidRequestError(errors.New("a response control event is already pending")))
			}
			continue
		}
		s.resumeSteeringCreate(streamID, message)
		if apiErr := s.enqueue(streamID, message); apiErr != nil {
			s.sendError(envelope.EventID, streamID, apiErr)
		}
		if s.ctx.Err() != nil {
			return nil
		}
	}
}

func (s *responsesWSSession) runRequest(state *responsesWSCallState, message []byte, requestID string) {
	var envelope responsesWSCreateEvent
	streamID := state.streamID
	var apiErr *types.NewAPIError
	var upstreamURLs []string
	defer func() {
		if recovered := recover(); recovered != nil {
			apiErr = types.NewError(fmt.Errorf("responses websocket request panic: %v", recovered), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			state.closeCode = websocket.CloseInternalServerErr
		}
		// Finish settlement and middleware before publishing the terminal. Keep
		// this lane occupied until the write finishes, without holding stateMu
		// across network I/O or blocking admission on other lanes.
		if state.automatic && !state.preparationReported {
			state.ready <- apiErr
		}
		outgoing := state.terminal
		if apiErr != nil && (!state.automatic || state.started) {
			if body, err := buildResponsesWSErrorPayload(envelope.EventID, streamID, apiErr, upstreamURLs...); err == nil {
				outgoing = &responsesWSMessage{kind: websocket.TextMessage, body: body}
			}
		}
		if outgoing != nil {
			if err := s.writeClient(outgoing.kind, outgoing.body); err != nil {
				state.closeCode = websocket.CloseInternalServerErr
			}
		}
		if state.closeCode != 0 {
			s.shutdown(state.closeCode, state.closeReason)
		}
		s.stateMu.Lock()
		stream := s.streams[state.streamID]
		if state.responseID != "" && state.started {
			stream.lastResponseID = state.responseID
			if s.completedResponses == nil {
				s.completedResponses = make(map[string]bool)
			}
			if !s.completedResponses[state.responseID] {
				s.completedResponses[state.responseID] = true
				s.completedOrder = append(s.completedOrder, state.responseID)
				if len(s.completedOrder) > responsesWSMaxQueued {
					delete(s.completedResponses, s.completedOrder[0])
					s.completedOrder = s.completedOrder[1:]
				}
			}
		}
		if stream.current == state {
			stream.current = nil
			s.active--
			if pending := stream.steering; pending != nil && pending.reservation != nil && pending.reservation != state {
				stream.current = pending.reservation
				s.active++
			}
		}
		if pending := stream.steering; pending != nil && pending.reservation == state {
			pending.reservation = nil
		}
		close(state.done)
		s.scheduleLocked()
		s.stateMu.Unlock()
	}()
	if s.ctx.Err() != nil {
		return
	}
	envelope, _, _ = parseResponsesWSEnvelope(message)
	create, parseErr := normalizeResponsesWSCreateEvent(message, envelope, streamID)
	request := s.request.Clone(s.ctx)
	request.Body = io.NopCloser(bytes.NewReader(message))
	request.ContentLength = int64(len(message))
	request.Header.Set("Content-Type", "application/json")
	apiErr = s.runner(request, requestID, func(c *gin.Context) *types.NewAPIError {
		if parseErr != nil {
			return newResponsesWSInvalidRequestError(parseErr)
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(create.Body))
		c.Request.ContentLength = int64(len(create.Body))
		callErr := s.runCall(c, state, create)
		if state.automatic && !state.started {
			// A speculative reservation is not a successful model request.
			c.Status(http.StatusBadRequest)
		}
		upstreamURLs = common.UpstreamAddresses(c)
		return callErr
	})
	if apiErr != nil && !state.automatic && !state.upstreamRejected && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		state.closeCode = websocket.ClosePolicyViolation
	}
}

func (s *responsesWSSession) runCall(c *gin.Context, state *responsesWSCallState, create responsesWSCreateRequest) (apiErr *types.NewAPIError) {
	policy := service.RequestPolicy(c)
	modelName := create.Request.Model
	started := time.Now()
	var info *relaycommon.RelayInfo
	var accumulator *service.ResponsesUsageAccumulator
	billingPrepared := false
	defer func() {
		if recovered := recover(); recovered != nil {
			apiErr = types.NewError(fmt.Errorf("responses websocket call panic: %v", recovered), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			state.closeCode = websocket.CloseInternalServerErr
		}
		if info == nil && modelName != "" {
			info = &relaycommon.RelayInfo{OriginModelName: modelName, UsingGroup: common.GetContextKeyString(c, appconstant.ContextKeyUsingGroup), StartTime: started}
		}
		if state.automatic && !state.started && accumulator != nil {
			s.stateMu.Lock()
			committed := state.responseID != ""
			s.stateMu.Unlock()
			if committed {
				// A disconnect can win the inbox select after the reader commits
				// response.created. The reservation already owns that generation.
				state.started = true
				accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.created"}, nil)
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, s.ctx.Err())
				ConsumeResponsesQuota(c, info, accumulator.Finish())
			}
		}
		if state.automatic && !state.started {
			if info != nil && info.Billing != nil {
				if billing, ok := info.Billing.(*service.BillingSession); ok {
					if err := billing.RefundAndWait(c); err != nil {
						apiErr = types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
						state.closeCode = websocket.CloseInternalServerErr
					}
				}
			}
			return
		}
		perfmetrics.RecordRelayResult(c.Request.Context(), info, apiErr)
		// Settlement already marks the request policy successful, and nothing
		// reads a termination decision after this point on the WebSocket path,
		// so neither policy record belongs here.
		if info != nil && billingPrepared {
			apiErr = RefundFailedRequestBilling(c, info, apiErr)
			if apiErr != nil {
				// A queued request must see the released reservation when its
				// authentication and quota checks run after this lane finishes.
				if billing, ok := info.Billing.(*service.BillingSession); ok {
					if err := billing.RefundAndWait(c); err != nil {
						state.closeCode = websocket.CloseInternalServerErr
					}
				}
			}
		}
	}()
	if s.ctx.Err() != nil {
		return types.NewError(s.ctx.Err(), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
	}
	if modelName == "" {
		return newResponsesWSInvalidRequestError(errors.New("model is required"))
	}
	if s.lockedModel != "" && modelName != s.lockedModel {
		return newResponsesWSInvalidRequestError(fmt.Errorf("responses websocket connection is locked to model %q", s.lockedModel))
	}
	if apiErr = checkResponsesWSModelAccess(c, modelName); apiErr != nil {
		return apiErr
	}
	common.SetContextKey(c, appconstant.ContextKeyOriginalModel, modelName)
	common.SetContextKey(c, appconstant.ContextKeyRequestStartTime, time.Now())
	service.GetChannelConstraints(c).AddFilter(appdto.ChannelFilter{Kind: appdto.FilterRequestPath, RequestPath: c.Request.URL.Path})
	billingCreate := create
	if !state.automatic {
		s.stateMu.Lock()
		pending := s.streams[state.streamID].steering
		if pending != nil && pending.waitingForInput && pending.parentID != create.Request.PreviousResponseID {
			s.stateMu.Unlock()
			return newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSSteerContinuationRequired)))
		}
		if pending != nil && pending.parentID == create.Request.PreviousResponseID && pending.waitingForInput {
			billingCreate, apiErr = responsesWSContinuationRequest(create, pending.input(), true)
		}
		s.stateMu.Unlock()
		if apiErr != nil {
			return apiErr
		}
	}

	if s.lockedChannelID != 0 {
		if apiErr = s.restoreConnectionContext(c, modelName); apiErr != nil {
			return apiErr
		}
		info = relaycommon.GenRelayInfoResponses(c, &billingCreate.Request)
		info.BillingRequestInput = &billingexpr.RequestInput{Body: billingCreate.Body}
		info.IsStream = true
		common.SetContextKey(c, appconstant.ContextKeyIsStream, true)
		if apiErr = PrepareRequestBilling(c, info); apiErr != nil {
			return apiErr
		}
		billingPrepared = true
		if state.automatic {
			meta := state.channelMeta
			info.ChannelMeta = &meta
			adaptor := GetAdaptor(info.ApiType)
			adaptor.Init(info)
		} else {
			var payload []byte
			payload, apiErr = buildResponsesWSCreatePayload(c, info, create.Request, create.Generate, create.StreamID)
			if apiErr != nil {
				return apiErr
			}
			s.rememberResponsesRequest(state, create, info)
			if err := s.writeCreate(state, payload); err != nil {
				state.closeCode = websocket.CloseInternalServerErr
				return types.NewError(err, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			}
		}
	} else {
		retry := &service.RetryParam{Ctx: c, TokenGroup: common.GetContextKeyString(c, appconstant.ContextKeyUsingGroup), ModelName: modelName, RequestPath: c.Request.URL.Path, Retry: common.GetPointer(0)}
		for ; retry.GetRetry() <= common.RetryTimes; retry.IncreaseRetry() {
			var channel *appmodel.Channel
			channel, apiErr = selectResponsesWSChannel(c, modelName, retry)
			if apiErr != nil {
				return apiErr
			}
			service.AppendUsedChannel(c, channel.Id)
			if info == nil {
				info = relaycommon.GenRelayInfoResponses(c, &billingCreate.Request)
				info.BillingRequestInput = &billingexpr.RequestInput{Body: billingCreate.Body}
				info.IsStream = true
				common.SetContextKey(c, appconstant.ContextKeyIsStream, true)
				if apiErr = PrepareRequestBilling(c, info); apiErr != nil {
					return apiErr
				}
				billingPrepared = true
			} else {
				info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)
				if apiErr = service.PrepareTieredBillingForSelectedGroup(c, info); apiErr != nil {
					return apiErr
				}
			}
			info.RetryIndex = retry.GetRetry()
			policy.BeginAttempt(channel, info.UsingGroup)
			var payload []byte
			payload, apiErr = buildResponsesWSCreatePayload(c, info, create.Request, create.Generate, create.StreamID)
			if apiErr != nil {
				return apiErr
			}
			adaptor := GetAdaptor(info.ApiType)
			adaptor.Init(info)
			target, dialErr := relaychannel.DoWssRequest(adaptor, c, info, nil)
			if dialErr != nil {
				apiErr = service.NormalizeViolationFeeError(types.NewError(dialErr, types.ErrorCodeDoRequestFailed))
				service.ResetStatusCode(apiErr, c.GetString("status_code_mapping"))
				info.LastError = apiErr
				decision := service.DecideRelayRetry(c, apiErr, common.RetryTimes-retry.GetRetry())
				service.RecordPolicyFailure(c, channel.Id, apiErr, decision)
				service.ProcessChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, info.ApiKey, channel.GetAutoBan()), apiErr, info)
				if decision.Action == "retry" {
					continue
				}
				return apiErr
			}
			if !s.setTarget(target) {
				return types.NewError(context.Canceled, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			}
			s.rememberResponsesRequest(state, create, info)
			if err := s.writeCreate(state, payload); err != nil {
				state.closeCode = websocket.CloseInternalServerErr
				return types.NewError(err, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			}
			s.lockedModel, s.lockedChannelID, s.lockedGroup = modelName, channel.Id, info.UsingGroup
			s.lockedKey, s.lockedKeyIndex = info.ApiKey, info.ChannelMultiKeyIndex
			s.lockedRoute, _ = channel.GetOtherSettings().AdvancedCustom.MatchPathForModel(c.Request.URL.Path, modelName)
			s.lockedContext = make(map[appconstant.ContextKey]any)
			for _, key := range []appconstant.ContextKey{
				appconstant.ContextKeyChannelId, appconstant.ContextKeyChannelName, appconstant.ContextKeyChannelType,
				appconstant.ContextKeyChannelCreateTime, appconstant.ContextKeyChannelSetting, appconstant.ContextKeyChannelOtherSetting,
				appconstant.ContextKeyChannelParamOverride, appconstant.ContextKeyChannelHeaderOverride, appconstant.ContextKeyChannelOrganization,
				appconstant.ContextKeyChannelAutoBan, appconstant.ContextKeyChannelModelMapping, appconstant.ContextKeyChannelStatusCodeMapping,
				appconstant.ContextKeyChannelIsMultiKey, appconstant.ContextKeyChannelMultiKeyIndex, appconstant.ContextKeyChannelKey, appconstant.ContextKeyChannelBaseUrl,
				appconstant.ContextKeyUpstreamRequestOrigin,
			} {
				if value, ok := c.Get(string(key)); ok {
					s.lockedContext[key] = value
				}
			}
			s.registerChannelClose(channel.Id)
			s.stateMu.Lock()
			s.connected = true
			s.startTargetReader(target)
			s.scheduleLocked()
			s.stateMu.Unlock()
			apiErr = nil
			break
		}
		if apiErr != nil {
			return apiErr
		}
	}

	accumulator = service.NewResponsesUsageAccumulator(info)
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RequireTerminal()
	common.SetContextKey(c, appconstant.ContextKeyResponseStreamStatus, info.StreamStatus)
	if state.automatic {
		state.preparationReported = true
		state.ready <- nil
	}
	timeout := time.Duration(appconstant.StreamingTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	idle := time.NewTimer(timeout)
	defer idle.Stop()
	accepted := false
	var pendingControl []byte
	var sentControl []byte
	var responseID string
	for {
		var incoming responsesWSMessage
		select {
		case incoming = <-state.inbox:
		case <-state.abort:
			return nil
		case update := <-state.updates:
			update.reply <- updateResponsesWSSteerReservation(c, info, state, update.create)
			continue
		case control := <-state.controls:
			if pendingControl != nil || sentControl != nil {
				s.sendError(control.eventID, control.streamID, newResponsesWSInvalidRequestError(errors.New("a response control event is already pending")))
				continue
			}
			if !accepted {
				pendingControl = control.body
				continue
			}
			if err := s.writeControl(state, control.body); err != nil {
				s.shutdown(websocket.CloseInternalServerErr, "")
			}
			sentControl = control.body
			continue
		case <-idle.C:
			if state.automatic && !state.started {
				state.closeCode, state.closeReason = websocket.CloseInternalServerErr, "upstream continuation timed out"
				return nil
			}
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, context.DeadlineExceeded)
			state.closeCode, state.closeReason = websocket.CloseInternalServerErr, "upstream response timed out"
			ConsumeResponsesQuota(c, info, accumulator.Finish())
			return nil
		case <-s.ctx.Done():
			select {
			case incoming = <-state.inbox:
			default:
				if state.automatic && !state.started {
					return nil
				}
				s.stateMu.Lock()
				upstreamErr := s.upstreamErr
				s.stateMu.Unlock()
				if upstreamErr != nil {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, upstreamErr)
				} else {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, s.ctx.Err())
				}
				ConsumeResponsesQuota(c, info, accumulator.Finish())
				return nil
			}
		}
		idle.Reset(timeout)
		if incoming.err != nil {
			if state.automatic && !state.started {
				return nil
			}
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, incoming.err)
			// The reader closes the connection after every lane has consumed
			// its preceding frames and finished settlement.
			ConsumeResponsesQuota(c, info, accumulator.Finish())
			return nil
		}
		info.SetFirstResponseTime()
		var event struct {
			dto.ResponsesStreamResponse
			StreamID string `json:"stream_id"`
		}
		if err := common.Unmarshal(incoming.body, &event); err != nil {
			info.StreamStatus.RecordError("invalid upstream websocket event")
		} else {
			if event.Type != "error" && event.StreamID != "" && event.StreamID != create.StreamID {
				if err := s.writeClient(incoming.kind, incoming.body); err != nil {
					s.shutdown(websocket.CloseInternalServerErr, "")
				}
				continue
			}
			// A repeated terminal from the previous response must never finish
			// a subsequent request on this persistent connection.
			if event.Response != nil && event.Response.ID != "" && event.Response.ID == state.previousResponseID {
				continue
			}
			if event.Type == "error" {
				var rejection responsesWSErrorEvent
				_ = common.Unmarshal(incoming.body, &rejection)
				if rejection.ResponseID != "" && rejection.ResponseID == state.previousResponseID {
					continue
				}
				terminal, ambiguous, controlError := responsesWSErrorEndsRequest(rejection, create.StreamID, responseID, sentControl)
				if !terminal {
					if err := s.writeClient(incoming.kind, incoming.body); err != nil {
						s.shutdown(websocket.CloseInternalServerErr, "")
					}
					// Only a control error in this stream resolves its pending control.
					if controlError {
						sentControl = nil
						s.stateMu.Lock()
						state.sentControl = nil
						s.stateMu.Unlock()
					}
					continue
				}
				if accepted {
					if rejection.Error != nil {
						code := ""
						if rejection.Error.Code != nil {
							code = fmt.Sprint(rejection.Error.Code)
						}
						info.StreamStatus.MarkFailed(code, rejection.Error.Type, rejection.Status)
					}
					accumulator.Observe(&event.ResponsesStreamResponse, incoming.body)
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
					state.terminal = &incoming
					if ambiguous {
						state.closeCode = websocket.CloseInternalServerErr
					}
					ConsumeResponsesQuota(c, info, accumulator.Finish())
					return nil
				}
				if rejection.Error == nil {
					// An error frame without an error object still describes the
					// rejected request; keep the client-facing type stable.
					rejection.Error = &types.OpenAIError{Type: "invalid_request_error", Message: event.Message, Code: event.Code}
				}
				rejected := types.WithOpenAIError(*rejection.Error, rejection.Status, types.ErrOptionWithSkipRetry())
				if rejected.StatusCode < 400 || rejected.StatusCode > 599 {
					rejected.StatusCode = http.StatusBadRequest
				}
				state.upstreamRejected = true
				return rejected
			}
			if strings.HasPrefix(event.Type, "response.") {
				state.started = true
				if !accepted {
					// Like HTTP, bind the session only once upstream accepted the request.
					service.RecordChannelAffinity(c, s.lockedChannelID)
				}
				accepted = true
				if event.Response != nil && event.Response.ID != "" {
					responseID = event.Response.ID
				}
			}
			accumulator.Observe(&event.ResponsesStreamResponse, incoming.body)
		}
		switch event.Type {
		case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			state.terminal = &incoming
			ConsumeResponsesQuota(c, info, accumulator.Finish())
			return nil
		}
		if err := s.writeClient(incoming.kind, incoming.body); err != nil {
			s.shutdown(websocket.CloseInternalServerErr, "")
		}
		if accepted && pendingControl != nil {
			if err := s.writeControl(state, pendingControl); err != nil {
				s.shutdown(websocket.CloseInternalServerErr, "")
			}
			sentControl = pendingControl
			pendingControl = nil
		}
	}
}

// Each lane has one active generation, but a cancel can fail independently. Correlate
// available IDs first; uncorrelated control/server errors require a bounded
// connection close because continuing could leave the generation stuck forever.
func responsesWSErrorEndsRequest(event responsesWSErrorEvent, streamID, responseID string, control []byte) (terminal, ambiguous, controlError bool) {
	if len(control) > 0 {
		var pending struct {
			EventID    string `json:"event_id"`
			StreamID   string `json:"stream_id"`
			ResponseID string `json:"response_id"`
		}
		_ = common.Unmarshal(control, &pending)
		if pending.StreamID == "" {
			pending.StreamID = streamID
		}
		if event.EventID != "" && event.EventID == pending.EventID {
			return false, false, true
		}
		if event.ResponseID != "" && event.ResponseID == pending.ResponseID && event.ResponseID != responseID {
			return false, false, true
		}
		if (event.StreamID == "" || event.StreamID == pending.StreamID) && event.Error != nil && event.Error.Type == "invalid_request_error" {
			switch event.Error.Code {
			case "response_not_found", "response_not_active", "response_already_completed":
				return false, false, true
			}
		}
	}
	if event.StreamID != "" && event.StreamID != streamID || responseID != "" && event.ResponseID != "" && event.ResponseID != responseID {
		return false, false, false
	}
	return true, len(control) > 0 && event.ResponseID == "", false
}

func (s *responsesWSSession) restoreConnectionContext(c *gin.Context, model string) *types.NewAPIError {
	// The channel type is not rechecked: a locked channel keeps its type, and
	// the FilterResponsesWebSocket rules were applied when it was selected.
	channel, err := appmodel.CacheGetChannel(s.lockedChannelID)
	if err != nil || channel == nil || channel.Status != common.ChannelStatusEnabled || !channel.GetSetting().ResponsesWebSocketEnabled {
		return types.NewErrorWithStatusCode(errors.New("Responses WebSocket is disabled for this channel"), types.ErrorCode(appdto.FilterResponsesWebSocket), http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	keyEnabled := channel.Key == s.lockedKey
	if channel.ChannelInfo.IsMultiKey {
		keys := channel.GetKeys()
		status := channel.ChannelInfo.MultiKeyStatusList[s.lockedKeyIndex]
		keyEnabled = s.lockedKeyIndex >= 0 && s.lockedKeyIndex < len(keys) && keys[s.lockedKeyIndex] == s.lockedKey && (status == 0 || status == common.ChannelStatusEnabled)
	}
	if !keyEnabled {
		return types.NewErrorWithStatusCode(errors.New("the upstream connection credential is no longer enabled"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	// Changes that alter the physical upstream connection require a new
	// handshake. Request-level settings can be refreshed without rotating keys.
	if channel.GetBaseURL() != s.lockedContext[appconstant.ContextKeyChannelBaseUrl] ||
		!reflect.DeepEqual(channel.GetHeaderOverride(), s.lockedContext[appconstant.ContextKeyChannelHeaderOverride]) {
		return types.NewErrorWithStatusCode(errors.New("upstream connection settings changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	if previous, ok := s.lockedContext[appconstant.ContextKeyChannelSetting].(dto.ChannelSettings); ok && previous.Proxy != channel.GetSetting().Proxy {
		return types.NewErrorWithStatusCode(errors.New("upstream proxy changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	// An advanced custom connection was dialed through the route matched for
	// this model. Only the fields that shape the upstream handshake force a
	// reconnect: target path, protocol conversion and credential placement.
	// Other channel types match the zero route on both sides.
	route, _ := channel.GetOtherSettings().AdvancedCustom.MatchPathForModel(c.Request.URL.Path, model)
	if strings.TrimSpace(route.UpstreamPath) != strings.TrimSpace(s.lockedRoute.UpstreamPath) ||
		route.IsNative() != s.lockedRoute.IsNative() ||
		!reflect.DeepEqual(route.Auth, s.lockedRoute.Auth) {
		return types.NewErrorWithStatusCode(errors.New("upstream route changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	if pin, found, _ := service.GetChannelConstraints(c).ResolvedPin(); found {
		if pin.ChannelId != s.lockedChannelID {
			return types.NewErrorWithStatusCode(errors.New("channel pin changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
		}
	} else {
		group := common.GetContextKeyString(c, appconstant.ContextKeyUsingGroup)
		if group == "auto" {
			if !slices.Contains(service.GetRequestAutoGroups(c, common.GetContextKeyString(c, appconstant.ContextKeyUserGroup)), s.lockedGroup) {
				return types.NewErrorWithStatusCode(errors.New("the connection group is no longer allowed"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
			}
			group = s.lockedGroup
			common.SetContextKey(c, appconstant.ContextKeyAutoGroup, group)
		}
		if !appmodel.IsChannelEnabledForGroupModel(group, model, s.lockedChannelID) {
			return types.NewErrorWithStatusCode(errors.New("the connection channel is no longer allowed for this group and model"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
		}
	}
	for key, value := range s.lockedContext {
		c.Set(string(key), value)
	}
	common.SetContextKey(c, appconstant.ContextKeyChannelSetting, channel.GetSetting())
	common.SetContextKey(c, appconstant.ContextKeyChannelOtherSetting, channel.GetOtherSettings())
	common.SetContextKey(c, appconstant.ContextKeyChannelParamOverride, channel.GetParamOverride())
	common.SetContextKey(c, appconstant.ContextKeyChannelModelMapping, channel.GetModelMapping())
	common.SetContextKey(c, appconstant.ContextKeyChannelStatusCodeMapping, channel.GetStatusCodeMapping())
	policy := service.RequestPolicy(c)
	policy.BeginAttempt(channel, s.lockedGroup)
	policy.AddEvent(service.PolicyEvent{ChannelID: channel.Id, Decision: service.PolicyDecision{Action: "select", Reason: "pinned_channel", Source: "channel_constraint"}})
	return nil
}

func buildResponsesWSCreatePayload(c *gin.Context, info *relaycommon.RelayInfo, req dto.OpenAIResponsesRequest, generate common.RawMessage, streamID string) ([]byte, *types.NewAPIError) {
	_, jsonData, storage, apiErr := prepareResponsesRequest(c, info, &req)
	if apiErr != nil {
		return nil, apiErr
	}
	if storage != nil {
		var err error
		jsonData, err = storage.Bytes()
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
		}
	}
	event, err := buildResponsesWSCreateEvent(jsonData, generate, streamID)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	return event, nil
}

// enqueue applies connection-wide backpressure without creating a goroutine or
// retaining an authenticated context for a waiting request.
func (s *responsesWSSession) enqueue(streamID string, body []byte) *types.NewAPIError {
	for {
		s.stateMu.Lock()
		if s.closing || s.ctx.Err() != nil {
			s.stateMu.Unlock()
			return nil
		}
		stream := s.streams[streamID]
		if stream == nil && streamID != "" && s.namedStreams == responsesWSMaxStreams {
			s.stateMu.Unlock()
			return types.WithOpenAIError(types.OpenAIError{Type: "invalid_request_error", Code: "websocket_stream_limit_reached", Param: "stream_id", Message: i18n.Translate(s.lang, i18n.MsgResponsesWSStreamLimit)}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if s.queued < responsesWSMaxQueued && int64(len(body)) <= s.maxQueuedBytes-s.queuedBytes {
			if stream == nil {
				stream = &responsesWSStream{}
				s.streams[streamID] = stream
				if streamID != "" {
					s.namedStreams++
				}
			}
			stream.queue = append(stream.queue, responsesWSPendingRequest{body: body, index: s.nextEventIndex})
			s.nextEventIndex++
			s.queued++
			s.queuedBytes += int64(len(body))
			s.scheduleLocked()
			s.stateMu.Unlock()
			return nil
		}
		changed := s.changed
		s.stateMu.Unlock()
		select {
		case <-changed:
		case <-s.ctx.Done():
			return nil
		}
	}
}

// scheduleLocked starts the oldest runnable lane head. Only the first worker
// may establish a connection; once it publishes the binding, all lanes can run.
func (s *responsesWSSession) scheduleLocked() {
	for !s.closing && s.ctx.Err() == nil && s.active < responsesWSMaxActive && (s.connected || s.active == 0) {
		var next *responsesWSStream
		var streamID string
		for id, stream := range s.streams {
			if stream.current == nil && (stream.steering == nil || stream.steering.reservation == nil) && len(stream.queue) > 0 && (next == nil || stream.queue[0].index < next.queue[0].index) {
				next, streamID = stream, id
			}
		}
		if next == nil {
			return
		}
		pending := next.queue[0]
		next.queue[0] = responsesWSPendingRequest{}
		next.queue = next.queue[1:]
		s.queued--
		s.queuedBytes -= int64(len(pending.body))
		close(s.changed)
		s.changed = make(chan struct{})
		state := &responsesWSCallState{streamID: streamID, previousResponseID: next.lastResponseID,
			inbox: make(chan responsesWSMessage, 1), controls: make(chan responsesWSControl, 1), done: make(chan struct{})}
		next.current = state
		s.active++
		requestID := fmt.Sprintf("%s-ws-%d", s.requestID, pending.index)
		s.workers.Go(func() { s.runRequest(state, pending.body, requestID) })
	}
}

func (s *responsesWSSession) controlOwner(streamID, responseID string) *responsesWSCallState {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closing {
		return nil
	}
	if responseID != "" {
		for _, stream := range s.streams {
			if state := stream.current; state != nil && state.responseID == responseID {
				if streamID != "" && streamID != state.streamID {
					return nil
				}
				return state
			}
		}
	}
	if stream := s.streams[streamID]; stream != nil && stream.current != nil {
		return stream.current
	}
	if streamID == "" && s.active == 1 {
		for _, stream := range s.streams {
			if stream.current != nil {
				return stream.current
			}
		}
	}
	return nil
}

// routeTargetEvent assigns a frame before it can reach any usage accumulator.
// It also marks terminal frames before delivery, so repeated frames or an EOF
// cannot overtake the worker's settlement and client-facing terminal write.
func (s *responsesWSSession) routeTargetEvent(body []byte) (owner *responsesWSCallState, drop, fatal bool) {
	var event struct {
		responsesWSErrorEvent
		Response *struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if common.Unmarshal(body, &event) != nil {
		return nil, false, true
	}
	responseID := event.ResponseID
	if event.Response != nil && event.Response.ID != "" {
		responseID = event.Response.ID
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if event.Type == "error" && event.Error != nil && event.Error.Code == "websocket_connection_limit_reached" {
		return nil, false, true
	}
	var sole *responsesWSCallState
	active := 0
	completed := responseID != "" && s.completedResponses[responseID]
	for _, stream := range s.streams {
		if responseID != "" && responseID == stream.lastResponseID {
			completed = true
		}
		state := stream.current
		if state == nil || !state.submitted {
			continue
		}
		active++
		sole = state
		if event.StreamID != "" {
			if state.streamID == event.StreamID {
				owner = state
			}
			continue
		}
		matches := responseID != "" && responseID == state.responseID
		if event.Type == "error" && len(state.sentControl) > 0 {
			matches = matches || event.EventID != "" && event.EventID == gjson.GetBytes(state.sentControl, "event_id").String() ||
				responseID != "" && responseID == gjson.GetBytes(state.sentControl, "response_id").String()
			if event.EventID == "" && responseID == "" {
				_, _, controlError := responsesWSErrorEndsRequest(event.responsesWSErrorEvent, state.streamID, state.responseID, state.sentControl)
				matches = matches || controlError
			}
		}
		if matches {
			if owner != nil && owner != state {
				return nil, false, true
			}
			owner = state
		}
	}
	if completed {
		controlError := false
		if owner != nil && event.Type == "error" {
			_, _, controlError = responsesWSErrorEndsRequest(event.responsesWSErrorEvent, owner.streamID, owner.responseID, owner.sentControl)
		}
		if !controlError {
			return nil, true, false
		}
	}
	if owner == nil && event.StreamID == "" {
		// An unscoped error with concurrent lanes may be connection-wide;
		// the existence of a default lane is not enough to assign it there.
		if stream := s.streams[""]; event.Type != "error" && stream != nil && stream.current != nil && stream.current.submitted {
			owner = stream.current
		} else if active == 1 {
			owner = sole
		}
	}
	if owner == nil {
		return nil, false, event.Type == "error" && event.StreamID == "" && responseID == "" && active > 0
	}
	if owner.upstreamDone {
		return nil, true, false
	}
	terminal := false
	if event.Type == "error" {
		terminal, _, _ = responsesWSErrorEndsRequest(event.responsesWSErrorEvent, owner.streamID, owner.responseID, owner.sentControl)
	} else {
		if responseID != "" && owner.responseID != "" && responseID != owner.responseID {
			return nil, false, false
		}
		switch event.Type {
		case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
			terminal = true
		}
	}
	if responseID != "" && (terminal || strings.HasPrefix(event.Type, "response.")) {
		owner.responseID = responseID
	}
	owner.upstreamDone = terminal
	return owner, false, false
}

func (s *responsesWSSession) startTargetReader(target *websocket.Conn) {
	limit := int64(helper.DefaultMaxScannerBufferSize)
	if appconstant.StreamScannerMaxBufferMB > 0 {
		limit = int64(appconstant.StreamScannerMaxBufferMB) << 20
	}
	target.SetReadLimit(limit)
	s.workers.Go(func() {
		for {
			kind, body, err := target.ReadMessage()
			incoming := responsesWSMessage{kind: kind, body: body, err: err}
			if err != nil {
				s.stateMu.Lock()
				s.closing, s.upstreamErr = true, err
				var finishing []*responsesWSCallState
				var interruptedStates []*responsesWSCallState
				interrupted := false
				for _, stream := range s.streams {
					if state := stream.current; state != nil {
						finishing = append(finishing, state)
						interrupted = interrupted || !state.upstreamDone
						if !state.upstreamDone {
							interruptedStates = append(interruptedStates, state)
						}
					}
					if pending := stream.steering; pending != nil && pending.reservation != nil && pending.reservation != stream.current {
						finishing = append(finishing, pending.reservation)
						interruptedStates = append(interruptedStates, pending.reservation)
					}
				}
				s.stateMu.Unlock()
				for _, state := range interruptedStates {
					select {
					case state.inbox <- incoming:
					case <-state.done:
					case <-s.ctx.Done():
						return
					}
				}
				for _, state := range finishing {
					select {
					case <-state.done:
					case <-s.ctx.Done():
						return
					}
				}
				code := websocket.CloseInternalServerErr
				if !interrupted && websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					code = err.(*websocket.CloseError).Code
				}
				s.shutdown(code, "upstream connection closed")
				return
			}
			if s.handleSteeringEvent(incoming) {
				continue
			}
			if !s.admitAutomaticResponse(body) {
				s.shutdown(websocket.CloseInternalServerErr, "unreserved upstream response")
				return
			}
			state, drop, fatal := s.routeTargetEvent(body)
			if drop {
				continue
			}
			if state != nil {
				select {
				case state.inbox <- incoming:
				case <-state.done:
				case <-s.ctx.Done():
					return
				}
				continue
			}
			if writeErr := s.writeClient(kind, body); writeErr != nil || fatal {
				s.shutdown(websocket.CloseInternalServerErr, "")
				return
			}
		}
	})
}

func (s *responsesWSSession) writeCreate(state *responsesWSCallState, payload []byte) error {
	s.stateMu.Lock()
	if s.closing || s.ctx.Err() != nil {
		s.stateMu.Unlock()
		return context.Canceled
	}
	state.submitted = true
	s.stateMu.Unlock()
	return s.writeTarget(websocket.TextMessage, payload)
}

func (s *responsesWSSession) writeControl(state *responsesWSCallState, payload []byte) error {
	s.stateMu.Lock()
	state.sentControl = payload
	s.stateMu.Unlock()
	return s.writeTarget(websocket.TextMessage, payload)
}

func (s *responsesWSSession) getTarget() *websocket.Conn {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	return s.target
}

func (s *responsesWSSession) setTarget(target *websocket.Conn) bool {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	if s.ctx.Err() != nil {
		_ = target.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = target.Close()
		return false
	}
	s.target = target
	return true
}

func (s *responsesWSSession) writeTarget(kind int, message []byte) error {
	s.targetWriteMu.Lock()
	defer s.targetWriteMu.Unlock()
	target := s.getTarget()
	if target == nil {
		return errors.New("responses websocket upstream is not connected")
	}
	if err := target.SetWriteDeadline(time.Now().Add(responsesWSWriteTimeout)); err != nil {
		return err
	}
	return target.WriteMessage(kind, message)
}

func (s *responsesWSSession) writeClient(kind int, message []byte) error {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	if err := s.client.SetWriteDeadline(time.Now().Add(responsesWSWriteTimeout)); err != nil {
		return err
	}
	return s.client.WriteMessage(kind, message)
}

func (s *responsesWSSession) sendError(eventID, streamID string, apiErr *types.NewAPIError) {
	if apiErr == nil {
		return
	}
	payload, err := buildResponsesWSErrorPayload(eventID, streamID, apiErr)
	if err == nil {
		_ = s.writeClient(websocket.TextMessage, payload)
	}
}

// shutdown ends the connection once. Each peer gets a close frame that says
// why, bounded to one second, and both sockets close even if a write failed.
// Close frames are control writes, which gorilla allows alongside a blocked
// data writer, so shutdown takes no write lock and never waits for workers.
func (s *responsesWSSession) shutdown(code int, reason string) {
	s.closeOnce.Do(func() {
		s.cancel()
		s.stateMu.Lock()
		s.closing = true
		for _, stream := range s.streams {
			stream.queue = nil
		}
		s.queued, s.queuedBytes = 0, 0
		if s.changed != nil {
			close(s.changed)
			s.changed = nil
		}
		s.stateMu.Unlock()
		s.connectionMu.Lock()
		target, unregister := s.target, s.unregister
		s.target, s.unregister = nil, nil
		s.connectionMu.Unlock()
		if unregister != nil {
			unregister()
		}
		closeMessage := websocket.FormatCloseMessage(code, reason)
		_ = s.client.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(time.Second))
		if target != nil {
			_ = target.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(time.Second))
			_ = target.Close()
		}
		_ = s.client.Close()
	})
}

func (s *responsesWSSession) registerChannelClose(channelID int) {
	unregister := wsmanager.Register(channelID, wsmanager.KindResponses, s.closeForPolicy)
	s.connectionMu.Lock()
	if s.ctx.Err() != nil {
		s.connectionMu.Unlock()
		unregister()
		return
	}
	s.unregister = unregister
	s.connectionMu.Unlock()
}

// closeForPolicy tells both peers why the connection ends, like the realtime
// relay does.
func (s *responsesWSSession) closeForPolicy(reason string) {
	s.shutdown(websocket.ClosePolicyViolation, reason)
}

// Stream identity belongs to the WebSocket envelope. For the legacy wrapped
// input, the top-level field takes precedence over response.stream_id.
func parseResponsesWSEnvelope(message []byte) (responsesWSCreateEvent, string, error) {
	var event responsesWSCreateEvent
	if err := common.Unmarshal(message, &event); err != nil {
		return event, "", fmt.Errorf("invalid websocket event json: %w", err)
	}
	streamRaw := event.StreamID
	if len(streamRaw) == 0 && len(event.Request) > 0 {
		var wrapped struct {
			StreamID common.RawMessage `json:"stream_id"`
		}
		if err := common.Unmarshal(event.Request, &wrapped); err == nil {
			streamRaw = wrapped.StreamID
		}
	}
	var streamID string
	if len(streamRaw) > 0 {
		if err := common.Unmarshal(streamRaw, &streamID); err != nil || len(streamID) < 1 || len(streamID) > 256 {
			return event, "", errResponsesWSInvalidStreamID
		}
		for _, char := range streamID {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
				return event, "", errResponsesWSInvalidStreamID
			}
		}
	}
	if strings.TrimSpace(event.Type) == "" {
		return event, streamID, errors.New("websocket event type is required")
	}
	return event, streamID, nil
}

func newResponsesWSInvalidRequestError(err error) *types.NewAPIError {
	return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
}

// normalizeResponsesWSCreateEvent builds the HTTP-shaped request body from a
// response.create whose envelope the read loop already parsed and validated.
// The message is decoded once more as a raw map (the body source) and once as
// the typed request; the envelope is not parsed again.
func normalizeResponsesWSCreateEvent(message []byte, event responsesWSCreateEvent, streamID string) (responsesWSCreateRequest, error) {
	create := responsesWSCreateRequest{StreamID: streamID, Generate: event.Generate}
	// Decode the wrapped request on its own so outer fields cannot enter the HTTP body.
	source := message
	if len(event.Request) > 0 {
		source = event.Request
	}
	var raw map[string]common.RawMessage
	if err := common.Unmarshal(source, &raw); err != nil {
		return create, err
	}
	if len(event.Request) > 0 {
		if len(create.Generate) == 0 {
			create.Generate = raw["generate"]
		}
	} else {
		for _, key := range []string{"type", "event_id", "background", "stream", "stream_options"} {
			delete(raw, key)
		}
	}
	delete(raw, "generate")
	delete(raw, "stream_id")
	payload, err := common.Marshal(raw)
	if err != nil {
		return create, err
	}
	if err := common.Unmarshal(payload, &create.Request); err != nil {
		return create, err
	}
	if helper.ExceedsMaxTokensLimit(create.Request.MaxOutputTokens) {
		return create, errors.New("max_output_tokens is invalid")
	}
	create.Request.Stream = nil
	create.Request.StreamOptions = nil
	create.Body = payload
	return create, nil
}

func buildResponsesWSCreateEvent(jsonData []byte, generate common.RawMessage, streamID string) ([]byte, error) {
	if !gjson.ValidBytes(jsonData) {
		return nil, errors.New("invalid responses request JSON")
	}
	// Borrow a read-only view for traversal; all offsets/keys stay in this call.
	// The event gets a separate buffer, including when jsonData belongs to the
	// incoming request's memory storage and must survive another attempt.
	data := gjson.Parse(unsafe.String(unsafe.SliceData(jsonData), len(jsonData)))
	if !data.IsObject() {
		return nil, errors.New("responses request must be a JSON object")
	}
	envelope, err := common.Marshal(struct {
		Type     string            `json:"type"`
		StreamID string            `json:"stream_id,omitempty"`
		Generate common.RawMessage `json:"generate,omitempty"`
	}{Type: responsesWSEventTypeResponseCreate, StreamID: streamID, Generate: generate})
	if err != nil {
		return nil, err
	}

	type memberSpan struct{ start, end int }
	var localMembers [16]memberSpan
	members := localMembers[:0]
	positions := make(map[string]int)
	data.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "type", "stream_id", "event_id", "background", "stream", "stream_options":
			// Channel overrides must not shadow the WebSocket envelope.
			return true
		case "generate":
			if len(generate) > 0 {
				return true
			}
		}
		span := memberSpan{start: key.Index, end: value.Index + len(value.Raw)}
		if index, exists := positions[key.Str]; exists {
			// Preserve the old map decoder's last-value-wins behavior, also
			// for keys with different JSON escaping but the same decoded name.
			members[index] = span
		} else {
			positions[key.Str] = len(members)
			members = append(members, span)
		}
		return true
	})
	outputSize := len(envelope)
	for _, span := range members {
		outputSize += 1 + span.end - span.start
	}
	event := make([]byte, 0, outputSize)
	event = append(event, envelope[:len(envelope)-1]...)
	for _, span := range members {
		event = append(event, ',')
		event = append(event, jsonData[span.start:span.end]...)
	}
	return append(event, '}'), nil
}

func buildResponsesWSErrorPayload(eventID, streamID string, apiErr *types.NewAPIError, upstreamURLs ...string) ([]byte, error) {
	if apiErr == nil {
		return nil, errors.New("api error is nil")
	}
	status := apiErr.StatusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}
	openaiErr := apiErr.ToOpenAIError(upstreamURLs...)
	return common.Marshal(&responsesWSErrorEvent{
		Type:     "error",
		Status:   status,
		EventID:  eventID,
		StreamID: streamID,
		Error:    &openaiErr,
	})
}

// checkResponsesWSModelAccess applies the token model limit with the same name
// matching as the HTTP distributor. Unlike HTTP, it also runs for requests
// pinned to a channel: the check is stricter there on purpose, because a
// persistent connection keeps serving the model after the pin was resolved.
func checkResponsesWSModelAccess(c *gin.Context, modelName string) *types.NewAPIError {
	if !common.GetContextKeyBool(c, appconstant.ContextKeyTokenModelLimitEnabled) {
		return nil
	}
	raw, ok := common.GetContextKey(c, appconstant.ContextKeyTokenModelLimit)
	if !ok {
		return types.NewErrorWithStatusCode(errors.New("token has no model access"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	tokenModelLimit, ok := raw.(map[string]bool)
	if !ok {
		tokenModelLimit = map[string]bool{}
	}
	if !middleware.TokenModelLimitAllows(tokenModelLimit, modelName) {
		return types.NewErrorWithStatusCode(fmt.Errorf("token is not allowed to use model %s", modelName), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	return nil
}

// selectResponsesWSChannel narrows the shared HTTP selection to channels that
// speak the Responses WebSocket protocol and renders its outcome as a
// non-retryable NewAPIError.
func selectResponsesWSChannel(c *gin.Context, modelName string, retryParam *service.RetryParam) (*appmodel.Channel, *types.NewAPIError) {
	constraints := service.GetChannelConstraints(c)
	if !slices.ContainsFunc(constraints.Filters, func(filter appdto.ChannelFilter) bool {
		return filter.Kind == appdto.FilterResponsesWebSocket
	}) {
		constraints.AddFilter(appdto.ChannelFilter{Kind: appdto.FilterResponsesWebSocket})
	}
	channel, _, selectErr := service.SelectChannelForRequest(c, modelName, retryParam)
	if selectErr != nil {
		message := selectErr.Message
		if selectErr.MessageID != "" {
			message = i18n.T(c, selectErr.MessageID, selectErr.Params)
		}
		code := selectErr.Code
		if code == "" {
			code = types.ErrorCodeGetChannelFailed
		}
		return nil, types.NewErrorWithStatusCode(errors.New(message), code, selectErr.StatusCode, types.ErrOptionWithSkipRetry())
	}
	if err := middleware.SetupContextForSelectedChannel(c, channel, modelName); err != nil {
		return nil, err
	}
	return channel, nil
}
