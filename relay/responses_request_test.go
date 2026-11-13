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
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/requestplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesRequestPreservesImageOptions(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.PassThroughRequestEnabled
	t.Cleanup(func() { settings.PassThroughRequestEnabled = previous })
	settings.PassThroughRequestEnabled = false
	service.InitHttpClient()

	type upstreamRequest struct {
		body []byte
		path string
		err  error
	}
	received := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		received <- upstreamRequest{body: body, path: r.URL.Path, err: err}
		// Stop after request forwarding so this fixture needs no quota or database state.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"fixture upstream rejection","type":"invalid_request_error"}}`)
	}))
	t.Cleanup(upstream.Close)

	for _, tc := range []struct {
		name, image string
	}{
		{name: "omitted"},
		{name: "auto", image: `{"context_compression":"auto"}`},
		{name: "enabled", image: `{"context_compression":"enabled"}`},
		{name: "disabled", image: `{"context_compression":"disabled"}`},
		{name: "empty object", image: `{}`},
		{name: "null", image: `null`},
		{name: "extension", image: `{"context_compression":"enabled","vendor_option":{"enabled":false}}`},
	} {
		fields := `"model":"gpt-4.1","input":"hi"`
		if tc.image != "" {
			fields += `,"image":` + tc.image
		}
		for _, transport := range []string{"http", "ws-flat", "ws-wrapped", "compact"} {
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				path := "/v1/responses"
				if transport == "compact" {
					path += "/compact"
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader("{"+fields+"}"))
				c.Request.Header.Set("Content-Type", "application/json")
				t.Cleanup(func() { common.CleanupBodyStorage(c) })
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4.1")
				common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
				common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)

				var wire []byte
				switch transport {
				case "ws-flat", "ws-wrapped":
					message := `{"type":"response.create",` + fields + `}`
					if transport == "ws-wrapped" {
						message = `{"type":"response.create","response":{` + fields + `}}`
					}
					create, _, err := normalizeResponsesWSTestMessage([]byte(message))
					require.NoError(t, err)
					info := relaycommon.GenRelayInfoResponses(c, &create.Request)
					payload, apiErr := buildResponsesWSCreatePayload(c, info, create.Request, create.Generate, create.StreamID)
					require.Nil(t, apiErr)
					assert.Equal(t, "response.create", gjson.GetBytes(payload, "type").String())
					wire = payload
				default:
					var info *relaycommon.RelayInfo
					if transport == "compact" {
						request, err := helper.GetAndValidateResponsesCompactionRequest(c)
						require.NoError(t, err)
						info = relaycommon.GenRelayInfoResponsesCompaction(c, request)
					} else {
						request, err := helper.GetAndValidateResponsesRequest(c)
						require.NoError(t, err)
						info = relaycommon.GenRelayInfoResponses(c, request)
					}
					apiErr := ResponsesHelper(c, info)
					select {
					case request := <-received:
						require.NoError(t, request.err)
						assert.Equal(t, path, request.path)
						wire = request.body
					default:
						t.Fatal("request did not reach the upstream")
					}
					require.NotNil(t, apiErr)
					require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
					require.Contains(t, apiErr.Error(), "fixture upstream rejection")
				}

				image := gjson.GetBytes(wire, "image")
				if tc.image == "" {
					assert.False(t, image.Exists(), "omitted image options must not acquire a default")
					return
				}
				require.True(t, image.Exists(), "image options must reach the upstream")
				assert.JSONEq(t, tc.image, image.Raw)
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
