package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/requestplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Return an upstream error after capturing the request so forwarding tests
// exercise ResponsesHelper without entering quota settlement.
func newResponsesImageCaptureUpstream(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case captured <- body:
		default:
			http.Error(w, "unexpected additional upstream request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"request_captured","message":"Captured request"}}`)
	}))
	t.Cleanup(server.Close)
	return server.URL, captured
}

func TestResponsesImageContextCompressionForwarding(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.PassThroughRequestEnabled
	t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })
	upstream, captured := newResponsesImageCaptureUpstream(t)
	for _, transport := range []string{"http", "compact", "websocket flat", "websocket wrapped"} {
		for _, tc := range []struct {
			name, image, override, want           string
			passthrough, globalPassthrough, codex bool
		}{
			{name: "auto", image: `{"context_compression":"auto"}`},
			{name: "enabled", image: `{"context_compression":"enabled"}`},
			{name: "disabled", image: `{"context_compression":"disabled"}`},
			{name: "omitted"},
			{name: "null", image: `null`},
			{name: "empty", image: `{}`},
			{name: "extension", image: `{"context_compression":"enabled","future":{"flag":false}}`},
			{name: "upstream validation", image: `{"context_compression":"future_policy"}`},
			{name: "channel passthrough", image: `{"context_compression":"disabled"}`, passthrough: true},
			{name: "global passthrough", image: `{"context_compression":"enabled"}`, globalPassthrough: true},
			{name: "override", image: `{"context_compression":"disabled","future":true}`, override: "enabled", want: `{"context_compression":"enabled","future":true}`},
			{name: "codex", image: `{"context_compression":"enabled"}`, codex: true},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				settings.PassThroughRequestEnabled = tc.globalPassthrough
				raw := `{"model":"gpt-test","input":"hello"`
				if tc.image != "" {
					raw += `,"image":` + tc.image
				}
				raw += `}`
				path := "/v1/responses"
				var format types.RelayFormat = types.RelayFormatOpenAIResponses
				if transport == "compact" {
					path += "/compact"
					format = types.RelayFormatOpenAIResponsesCompaction
				}
				var create responsesWSCreateRequest
				isWebSocket := strings.HasPrefix(transport, "websocket")
				if isWebSocket {
					event := `{"type":"response.create",` + raw[1:]
					if transport == "websocket wrapped" {
						event = `{"type":"response.create","response":` + raw + `}`
					}
					var err error
					create, _, err = normalizeResponsesWSTestMessage([]byte(event))
					require.NoError(t, err)
					raw = string(create.Body)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
				c.Request.Header.Set("Content-Type", "application/json")
				t.Cleanup(func() { common.CleanupBodyStorage(c) })
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")
				channelType := constant.ChannelTypeOpenAI
				if tc.codex {
					channelType = constant.ChannelTypeCodex
					common.SetContextKey(c, constant.ContextKeyChannelKey, `{"access_token":"test-access-token","account_id":"test-account"}`)
				}
				common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream)
				common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: tc.passthrough})
				if tc.override != "" {
					common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{
						"operations": []any{map[string]any{"path": "image.context_compression", "mode": "set", "value": tc.override}},
					})
				}
				var wire []byte
				if isWebSocket {
					info := relaycommon.GenRelayInfoResponses(c, &create.Request)
					var apiErr *types.NewAPIError
					wire, apiErr = buildResponsesWSCreatePayload(c, info, create.Request, create.Generate, "image-test")
					require.Nil(t, apiErr)
					assert.Equal(t, "response.create", gjson.GetBytes(wire, "type").String())
					assert.Equal(t, "image-test", gjson.GetBytes(wire, "stream_id").String())
				} else {
					req, err := helper.GetAndValidateRequest(c, format)
					require.NoError(t, err)
					info, err := relaycommon.GenRelayInfo(c, format, req, nil)
					require.NoError(t, err)
					apiErr := ResponsesHelper(c, info)
					require.NotNil(t, apiErr)
					require.Equal(t, "request_captured", string(apiErr.GetErrorCode()), "%v", apiErr)
					select {
					case wire = <-captured:
					default:
						t.Fatal("upstream did not receive the request")
					}
				}
				want := tc.image
				if tc.override != "" {
					want = tc.want
				}
				if want == "" {
					assert.False(t, gjson.GetBytes(wire, "image").Exists())
				} else {
					assert.JSONEq(t, want, gjson.GetBytes(wire, "image").Raw)
				}
			})
		}
	}
}

