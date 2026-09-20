package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
)

// ResponsesUsageEvent is an accounting projection, not a wire representation.
// Transports must forward the original bytes, including fields omitted here.
type ResponsesUsageEvent struct {
	dto.ResponsesStreamResponse
	StreamID string
}

// responsesUsageJSON borrows a field only for the duration of
// DecodeResponsesUsageEvent. Decode all retained values through the host codec
// before returning; these views must never escape into the event or accumulator.
// Keeping repeated fields lets the codec retain its normal merge/null semantics.
type responsesUsageJSON struct {
	data     []byte
	previous [][]byte
}

func (r *responsesUsageJSON) UnmarshalJSON(data []byte) error {
	if r.data != nil {
		r.previous = append(r.previous, r.data)
	}
	r.data = data
	return nil
}

func (r *responsesUsageJSON) decodeInto(value any) error {
	for _, data := range r.previous {
		if err := common.Unmarshal(data, value); err != nil {
			return err
		}
	}
	if len(r.data) == 0 {
		return nil
	}
	return common.Unmarshal(r.data, value)
}

type responsesUsageOutput struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	CallID string `json:"call_id"`
	Status string `json:"status"`
	Name   string `json:"name"`
	Result string `json:"result"`
}

type responsesUsageMetadata struct {
	Type     string `json:"type"`
	StreamID string `json:"stream_id"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Response *struct {
		ID                string                 `json:"id"`
		Model             string                 `json:"model"`
		Status            common.RawMessage      `json:"status"`
		Error             any                    `json:"error"`
		IncompleteDetails *dto.IncompleteDetails `json:"incomplete_details"`
		Usage             *dto.Usage             `json:"usage"`
		Output            responsesUsageJSON     `json:"output"`
	} `json:"response"`
}

func (o responsesUsageOutput) responsesOutput() dto.ResponsesOutput {
	return dto.ResponsesOutput{Type: o.Type, ID: o.ID, CallId: o.CallID, Status: o.Status, Name: o.Name, Result: o.Result}
}

// DecodeResponsesUsageEvent skips echoed requests, annotations, obfuscation,
// tool arguments and other payloads that do not contribute accounting facts.
// The host codec still validates the entire JSON document. Only terminal
// events without output usage need the output text for the existing estimate.
func DecodeResponsesUsageEvent(data []byte) (ResponsesUsageEvent, error) {
	// The lookup only selects a projection. The host codec validates the whole
	// event and supplies the authoritative type, including escaped/duplicate keys.
	eventType := gjson.GetBytes(data, "type").Str
	var metadata responsesUsageMetadata
	var event ResponsesUsageEvent
	for {
		var err error
		var delta string
		var item *responsesUsageOutput
		var outputIndex *int
		switch eventType {
		case "response.output_text.delta", "response.function_call_arguments.delta",
			"response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
			var projection struct {
				responsesUsageMetadata
				Delta string `json:"delta"`
			}
			err = common.Unmarshal(data, &projection)
			metadata, delta = projection.responsesUsageMetadata, projection.Delta
		case dto.ResponsesOutputTypeItemDone:
			var projection struct {
				responsesUsageMetadata
				Item        *responsesUsageOutput `json:"item"`
				OutputIndex *int                  `json:"output_index"`
			}
			err = common.Unmarshal(data, &projection)
			metadata, item, outputIndex = projection.responsesUsageMetadata, projection.Item, projection.OutputIndex
		default:
			var projection responsesUsageMetadata
			err = common.Unmarshal(data, &projection)
			metadata = projection
		}
		event = ResponsesUsageEvent{
			ResponsesStreamResponse: dto.ResponsesStreamResponse{Type: metadata.Type, Code: metadata.Code, Message: metadata.Message},
			StreamID:                metadata.StreamID,
		}
		if err != nil {
			return event, err
		}
		if metadata.Type != eventType {
			eventType = metadata.Type
			continue
		}
		event.Delta = delta
		event.OutputIndex = outputIndex
		if item != nil {
			output := item.responsesOutput()
			event.Item = &output
		}
		break
	}
	if response := metadata.Response; response != nil {
		event.Response = &dto.OpenAIResponsesResponse{
			ID: response.ID, Model: response.Model, Status: response.Status,
			Error: response.Error, IncompleteDetails: response.IncompleteDetails, Usage: response.Usage,
		}
	}
	switch event.Type {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		if event.Response == nil {
			return event, nil
		}
		usage := event.Response.Usage
		// ApplyResponsesUsage uses the native Responses output_tokens field;
		// a chat-style completion_tokens field alone still needs the fallback.
		if usage != nil && usage.OutputTokens != 0 {
			var outputs []responsesUsageOutput
			if err := metadata.Response.Output.decodeInto(&outputs); err != nil {
				return event, err
			}
			if outputs != nil {
				event.Response.Output = make([]dto.ResponsesOutput, len(outputs))
			}
			for i, output := range outputs {
				// Retain the real image result: the counter hashes it for dedup,
				// including upstreams that omit both item and call IDs.
				event.Response.Output[i] = output.responsesOutput()
			}
			return event, nil
		}
		var outputs []struct {
			responsesUsageOutput
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := metadata.Response.Output.decodeInto(&outputs); err != nil {
			return event, err
		}
		if outputs != nil {
			event.Response.Output = make([]dto.ResponsesOutput, len(outputs))
		}
		for i, output := range outputs {
			event.Response.Output[i] = output.responsesOutput()
			event.Response.Output[i].Role = output.Role
			if len(output.Content) > 0 {
				event.Response.Output[i].Content = make([]dto.ResponsesOutputContent, len(output.Content))
			}
			for j, content := range output.Content {
				event.Response.Output[i].Content[j] = dto.ResponsesOutputContent{Type: content.Type, Text: content.Text}
			}
		}
	}
	return event, nil
}
