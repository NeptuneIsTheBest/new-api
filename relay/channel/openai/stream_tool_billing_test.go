package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveStreamChoicesDedupesFunctionCallNames(t *testing.T) {
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	seen := make(map[string]struct{})
	var names []string

	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
	}
	for _, chunk := range chunks {
		parsed := parseChatStreamChunk(chunk)
		require.NoError(t, parsed.err)
		observeStreamChoices(info, parsed.response, seen, &names)
	}

	require.Len(t, names, 2)
	assert.Equal(t, []string{"get_weather", "get_time"}, names)
	assert.Empty(t, info.StreamStatus.ResponseOutcome(), "tool call deltas carry no finish reason")

	finished := parseChatStreamChunk(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	require.NoError(t, finished.err)
	observeStreamChoices(info, finished.response, seen, &names)
	assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
	assert.False(t, info.PerformanceBusinessRejection)

	filtered := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	rejected := parseChatStreamChunk(`{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`)
	require.NoError(t, rejected.err)
	observeStreamChoices(filtered, rejected.response, map[string]struct{}{}, &names)
	assert.True(t, filtered.PerformanceBusinessRejection)
}

func TestOaiStreamHandlerPreservesWireAndUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	first := `{"id":"chat_1","model":"gpt-4o","vendor":{"keep":false},"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`
	usage := `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`
	malformedChoices := `{"choices":false,"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`
	for _, tc := range []struct {
		name         string
		chunks       []string
		includeUsage bool
		want         []string
		errors       int
	}{
		{"native unknown fields", []string{first, usage}, true, []string{first, usage}, 0},
		{"hidden final usage", []string{first, usage}, false, []string{first}, 0},
		{"penultimate usage and absent fields", []string{first, usage, `{}`}, true, []string{first, usage, `{}`}, 0},
		{"penultimate usage with malformed choices", []string{first, malformedChoices, `{}`}, true, []string{first, malformedChoices, `{}`}, 1},
		{"invalid JSON remains a soft error", []string{first, `{invalid`, usage}, true, []string{first, `{invalid`, usage}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
				OriginModelName: "gpt-4o", DisablePing: true, ShouldIncludeUsage: tc.includeUsage,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4o"},
			}
			wire := "data: " + strings.Join(tc.chunks, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}
			got, apiErr := OaiStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, got)
			assert.Equal(t, 2, got.PromptTokens)
			assert.Equal(t, 3, got.CompletionTokens)
			assert.Equal(t, 5, got.TotalTokens)
			assert.Equal(t, "data: "+strings.Join(tc.want, "\n\ndata: ")+"\n\ndata: [DONE]\n\n", recorder.Body.String())
			assert.True(t, recorder.Flushed)
			assert.Equal(t, tc.errors, info.StreamStatus.ErrorCount)
			assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
		})
	}
}

func TestParsedStreamFormatPreservesCaller(t *testing.T) {
	chunks := []string{
		`{"id":"chat_1","model":"gpt-4o","choices":[{"index":0,"delta":{"reasoning_content":"consider"}}]}`,
		`{"id":"chat_1","choices":[{"index":0,"delta":{"reasoning_content":" evidence","content":"answer"}}]}`,
		`{"id":"chat_1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`,
	}
	for _, tc := range []struct {
		name            string
		format          types.RelayFormat
		force, thinking bool
	}{
		{"native", types.RelayFormatOpenAI, false, false},
		{"forced", types.RelayFormatOpenAI, true, false},
		{"thinking", types.RelayFormatOpenAI, false, true},
		{"claude", types.RelayFormatClaude, false, false},
		{"gemini", types.RelayFormatGemini, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want []dto.ChatCompletionsStreamResponse
			var responses []dto.ChatCompletionsStreamResponse
			for _, chunk := range chunks {
				var original, response dto.ChatCompletionsStreamResponse
				require.NoError(t, common.UnmarshalJsonStr(chunk, &original))
				require.NoError(t, common.UnmarshalJsonStr(chunk, &response))
				want = append(want, original)
				responses = append(responses, response)
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{RelayFormat: tc.format, ChannelMeta: &relaycommon.ChannelMeta{}, ThinkingContentInfo: relaycommon.ThinkingContentInfo{IsFirstThinkingContent: true}}
			for index := range responses {
				require.NoError(t, HandleParsedStreamFormat(c, info, &responses[index], tc.force, tc.thinking))
			}
			assert.Equal(t, want, responses, "forwarding must not mutate events shared with accounting")
			if tc.thinking {
				assert.Contains(t, recorder.Body.String(), `\u003cthink\u003e\nconsider`)
				assert.Contains(t, recorder.Body.String(), `\n\u003c/think\u003e\n`)
				assert.NotContains(t, recorder.Body.String(), "reasoning_content")
			}
		})
	}
}
