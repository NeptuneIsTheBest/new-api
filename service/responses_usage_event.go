package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// ResponsesUsageEvent is an accounting projection, not a wire representation.
// Transports must forward the original bytes, including fields omitted here.
type ResponsesUsageEvent struct {
	dto.ResponsesStreamResponse
	StreamID string
}

type responsesUsageOutput struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	CallID string `json:"call_id"`
	Status string `json:"status"`
	Name   string `json:"name"`
	Result string `json:"result"`
}

func (o responsesUsageOutput) responsesOutput() dto.ResponsesOutput {
	return dto.ResponsesOutput{Type: o.Type, ID: o.ID, CallId: o.CallID, Status: o.Status, Name: o.Name, Result: o.Result}
}

// DecodeResponsesUsageEvent skips echoed requests, annotations, obfuscation,
// tool arguments and other payloads that do not contribute accounting facts.
// The host codec still validates the entire JSON document. Only terminal
// events without output usage need the output text for the existing estimate.
func DecodeResponsesUsageEvent(data []byte) (ResponsesUsageEvent, error) {
	var metadata struct {
		Type     string `json:"type"`
		StreamID string `json:"stream_id"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		Delta    string `json:"delta"`
		Response *struct {
			ID                string                 `json:"id"`
			Model             string                 `json:"model"`
			Status            common.RawMessage      `json:"status"`
			Error             any                    `json:"error"`
			IncompleteDetails *dto.IncompleteDetails `json:"incomplete_details"`
			Usage             *dto.Usage             `json:"usage"`
		} `json:"response"`
	}
	err := common.Unmarshal(data, &metadata)
	event := ResponsesUsageEvent{
		ResponsesStreamResponse: dto.ResponsesStreamResponse{Type: metadata.Type, Code: metadata.Code, Message: metadata.Message, Delta: metadata.Delta},
		StreamID:                metadata.StreamID,
	}
	if err != nil {
		return event, err
	}
	if response := metadata.Response; response != nil {
		event.Response = &dto.OpenAIResponsesResponse{
			ID: response.ID, Model: response.Model, Status: response.Status,
			Error: response.Error, IncompleteDetails: response.IncompleteDetails, Usage: response.Usage,
		}
	}
	switch event.Type {
	case dto.ResponsesOutputTypeItemDone:
		var details struct {
			Item        *responsesUsageOutput `json:"item"`
			OutputIndex *int                  `json:"output_index"`
		}
		if err := common.Unmarshal(data, &details); err != nil {
			return event, err
		}
		event.OutputIndex = details.OutputIndex
		if details.Item != nil {
			item := details.Item.responsesOutput()
			event.Item = &item
		}
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		if event.Response == nil {
			return event, nil
		}
		var details struct {
			Response struct {
				Output []responsesUsageOutput `json:"output"`
			} `json:"response"`
		}
		if err := common.Unmarshal(data, &details); err != nil {
			return event, err
		}
		if details.Response.Output != nil {
			event.Response.Output = make([]dto.ResponsesOutput, len(details.Response.Output))
			for i, output := range details.Response.Output {
				// Retain the real image result: the counter hashes it for dedup,
				// including upstreams that omit both item and call IDs.
				event.Response.Output[i] = output.responsesOutput()
			}
		}
		usage := event.Response.Usage
		// ApplyResponsesUsage uses the native Responses output_tokens field;
		// a chat-style completion_tokens field alone still needs the fallback.
		if usage != nil && usage.OutputTokens != 0 {
			return event, nil
		}
		var text struct {
			Response struct {
				Output []struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := common.Unmarshal(data, &text); err != nil {
			return event, err
		}
		for i, output := range text.Response.Output {
			event.Response.Output[i].Role = output.Role
			for _, content := range output.Content {
				event.Response.Output[i].Content = append(event.Response.Output[i].Content, dto.ResponsesOutputContent{Type: content.Type, Text: content.Text})
			}
		}
	}
	return event, nil
}
