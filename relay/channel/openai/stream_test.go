package openai

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOaiStreamHandlerEmptySSEEvents(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	const emptyEvent = "data:\n\n"
	const whitespaceEvent = "data: \t \n\n"
	const multilineEmptyEvent = "data:\ndata: \n\n"
	const firstPayload = " \t" + `{"id":"chat_1","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}` + " \t"
	const secondPayload = `{"id":"chat_1","model":"gpt-test","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`
	const usagePayload = `{"id":"chat_1","model":"gpt-test","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`

	for _, tc := range []struct {
		name     string
		before   string
		between  string
		after    string
		lastJSON string
	}{
		{name: "no empty events"},
		{name: "leading empty event", before: emptyEvent},
		{name: "between events", between: emptyEvent},
		{name: "consecutive empty events", between: emptyEvent + "data: \n\n"},
		{name: "trailing empty event", after: emptyEvent},
		{name: "consecutive trailing empty events", after: emptyEvent + "data: \n\n"},
		{name: "whitespace events", before: whitespaceEvent, between: whitespaceEvent, after: whitespaceEvent},
		{name: "multiline empty events", before: multilineEmptyEvent, between: multilineEmptyEvent, after: multilineEmptyEvent},
		{name: "usage before empty JSON object", between: emptyEvent, after: emptyEvent, lastJSON: `{}`},
		{name: "usage before empty choices", between: whitespaceEvent, after: multilineEmptyEvent, lastJSON: `{"choices":[]}`},
	} {
		for _, format := range []struct {
			name        string
			forceFormat bool
		}{
			{name: "passthrough"},
			{name: "force format", forceFormat: true},
		} {
			t.Run(tc.name+"/"+format.name, func(t *testing.T) {
				wantPayloads := []string{firstPayload, secondPayload, usagePayload}
				body := tc.before + "data: " + firstPayload + "\n\n" + tc.between +
					"data: " + secondPayload + "\n\ndata: " + usagePayload + "\n\n"
				if tc.lastJSON != "" {
					body += tc.between + "data: " + tc.lastJSON + "\n\n"
					wantPayloads = append(wantPayloads, tc.lastJSON)
				}
				body += tc.after + "data: [DONE]\n\n"

				c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
				info.RelayMode = relayconstant.RelayModeChatCompletions
				info.ChannelType = constant.ChannelTypeOpenAI
				info.ChannelSetting.ForceFormat = format.forceFormat

				usage, apiErr := OaiStreamHandler(c, info, resp)
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, 2, usage.PromptTokens)
				assert.Equal(t, 3, usage.CompletionTokens)
				assert.Equal(t, 5, usage.TotalTokens)
				assert.False(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
				require.NotNil(t, info.StreamStatus)
				assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
				assert.NoError(t, info.StreamStatus.EndError)
				assert.Zero(t, info.StreamStatus.ErrorCount)

				events := strings.Split(strings.TrimSuffix(recorder.Body.String(), "\n\n"), "\n\n")
				require.Len(t, events, len(wantPayloads)+1)
				assert.Equal(t, "data: [DONE]", events[len(wantPayloads)])
				for i, wantPayload := range wantPayloads {
					payload, ok := strings.CutPrefix(events[i], "data: ")
					require.True(t, ok)
					if !format.forceFormat {
						assert.Equal(t, wantPayload, payload)
						continue
					}
					var want, got dto.ChatCompletionsStreamResponse
					require.NoError(t, common.UnmarshalJsonStr(wantPayload, &want))
					require.NoError(t, common.UnmarshalJsonStr(payload, &got))
					assert.Equal(t, want, got)
				}
			})
		}
	}
}

