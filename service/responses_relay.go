package service

import (
	"bytes"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
)

// Native relays forward the original payload. These projections only decode
// facts consumed by routing, health classification and accounting; echoed
// raw JSON and output text stay on wire. Validation-only fields preserve the
// full DTO's type checks, including nested tool schemas and annotations.
type responsesDiscardedString struct{}

func (*responsesDiscardedString) UnmarshalJSON(data []byte) error {
	// The enclosing codec validates JSON syntax. Checking the type avoids
	// allocating a decoded copy of large text that only needs forwarding.
	if len(data) > 0 && data[0] == '"' || bytes.Equal(data, []byte("null")) {
		return nil
	}
	var value string
	return common.Unmarshal(data, &value)
}

type responsesRelayContent struct {
	Type        responsesDiscardedString `json:"type"`
	Text        responsesDiscardedString `json:"text"`
	Annotations []any                    `json:"annotations"`
}

type responsesRelaySummary struct {
	Type responsesDiscardedString `json:"type"`
	Text responsesDiscardedString `json:"text"`
}

type responsesRelayOutput struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Result string `json:"result"`

	Role              responsesDiscardedString `json:"role"`
	Content           []responsesRelayContent  `json:"content"`
	Summary           []responsesRelaySummary  `json:"summary"`
	Quality           responsesDiscardedString `json:"quality"`
	Size              responsesDiscardedString `json:"size"`
	ContainerID       responsesDiscardedString `json:"container_id"`
	ServerLabel       responsesDiscardedString `json:"server_label"`
	ApprovalRequestID responsesDiscardedString `json:"approval_request_id"`
}

func (o *responsesRelayOutput) output() dto.ResponsesOutput {
	return dto.ResponsesOutput{
		Type: o.Type, ID: o.ID, Status: o.Status,
		CallId: o.CallID, Name: o.Name, Result: o.Result,
	}
}

type responsesRelayResponse struct {
	ID                string                 `json:"id"`
	CreatedAt         dto.IntValue           `json:"created_at"`
	Model             string                 `json:"model"`
	Status            common.RawMessage      `json:"status"`
	Error             any                    `json:"error"`
	IncompleteDetails *dto.IncompleteDetails `json:"incomplete_details"`
	Usage             *dto.Usage             `json:"usage"`
	Output            []responsesRelayOutput `json:"output"`

	Object            responsesDiscardedString `json:"object"`
	MaxOutputTokens   int                      `json:"max_output_tokens"`
	ParallelToolCalls bool                     `json:"parallel_tool_calls"`
	Store             bool                     `json:"store"`
	Temperature       float64                  `json:"temperature"`
	TopP              float64                  `json:"top_p"`
	Tools             []map[string]any         `json:"tools"`
	Reasoning         *struct {
		Effort  responsesDiscardedString `json:"effort"`
		Summary responsesDiscardedString `json:"summary"`
	} `json:"reasoning"`
}

func (r *responsesRelayResponse) response() *dto.OpenAIResponsesResponse {
	if r == nil {
		return nil
	}
	response := &dto.OpenAIResponsesResponse{
		ID: r.ID, CreatedAt: r.CreatedAt, Model: r.Model,
		Status: r.Status, Error: r.Error, IncompleteDetails: r.IncompleteDetails,
		Usage: r.Usage,
	}
	if r.Output != nil {
		response.Output = make([]dto.ResponsesOutput, len(r.Output))
		for i := range r.Output {
			response.Output[i] = r.Output[i].output()
		}
	}
	return response
}

// DecodeResponsesRelayResponse returns accounting metadata, not a response to
// marshal back to the client. The caller must forward the original JSON bytes.
func DecodeResponsesRelayResponse(data []byte) (*dto.OpenAIResponsesResponse, error) {
	var metadata responsesRelayResponse
	if err := common.Unmarshal(data, &metadata); err != nil {
		// Preserve the original DTO's error and partial decoding on bad input.
		var response dto.OpenAIResponsesResponse
		err = common.Unmarshal(data, &response)
		return &response, err
	}
	return metadata.response(), nil
}

type responsesRelayEvent struct {
	Type        string                  `json:"type"`
	Response    *responsesRelayResponse `json:"response"`
	Code        string                  `json:"code"`
	Message     string                  `json:"message"`
	Delta       string                  `json:"delta"`
	Item        *responsesRelayOutput   `json:"item"`
	OutputIndex *int                    `json:"output_index"`

	Param           responsesDiscardedString `json:"param"`
	Arguments       responsesDiscardedString `json:"arguments"`
	Input           responsesDiscardedString `json:"input"`
	Name            responsesDiscardedString `json:"name"`
	Text            responsesDiscardedString `json:"text"`
	SequenceNumber  int                      `json:"sequence_number"`
	AnnotationIndex int                      `json:"annotation_index"`
	Obfuscation     responsesDiscardedString `json:"obfuscation"`
	ContentIndex    int                      `json:"content_index"`
	SummaryIndex    int                      `json:"summary_index"`
	ItemID          responsesDiscardedString `json:"item_id"`
	Part            *responsesRelaySummary   `json:"part"`
}

// DecodeEvent reads native SSE/WebSocket accounting facts without retaining the
// wire payload. Observe remains the shared owner of accounting and outcomes.
// websocket enables the envelope's stream ID validation, which HTTP does not use.
func (a *ResponsesUsageAccumulator) DecodeEvent(data []byte, websocket bool) (dto.ResponsesStreamResponse, string, error) {
	if a.usage.CompletionTokens == 0 && a.outputText.Len() == 0 {
		switch gjson.GetBytes(data, "type").Str {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			// Without earlier deltas this terminal may be the only source of
			// billable text. Decode it once with the full DTO, rather than first
			// scanning a large output and then decoding it again for estimation.
			return decodeFullResponsesRelayEvent(data, websocket)
		}
	}
	var metadata struct {
		responsesRelayEvent
		StreamID string `json:"stream_id"`
	}
	// HTTP treats stream_id as an unknown extension. Only the WebSocket
	// envelope consumes and validates it.
	var target any = &metadata.responsesRelayEvent
	if websocket {
		target = &metadata
	}
	err := common.Unmarshal(data, target)
	needsOutputText := false
	if a.usage.CompletionTokens == 0 && a.outputText.Len() == 0 {
		switch metadata.Type {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			// The codec also accepts escaped/case-folded keys and duplicate
			// fields; preserve that behavior when the quick lookup missed them.
			needsOutputText = true
		}
	}
	if err != nil || needsOutputText {
		return decodeFullResponsesRelayEvent(data, websocket)
	}
	event := dto.ResponsesStreamResponse{
		Type: metadata.Type, Response: metadata.Response.response(),
		Code: metadata.Code, Message: metadata.Message, Delta: metadata.Delta,
		OutputIndex: metadata.OutputIndex,
	}
	if metadata.Item != nil {
		item := metadata.Item.output()
		event.Item = &item
	}
	return event, metadata.StreamID, nil
}

func decodeFullResponsesRelayEvent(data []byte, websocket bool) (dto.ResponsesStreamResponse, string, error) {
	var event struct {
		dto.ResponsesStreamResponse
		StreamID string `json:"stream_id"`
	}
	var target any = &event.ResponsesStreamResponse
	if websocket {
		target = &event
	}
	err := common.Unmarshal(data, target)
	return event.ResponsesStreamResponse, event.StreamID, err
}