func TestResponsesImageContextCompressionRejectsCrossProtocol(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.PassThroughRequestEnabled
	t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })
	settings.PassThroughRequestEnabled = false
	upstream, captured := newResponsesImageCaptureUpstream(t)
	for _, target := range []struct {
		format      types.RelayFormat
		channelType int
	}{
		{types.RelayFormatOpenAI, constant.ChannelTypeAdvancedCustom},
		{types.RelayFormatClaude, constant.ChannelTypeAnthropic},
		{types.RelayFormatGemini, constant.ChannelTypeGemini},
	} {
		for _, image := range []string{"", `null`, `{}`, `{"context_compression":"auto"}`, `{"context_compression":"enabled"}`, `{"context_compression":"disabled"}`} {
			t.Run(string(target.format)+"/"+image, func(t *testing.T) {
				raw := `{"model":"gpt-test","input":"hello"`
				if image != "" {
					raw += `,"image":` + image
				}
				raw += `}`
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
				c.Request.Header.Set("Content-Type", "application/json")
				t.Cleanup(func() { common.CleanupBodyStorage(c) })
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")
				common.SetContextKey(c, constant.ContextKeyChannelType, target.channelType)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream)
				common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
					ToolLossPolicy: string(types.ConversionLossPolicyAllow),
					AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
						IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: relayconvert.ConverterOpenAIResponsesToOpenAIChat,
					}}},
				})
				req, err := helper.GetAndValidateResponsesRequest(c)
				require.NoError(t, err)
				info := relaycommon.GenRelayInfoResponses(c, req)
				apiErr := ResponsesHelper(c, info)
				require.NotNil(t, apiErr)
				if image == "" || image == "null" {
					require.Equal(t, "request_captured", string(apiErr.GetErrorCode()))
					select {
					case wire := <-captured:
						assert.False(t, gjson.GetBytes(wire, "image").Exists())
					default:
						t.Fatal("upstream did not receive the converted request")
					}
					return
				}
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.Equal(t, types.ErrorCodeConvertRequestFailed, apiErr.GetErrorCode())
				assert.True(t, types.IsSkipRetryError(apiErr))
				var loss *types.ConversionLossError
				require.ErrorAs(t, apiErr, &loss)
				require.Len(t, loss.Diagnostics, 1)
				assert.Equal(t, "image", loss.Diagnostics[0].Path)
				assert.Equal(t, types.RelayFormatOpenAIResponses, string(loss.Diagnostics[0].From))
				assert.Equal(t, target.format, loss.Diagnostics[0].To)
				assert.Empty(t, captured, "rejected image options must not reach upstream")
			})
		}
	}
}

func TestResponsesImageContextCompressionResponseForwarding(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, mode := range []string{"json", "stream", "compact"} {
		for _, policy := range []string{"enabled", "disabled"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				object := "response"
				if mode == "compact" {
					object = "response.compaction"
				}
				body := `{"id":"resp_1","object":"` + object + `","model":"gpt-test","status":"completed","image":{"context_compression":"` + policy + `","future":true},"output":[],"usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25}}`
				contentType, want := "application/json", body
				if mode == "stream" {
					event := `{"type":"response.completed","response":` + body + `}`
					body = "data: " + event + "\n\ndata: [DONE]\n\n"
					want = "event: response.completed\ndata: " + event + "\n\n"
					contentType = "text/event-stream"
				}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				info := &relaycommon.RelayInfo{
					RelayFormat: types.RelayFormatOpenAIResponses,
					IsStream:    mode == "stream",
					DisablePing: true,
					ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-test"},
				}
				resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{contentType}}}
				var usage *dto.Usage
				var apiErr *types.NewAPIError
				switch mode {
				case "stream":
					usage, apiErr = openaichannel.OaiResponsesStreamHandler(c, info, resp)
				case "compact":
					usage, apiErr = openaichannel.OaiResponsesCompactionHandler(c, resp)
				default:
					usage, apiErr = openaichannel.OaiResponsesHandler(c, info, resp)
				}
				require.Nil(t, apiErr)
				assert.Equal(t, want, w.Body.String())
				require.NotNil(t, usage)
				assert.Equal(t, 20, usage.PromptTokens)
				assert.Equal(t, 5, usage.CompletionTokens)
				assert.Equal(t, 25, usage.TotalTokens)
			})
		}
	}
}

