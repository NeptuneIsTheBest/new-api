package channel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	channelconstant "github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_CodexPassthroughPassesContextHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		headersOverride map[string]any
	}{
		{
			name: "wildcard",
			headersOverride: map[string]any{
				"*": "",
			},
		},
		{
			name: "regex",
			headersOverride: map[string]any{
				"regex:.*": "",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx.Request.Header.Set("Originator", "Codex Desktop")
			ctx.Request.Header.Set("User-Agent", "Codex Desktop/0.142.4")
			ctx.Request.Header.Set("Session-Id", "sess-123")
			ctx.Request.Header.Set("Thread-Id", "thread-123")
			ctx.Request.Header.Set("X-Client-Request-Id", "request-123")
			ctx.Request.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
			ctx.Request.Header.Set("X-Codex-Turn-Metadata", `{"thread_source":"subagent"}`)
			ctx.Request.Header.Set("X-Codex-Window-Id", "thread-123:0")
			ctx.Request.Header.Set("X-Codex-Parent-Thread-Id", "parent-123")
			ctx.Request.Header.Set("X-OpenAI-Subagent", "guardian")
			ctx.Request.Header.Set("OpenAI-Beta", "client-beta")
			ctx.Request.Header.Set("X-OAI-Attestation", "client-attestation")
			ctx.Request.Header.Set("X-ResponsesAPI-Include-Timing-Metrics", "1")
			ctx.Request.Header.Set("Authorization", "Bearer client-token")
			ctx.Request.Header.Set("chatgpt-account-id", "client-account")
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("Accept", "text/event-stream")
			ctx.Request.Header.Set("Host", "malicious.example")
			ctx.Request.Header.Set("Content-Length", "123")

			info := &relaycommon.RelayInfo{
				IsChannelTest: false,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:     channelconstant.ChannelTypeCodex,
					HeadersOverride: tt.headersOverride,
				},
			}

			headers, err := processHeaderOverride(info, ctx)
			require.NoError(t, err)
			require.Equal(t, "Codex Desktop", headers["originator"])
			require.Equal(t, "Codex Desktop/0.142.4", headers["user-agent"])
			require.Equal(t, "sess-123", headers["session-id"])
			require.Equal(t, "thread-123", headers["thread-id"])
			require.Equal(t, "request-123", headers["x-client-request-id"])
			require.Equal(t, "remote_compaction_v2", headers["x-codex-beta-features"])
			require.Equal(t, `{"thread_source":"subagent"}`, headers["x-codex-turn-metadata"])
			require.Equal(t, "thread-123:0", headers["x-codex-window-id"])
			require.Equal(t, "parent-123", headers["x-codex-parent-thread-id"])
			require.Equal(t, "guardian", headers["x-openai-subagent"])

			for _, headerName := range []string{
				"authorization",
				"chatgpt-account-id",
				"content-type",
				"accept",
				"openai-beta",
				"x-oai-attestation",
				"x-responsesapi-include-timing-metrics",
				"host",
				"content-length",
			} {
				_, exists := headers[headerName]
				require.False(t, exists, "expected %s to be skipped", headerName)
			}
		})
	}
}

func TestProcessHeaderOverride_CodexExplicitOverrideCanSetProtocolHeaders(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("OpenAI-Beta", "client-beta")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: channelconstant.ChannelTypeCodex,
			HeadersOverride: map[string]any{
				"*":           "",
				"OpenAI-Beta": "admin-beta",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "admin-beta", headers["openai-beta"])
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}

func TestProcessHeaderOverride_ParamOverridePassthroughRespectsKeepOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		value      any
		keepOrigin bool
		expected   string
	}{
		{
			name:       "wildcard client header wins",
			value:      "*",
			keepOrigin: false,
			expected:   "client-trace",
		},
		{
			name:       "wildcard existing override wins",
			value:      "*",
			keepOrigin: true,
			expected:   "static-trace",
		},
		{
			name:       "regex client header wins",
			value:      "regex:(?i)^x-trace-id$",
			keepOrigin: false,
			expected:   "client-trace",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			ctx.Request.Header.Set("X-Trace-Id", "client-trace")

			info := &relaycommon.RelayInfo{
				IsChannelTest: false,
				ChannelMeta: &relaycommon.ChannelMeta{
					ParamOverride: map[string]any{
						"operations": []any{
							map[string]any{
								"mode":        "pass_headers",
								"value":       tt.value,
								"keep_origin": tt.keepOrigin,
							},
						},
					},
					HeadersOverride: map[string]any{
						"X-Trace-Id": "static-trace",
					},
				},
			}

			_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
			require.NoError(t, err)

			headers, err := processHeaderOverride(info, ctx)
			require.NoError(t, err)
			require.Equal(t, tt.expected, headers["x-trace-id"])
		})
	}
}

func TestProcessHeaderOverride_StaticWildcardKeepsExplicitOverridePriority(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "client-trace")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*":          "",
				"X-Trace-Id": "static-trace",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "static-trace", headers["x-trace-id"])
}
