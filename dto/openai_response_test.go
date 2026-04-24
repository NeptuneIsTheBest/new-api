package dto

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestResponsesStreamResponseAllowsObjectArgumentsForImageGenerationCall(t *testing.T) {
	raw := []byte(`{
		"type":"response.output_item.done",
		"item":{
			"id":"ig_123",
			"type":"image_generation_call",
			"status":"completed",
			"arguments":{"prompt":"a cat","size":"1024x1024"}
		}
	}`)

	var resp ResponsesStreamResponse
	require.NoError(t, common.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Item)
	require.Equal(t, ResponsesOutputTypeImageGenerationCall, resp.Item.Type)
	require.JSONEq(t, `{"prompt":"a cat","size":"1024x1024"}`, resp.Item.ArgumentsString())
}

func TestResponsesOutputArgumentsStringUnquotesFunctionCallArguments(t *testing.T) {
	raw := []byte(`{
		"type":"response.output_item.done",
		"item":{
			"id":"fc_123",
			"type":"function_call",
			"call_id":"call_123",
			"name":"lookup",
			"arguments":"{\"city\":\"Paris\",\"limit\":0}"
		}
	}`)

	var resp ResponsesStreamResponse
	require.NoError(t, common.Unmarshal(raw, &resp))
	require.NotNil(t, resp.Item)
	require.Equal(t, `{"city":"Paris","limit":0}`, resp.Item.ArgumentsString())
}

func TestOpenAIResponsesResponseAllowsImageGenerationCallArgumentsObject(t *testing.T) {
	raw := []byte(`{
		"id":"resp_123",
		"object":"response",
		"created_at":1710000000,
		"model":"gpt-image-1",
		"output":[{
			"id":"ig_123",
			"type":"image_generation_call",
			"status":"completed",
			"quality":"high",
			"size":"1024x1024",
			"arguments":{"prompt":"a cat","quality":"high"}
		}]
	}`)

	var resp OpenAIResponsesResponse
	require.NoError(t, common.Unmarshal(raw, &resp))
	require.True(t, resp.HasImageGenerationCall())
	require.Equal(t, "high", resp.GetQuality())
	require.Equal(t, "1024x1024", resp.GetSize())
	require.JSONEq(t, `{"prompt":"a cat","quality":"high"}`, resp.Output[0].ArgumentsString())
}
