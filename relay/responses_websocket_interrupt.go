package relay

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// An interrupt can outlive completion history. Keep its response record
// until acknowledgement so late control replies share its client sequence.
type responsesWSInterrupt struct {
	response *responsesWSResponse
	eventID  string
	size     int64
}

func (s *responsesWSSession) submitInterrupt(body []byte, event responsesWSCreateEvent, streamID string) (string, *types.NewAPIError) {
	if strings.TrimSpace(event.ResponseID) == "" {
		return streamID, newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSInvalidInterrupt)))
	}
	s.stateMu.Lock()
	response, owner := s.responseByIDLocked(event.ResponseID)
	if response == nil || streamID != "" && streamID != response.streamID || s.closing {
		s.stateMu.Unlock()
		return streamID, types.WithOpenAIError(types.OpenAIError{
			Type: "invalid_request_error", Code: "response_not_found", Param: "response_id",
			Message: i18n.Translate(s.lang, i18n.MsgResponsesWSResponseNotFound),
		}, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	lane := response.streamID
	if s.interrupts[event.ResponseID] != nil || owner != nil && owner.cancelPending {
		s.stateMu.Unlock()
		return lane, newResponsesWSInvalidRequestError(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSControlPending)))
	}
	size := int64(len(event.ResponseID)) + int64(len(event.EventID)) + int64(len(lane))
	if len(s.interrupts) >= responsesWSMaxQueued || size > s.maxQueuedBytes-s.interruptBytes {
		s.stateMu.Unlock()
		return lane, types.NewErrorWithStatusCode(errors.New(i18n.Translate(s.lang, i18n.MsgResponsesWSInterruptLimit)), types.ErrorCodeInvalidRequest, http.StatusTooManyRequests, types.ErrOptionWithSkipRetry())
	}
	pending := &responsesWSInterrupt{response: response, eventID: event.EventID, size: size}
	if s.interrupts == nil {
		s.interrupts = make(map[string]*responsesWSInterrupt)
	}
	s.interrupts[event.ResponseID] = pending
	s.interruptBytes += size
	s.stateMu.Unlock()
	// A known response ID proves create already reached this connection. The
	// worker may finish meanwhile; interruption still targets that exact ID.
	if err := s.writeTarget(websocket.TextMessage, body); err != nil {
		s.shutdown(websocket.CloseInternalServerErr, "")
	}
	return lane, nil
}

func (s *responsesWSSession) handleInterruptEvent(incoming responsesWSMessage) bool {
	eventType := gjson.GetBytes(incoming.body, "type").String()
	ack := eventType == "response.interrupt.accepted" || eventType == "response.interrupt.failed"
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
	pending := s.interrupts[responseID]
	if pending != nil && event.StreamID != "" && event.StreamID != pending.response.streamID {
		pending = nil
	}
	if !ack {
		// Generic errors need unambiguous correlation or a control-specific
		// rejection code. Explicit generation failures and cancel errors retain
		// their ordinary handling; unresolved interrupt errors must not end them.
		pending = nil
		ambiguous := false
		possibleInterrupt := false
		for id, candidate := range s.interrupts {
			if event.ResponseID != "" && event.ResponseID != id || event.StreamID != "" && event.StreamID != candidate.response.streamID {
				continue
			}
			possibleInterrupt = true
			matches := event.EventID != "" && event.EventID == candidate.eventID
			if event.EventID == "" && event.Error != nil && event.Error.Type == "invalid_request_error" {
				switch event.Error.Code {
				case "response_not_found", "response_not_active", "response_already_completed":
					matches = true
				}
			}
			if !matches && event.ResponseID == id && (event.EventID == "" || candidate.eventID == "") {
				// A finished response cannot have another generation failure.
				// Its interrupt can outlive the bounded completion history.
				stream := s.streams[candidate.response.streamID]
				matches = stream == nil || stream.current == nil || stream.current.responseID != id || stream.current.upstreamDone
			}
			if !matches {
				continue
			}
			if pending != nil {
				ambiguous = true
			}
			responseID, pending = id, candidate
		}
		if pending == nil && (!possibleInterrupt || event.ResponseID != "" ||
			event.StreamID == "" && s.defaultCreateErrorOwnerLocked(event.Error) != nil) {
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
			if pending == nil && event.EventID != "" && controlError {
				// An exact cancel event ID belongs to the ordinary control router.
				s.stateMu.Unlock()
				return false
			}
			ambiguous = ambiguous || controlError
		}
		if pending == nil {
			// A lane or unknown server event ID cannot distinguish a late
			// interrupt rejection from a newer generation's failure.
			ambiguous = true
		}
		if ambiguous {
			s.stateMu.Unlock()
			_ = s.writeClient(nil, incoming)
			s.shutdown(websocket.CloseInternalServerErr, "")
			return true
		}
	}
	response := s.controlReplyResponseLocked(responseID, event.StreamID)
	if pending != nil {
		delete(s.interrupts, responseID)
		s.interruptBytes -= pending.size
	}
	s.stateMu.Unlock()
	s.deliverControlReply(response, responsesWSControlReply{kind: incoming.kind, body: incoming.body})
	return true
}
