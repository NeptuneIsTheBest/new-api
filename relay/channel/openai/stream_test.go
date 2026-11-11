package openai

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
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
