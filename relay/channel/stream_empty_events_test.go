package channel_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/baidu"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/dify"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/QuantumNous/new-api/relay/channel/xai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamHandlersEmptySSEEvents(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	geminiFrames := []string{
		`{"modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`,
	}
	claudeFrames := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","model":"claude-test","role":"assistant","content":[],"usage":{"input_tokens":2,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	chatFrames := []string{
		`{"id":"chat_1","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
		`{"id":"chat_1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
	}

	for _, handler := range []struct {
		name        string
		mode        int
		format      types.RelayFormat
		handle      func(*gin.Context, *http.Response, *relaycommon.RelayInfo) (any, *types.NewAPIError)
		frames      []string
		terminal    string
		passthrough bool
		fatalJSON   bool
		failedEvent bool
	}{
		{
			name: "Gemini native", mode: relayconstant.RelayModeGemini, format: types.RelayFormatGemini,
			handle: (&gemini.Adaptor{}).DoResponse, frames: geminiFrames, terminal: `"finishReason":"STOP"`, fatalJSON: true,
		},
		{
			name: "Gemini to Chat", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI,
			handle: (&gemini.Adaptor{}).DoResponse, frames: geminiFrames, terminal: `"finish_reason":"stop"`, fatalJSON: true,
		},
		{
			name: "Gemini to Responses", mode: relayconstant.RelayModeResponses, format: types.RelayFormatOpenAIResponses,
			handle: (&gemini.Adaptor{}).DoResponse, frames: geminiFrames, terminal: "event: response.completed", failedEvent: true,
		},
		{
			name: "Claude native", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatClaude,
			handle: (&claude.Adaptor{}).DoResponse, frames: claudeFrames, terminal: "event: message_stop", fatalJSON: true,
		},
		{
			name: "Claude to Chat", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI,
			handle: (&claude.Adaptor{}).DoResponse, frames: claudeFrames, terminal: `"finish_reason":"stop"`, fatalJSON: true,
		},
		{
			name: "Claude to Responses", mode: relayconstant.RelayModeResponses, format: types.RelayFormatOpenAIResponses,
			handle: (&claude.Adaptor{}).DoResponse, frames: claudeFrames, terminal: "event: response.completed", failedEvent: true,
		},
		{
			name: "Chat to Responses", mode: relayconstant.RelayModeResponses, format: types.RelayFormatOpenAIResponses,
			handle: func(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
				return openai.OaiChatToResponsesStreamHandler(c, info, resp)
			},
			frames: chatFrames, terminal: "event: response.completed", failedEvent: true,
		},
		{
			name: "Baidu", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI,
			handle: (&baidu.Adaptor{}).DoResponse, terminal: `"finish_reason":"stop"`,
			frames: []string{
				`{"id":"baidu_1","result":"hello","sentence_id":0,"is_end":false}`,
				`{"id":"baidu_1","result":" world","sentence_id":1,"is_end":true,"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			},
		},
		{
			name: "Dify", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI,
			handle: (&dify.Adaptor{}).DoResponse, terminal: "data: [DONE]\n\n",
			frames: []string{
				`{"event":"message","conversation_id":"dify_1","answer":"hello"}`,
				`{"event":"message_end","metadata":{"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}`,
			},
		},
		{
			name: "xAI", mode: relayconstant.RelayModeChatCompletions, format: types.RelayFormatOpenAI,
			handle: (&xai.Adaptor{}).DoResponse, frames: chatFrames, terminal: `"finish_reason":"stop"`,
		},
		{
			name: "image", mode: relayconstant.RelayModeImagesGenerations, format: types.RelayFormatOpenAI,
			handle: (&openai.Adaptor{}).DoResponse, terminal: "data: [DONE]\n\n", passthrough: true,
			frames: []string{
				`{"type":"image_generation.partial_image","b64_json":"hello"}`,
				`{"type":"image_generation.completed","b64_json":"image","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"timings":{"cache_n":1}}`,
			},
		},
		{
			name: "audio", mode: relayconstant.RelayModeAudioSpeech, format: types.RelayFormatOpenAI,
			handle: (&openai.Adaptor{}).DoResponse, terminal: `"type":"speech.audio.done"`, passthrough: true,
			frames: []string{
				`{"type":"speech.audio.delta","audio":"hello"}`,
				`{"type":"speech.audio.done","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`,
			},
		},
	} {
		t.Run(handler.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				before  string
				between string
				after   string
				invalid bool
			}{
				{name: "no empty events"},
				{name: "leading", before: "data:\n\n"},
				{name: "between", between: "data:\n\n"},
				{name: "trailing", after: "data:\n\n"},
				{name: "consecutive", before: "data:\n\ndata: \n\n", between: "data:\n\ndata: \n\n", after: "data:\n\ndata: \n\n"},
				{name: "whitespace", before: "data: \t \n\n", between: "data: \t \n\n", after: "data: \t \n\n"},
				{name: "multiline empty", before: "data:\ndata: \n\n", between: "data:\ndata: \n\n", after: "data:\ndata: \n\n"},
				{name: "invalid JSON", between: "data: invalid JSON\n\n", invalid: true},
			} {
				if tc.invalid && handler.passthrough {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					var body strings.Builder
					body.WriteString(tc.before)
					for i, frame := range handler.frames {
						if i > 0 {
							// A single malformed event distinguishes soft errors from fatal stops.
							if !tc.invalid || i == 1 {
								body.WriteString(tc.between)
							}
						}
						body.WriteString("data:  \t" + frame + " \t\n\n")
					}
					body.WriteString(tc.after + "data: [DONE]\n\n")
					c, recorder, resp, info := newEmptyEventStreamContext(t, body.String())
					info.RelayMode, info.RelayFormat = handler.mode, handler.format
					if handler.mode == relayconstant.RelayModeImagesGenerations {
						info.PriceData.UsePrice = true
						info.PriceData.AddOtherRatio("n", 2)
					}

					value, apiErr := handler.handle(c, resp, info)
					require.NotNil(t, info.StreamStatus)
					if tc.invalid {
						if handler.fatalJSON {
							require.NotNil(t, apiErr)
						} else {
							require.Nil(t, apiErr)
						}
						if handler.failedEvent {
							assert.Contains(t, recorder.Body.String(), "event: response.failed")
						} else {
							assert.Equal(t, 1, info.StreamStatus.TotalErrorCount())
						}
						if handler.fatalJSON || handler.failedEvent {
							assert.NotContains(t, recorder.Body.String(), handler.terminal)
							return
						}
					} else {
						require.Nil(t, apiErr)
						assert.Zero(t, info.StreamStatus.TotalErrorCount())
						assert.NoError(t, info.StreamStatus.EndError)
					}
					usage, ok := value.(*dto.Usage)
					require.True(t, ok)
					require.NotNil(t, usage)
					assert.Equal(t, 2, usage.PromptTokens)
					assert.Equal(t, 3, usage.CompletionTokens)
					assert.Equal(t, 5, usage.TotalTokens)
					assert.True(t, info.StreamStatus.IsNormalEnd())
					out := recorder.Body.String()
					assert.Contains(t, out, "hello")
					assert.Contains(t, out, handler.terminal)
					assert.NotContains(t, out, "event: response.failed")
					if handler.passthrough {
						want := strings.ReplaceAll(body.String(), "data:\n", "data: \n")
						if handler.mode == relayconstant.RelayModeImagesGenerations {
							want = strings.ReplaceAll(want, "data:  \t"+handler.frames[0], "event: image_generation.partial_image\ndata:  \t"+handler.frames[0])
							want = strings.ReplaceAll(want, "data:  \t"+handler.frames[1], "event: image_generation.completed\ndata:  \t"+handler.frames[1])
							assert.Equal(t, 1, usage.PromptTokensDetails.CachedTokens)
							assert.Equal(t, 1.0, info.PriceData.OtherRatios()["n"])
						} else {
							want = strings.TrimSuffix(want, "data: [DONE]\n\n")
						}
						assert.Equal(t, want, out)
					} else {
						assert.NotContains(t, out, "data: \n\n")
						assert.NotContains(t, out, "data: \t \n\n")
					}
				})
			}
		})
	}
}