func TestPrepareResponsesRequestDeepSeekEffectiveReasoning(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.PassThroughRequestEnabled
	t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })
	settings.PassThroughRequestEnabled = false
	const input = `[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}]`
	for _, tc := range []struct {
		name, effort, override, endpoint         string
		passthrough, replaceInput, wantReasoning bool
		globalPassthrough                        bool
	}{
		{name: "override enables thinking", effort: "none", override: "high", wantReasoning: true},
		{name: "override disables thinking", effort: "high", override: "none"},
		{name: "override replaces history", effort: "high", override: "high", replaceInput: true, wantReasoning: true},
		{name: "no override", effort: "high", wantReasoning: true},
		{name: "thinking disabled", effort: "none"},
		{name: "third party endpoint", effort: "none", override: "high", endpoint: "https://other.example"},
		{name: "passthrough bypasses overrides and normalization", effort: "none", override: "high", passthrough: true},
		{name: "global passthrough bypasses normalization", effort: "high", globalPassthrough: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings.PassThroughRequestEnabled = tc.globalPassthrough
			endpoint := tc.endpoint
			if endpoint == "" {
				endpoint = "https://API.DEEPSEEK.COM:443/v1"
			}
			raw := `{"model":"deepseek-flash","reasoning":{"effort":"` + tc.effort + `"},"input":` + input + `}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			c.Request.Header.Set("Content-Type", "application/json")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "deepseek-flash")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeDeepSeek)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, endpoint)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: tc.passthrough})
			if tc.override != "" {
				ops := []any{map[string]any{"path": "reasoning.effort", "mode": "set", "value": tc.override}, map[string]any{"path": "vendor_extension", "mode": "set", "value": "preserved"}}
				if tc.replaceInput {
					var replacement any
					require.NoError(t, common.Unmarshal([]byte(input), &replacement))
					ops = append(ops, map[string]any{"path": "input", "mode": "set", "value": replacement})
				}
				common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{"operations": ops})
			}
			var req dto.OpenAIResponsesRequest
			require.NoError(t, common.Unmarshal([]byte(raw), &req))
			info := relaycommon.GenRelayInfoResponses(c, &req)
			_, body, closer, apiErr := PrepareResponsesRequest(c, info, &req)
			require.Nil(t, apiErr)
			t.Cleanup(func() { require.NoError(t, closer.Close()) })
			wire, err := io.ReadAll(body)
			require.NoError(t, err)
			wsInfo := relaycommon.GenRelayInfoResponses(c, &req)
			event, wsErr := buildResponsesWSCreatePayload(c, wsInfo, req, nil, "deepseek-test")
			require.Nil(t, wsErr)
			assert.Equal(t, "response.create", gjson.GetBytes(event, "type").String())
			assert.Equal(t, "deepseek-test", gjson.GetBytes(event, "stream_id").String())
			assert.JSONEq(t, gjson.GetBytes(wire, "input").Raw, gjson.GetBytes(event, "input").Raw)
			assert.JSONEq(t, gjson.GetBytes(wire, "reasoning").Raw, gjson.GetBytes(event, "reasoning").Raw)
			assert.JSONEq(t, input, string(req.Input))
			assert.Equal(t, tc.effort, req.Reasoning.Effort)
			if tc.passthrough || tc.globalPassthrough {
				assert.JSONEq(t, raw, string(wire))
				return
			}
			wantEffort := tc.effort
			if tc.override != "" {
				wantEffort = tc.override
				assert.Equal(t, "preserved", gjson.GetBytes(wire, "vendor_extension").String())
			}
			assert.Equal(t, wantEffort, gjson.GetBytes(wire, "reasoning.effort").String())
			if tc.wantReasoning {
				assert.Equal(t, "reasoning", gjson.GetBytes(wire, "input.0.type").String())
				assert.Equal(t, " ", gjson.GetBytes(wire, "input.0.content.0.text").String())
				assert.Equal(t, "function_call", gjson.GetBytes(wire, "input.1.type").String())
			} else {
				assert.JSONEq(t, input, gjson.GetBytes(wire, "input").Raw)
			}
		})
	}
}

// finalizedEgressPlugin rejects a body whose first input item is reasoning,
// which only the DeepSeek finalizer adds to the test request.
const finalizedEgressPlugin = `export const meta = {apiVersion: 1, key: "rp-finalized-egress", name: "Finalized egress", version: "1.0.0", author: {name: "Test"}};
export function egress(ctx, req) {
  const input = req.body && req.body.input;
  if (Array.isArray(input) && input.length && input[0].type === "reasoning") {
    return {action: "reject", code: "finalized.seen", message: "Egress saw the finalized body"};
  }
  return {action: "continue"};
}`

func TestPrepareResponsesRequestEgressReadsTheFinalizedBody(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.PassThroughRequestEnabled
	t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })
	settings.PassThroughRequestEnabled = false
	require.NoError(t, i18n.Init())
	plugin, err := jsplugin.DefaultRegistry.Register(finalizedEgressPlugin, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = jsplugin.DefaultRegistry.Unregister(plugin.Meta.Key) })
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: `[{"id":"fe","name":"Finalized","plugin":"rp-finalized-egress","enabled":true,"global":true}]`}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: "[]"}))
	})

	raw := `{"model":"deepseek-flash","reasoning":{"effort":"high"},"input":[{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}]}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "deepseek-flash")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeDeepSeek)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://api.deepseek.com")
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	_, _, apiErr := requestplugin.Ingress(c, types.RelayFormatOpenAIResponses)
	require.Nil(t, apiErr)
	var req dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(raw), &req))
	info := relaycommon.GenRelayInfoResponses(c, &req)
	_, _, closer, apiErr := PrepareResponsesRequest(c, info, &req)
	if closer != nil {
		t.Cleanup(func() { _ = closer.Close() })
	}
	require.NotNil(t, apiErr, "egress must read the reasoning item the finalizer added")
	assert.Equal(t, types.ErrorCodeRequestRejected, apiErr.GetErrorCode())
}