func TestOaiResponsesStreamHandlersEmptySSEEvents(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	const emptyEvent = "data:\n\n"
	const whitespaceEvent = "data: \t \n\n"
	const multilineEmptyEvent = "data:\ndata: \n\n"
	const createdPayload = `{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":1710000000}}`
	const firstPayload = " \t" + `{"type":"response.output_text.delta","delta":"hello"}` + " \t"
	const secondPayload = `{"type":"response.output_text.delta","delta":" world"}`
	const completedPayload = `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`

	for _, tc := range []struct {
		name       string
		before     string
		between    string
		after      string
		wantErrors int
	}{
		{name: "no empty events"},
		{name: "leading empty event", before: emptyEvent},
		{name: "between events", between: emptyEvent},
		{name: "consecutive empty events", between: emptyEvent + "data: \n\n"},
		{name: "trailing empty event", after: emptyEvent},
		{name: "whitespace events", before: whitespaceEvent, between: whitespaceEvent, after: whitespaceEvent},
		{name: "multiline empty events", before: multilineEmptyEvent, between: multilineEmptyEvent, after: multilineEmptyEvent},
		{name: "invalid JSON before completion", between: "data: invalid JSON\n\n", wantErrors: 1},
	} {
		for _, format := range []types.RelayFormat{types.RelayFormatOpenAIResponses, types.RelayFormatOpenAI} {
			t.Run(tc.name+"/"+string(format), func(t *testing.T) {
				body := tc.before + "data: " + createdPayload + "\n\ndata: " + firstPayload + "\n\n" +
					tc.between + "data: " + secondPayload + "\n\ndata: " + completedPayload + "\n\n" +
					tc.after + "data: [DONE]\n\n"
				c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
				info.ChannelType = constant.ChannelTypeOpenAI
				info.RelayFormat = format
				info.RelayMode = relayconstant.RelayModeChatCompletions
				var usage *dto.Usage
				var apiErr *types.NewAPIError
				if format == types.RelayFormatOpenAIResponses {
					c.Request.URL.Path = "/v1/responses"
					info.RelayMode = relayconstant.RelayModeResponses
					usage, apiErr = OaiResponsesStreamHandler(c, info, resp)
				} else {
					usage, apiErr = OaiResponsesToChatStreamHandler(c, info, resp)
				}
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, 2, usage.PromptTokens)
				assert.Equal(t, 3, usage.CompletionTokens)
				assert.Equal(t, 5, usage.TotalTokens)
				require.NotNil(t, info.StreamStatus)
				assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
				assert.NoError(t, info.StreamStatus.EndError)
				assert.Equal(t, tc.wantErrors, info.StreamStatus.TotalErrorCount())

				wantOutcome, wantAction := perfmetrics.OutcomeSuccess, "success"
				if tc.wantErrors > 0 {
					wantOutcome, wantAction = perfmetrics.OutcomeFailure, "stop"
				}
				assert.Equal(t, wantOutcome, perfmetrics.ClassifyRelayOutcome(c.Request.Context(), info, apiErr))
				service.MarkRequestPolicySuccess(c, info.StreamStatus)
				policy := service.RequestPolicy(c)
				assert.Equal(t, tc.wantErrors == 0, policy.Successful)
				events := policy.Events()
				require.Len(t, events, 1)
				assert.Equal(t, wantAction, events[0].Decision.Action)

				if format == types.RelayFormatOpenAIResponses {
					assert.Equal(t, string(relaycommon.ResponseOutcomeCompleted), info.StreamStatus.ResponseOutcome())
					wantBody := "event: response.created\ndata: " + createdPayload + "\n\n" +
						"event: response.output_text.delta\ndata: " + firstPayload + "\n\n" +
						"event: response.output_text.delta\ndata: " + secondPayload + "\n\n" +
						"event: response.completed\ndata: " + completedPayload + "\n\n"
					assert.Equal(t, wantBody, recorder.Body.String())
					return
				}
				requireOrderedSubstrings(t, recorder.Body.String(),
					`"role":"assistant"`,
					`"content":"hello"`,
					`"content":" world"`,
					`"finish_reason":"stop"`,
					`"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5`,
					"data: [DONE]\n\n",
				)
			})
		}
	}
}