type emptyEventFlushErrorWriter struct {
	gin.ResponseWriter
}

func (w *emptyEventFlushErrorWriter) FlushError() error {
	return io.ErrClosedPipe
}

func TestEmptySSEPassthroughWriteFailure(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name string
		mode int
	}{
		{name: "image", mode: relayconstant.RelayModeImagesGenerations},
		{name: "audio", mode: relayconstant.RelayModeAudioSpeech},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, resp, info := newEmptyEventStreamContext(t, "data:\n\n")
			info.RelayMode = tc.mode
			c.Writer = &emptyEventFlushErrorWriter{ResponseWriter: c.Writer}

			_, apiErr := (&openai.Adaptor{}).DoResponse(c, resp, info)
			require.Nil(t, apiErr)
			require.NotNil(t, info.StreamStatus)
			assert.Equal(t, 1, info.StreamStatus.TotalErrorCount())
			require.Len(t, info.StreamStatus.Errors, 1)
			assert.Contains(t, info.StreamStatus.Errors[0].Message, io.ErrClosedPipe.Error())
			assert.Equal(t, "data: \n\n", recorder.Body.String())
		})
	}
}

func newEmptyEventStreamContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(common.RequestIdKey, "empty-event-test")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta:        &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-test"},
		IsStream:           true,
		DisablePing:        true,
		ShouldIncludeUsage: true,
	}
	return c, recorder, resp, info
}
