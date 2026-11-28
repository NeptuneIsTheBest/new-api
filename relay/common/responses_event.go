package common

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// ResponsesPayload holds the response fields inspected by the relay. Tool
// declarations and other pass-through fields stay in the original payload.
type ResponsesPayload struct {
	CreatedAt         dto.IntValue           `json:"created_at"`
	Status            common.RawMessage      `json:"status"`
	Error             any                    `json:"error"`
	IncompleteDetails *dto.IncompleteDetails `json:"incomplete_details"`
	Model             string                 `json:"model"`
	Output            []dto.ResponsesOutput  `json:"output"`
	Usage             *dto.Usage             `json:"usage"`
}

// ResponsesStreamEvent is the shared view of native SSE and WebSocket events.
type ResponsesStreamEvent struct {
	Type        string               `json:"type"`
	Response    *ResponsesPayload    `json:"response"`
	Code        string               `json:"code"`
	Delta       string               `json:"delta"`
	Item        *dto.ResponsesOutput `json:"item"`
	OutputIndex *int                 `json:"output_index"`
}

// StreamResponse supplies the decoded fields to the Responses usage accumulator.
func (e *ResponsesStreamEvent) StreamResponse() dto.ResponsesStreamResponse {
	event := dto.ResponsesStreamResponse{
		Type:        e.Type,
		Code:        e.Code,
		Delta:       e.Delta,
		Item:        e.Item,
		OutputIndex: e.OutputIndex,
	}
	if r := e.Response; r != nil {
		event.Response = &dto.OpenAIResponsesResponse{
			CreatedAt:         r.CreatedAt,
			Status:            r.Status,
			Error:             r.Error,
			IncompleteDetails: r.IncompleteDetails,
			Model:             r.Model,
			Output:            r.Output,
			Usage:             r.Usage,
		}
	}
	return event
}
