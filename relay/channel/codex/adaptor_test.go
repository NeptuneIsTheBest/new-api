package codex

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRequestURLAlphaSearch(t *testing.T) {
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:    constant.ChannelTypeCodex,
			ChannelBaseUrl: "https://chatgpt.com",
		},
		RelayMode: relayconstant.RelayModeAlphaSearch,
	}

	url, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://chatgpt.com/backend-api/codex/alpha/search", url)
}

func TestResponsesBetaHeader(t *testing.T) {
	const httpBeta = "responses=experimental"
	service.InitHttpClient()
	for _, tc := range []struct {
		name         string
		useWebSocket bool
		stream       bool
		overrides    map[string]any
		want         string
	}{
		{name: "WebSocket", useWebSocket: true, stream: true, want: "responses_websockets=2026-02-06"},
		{name: "WebSocket with channel override", useWebSocket: true, stream: true, overrides: map[string]any{"OpenAI-Beta": httpBeta}, want: httpBeta},
		{name: "HTTP", want: httpBeta},
		{name: "HTTP SSE", stream: true, want: httpBeta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, []string{tc.want}, r.Header.Values("OpenAI-Beta"))
				if tc.useWebSocket {
					upgrader := websocket.Upgrader{}
					conn, err := upgrader.Upgrade(w, r, nil)
					if assert.NoError(t, err) {
						_ = conn.Close()
					}
				}
			}))
			defer upstream.Close()

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				RelayMode:   relayconstant.RelayModeResponses,
				IsStream:    tc.stream,
				DisablePing: true,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:     constant.ChannelTypeCodex,
					ChannelBaseUrl:  upstream.URL,
					ApiKey:          `{"access_token":"test-token","account_id":"test-account"}`,
					HeadersOverride: tc.overrides,
				},
			}
			if tc.useWebSocket {
				conn, err := channel.DoWssRequest(&Adaptor{}, ctx, info, nil)
				require.NoError(t, err)
				_ = conn.Close()
				return
			}
			resp, err := channel.DoApiRequest(&Adaptor{}, ctx, info, nil)
			require.NoError(t, err)
			_ = resp.Body.Close()
		})
	}
}

// The Codex backend rejects these fields, so the adaptor clears them rather
// than forwarding what the client sent.
func TestConvertOpenAIResponsesRequestDropsPenalties(t *testing.T) {
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeCodex},
		RelayMode:   relayconstant.RelayModeResponses,
	}

	converted, err := adaptor.ConvertOpenAIResponsesRequest(nil, info, dto.OpenAIResponsesRequest{
		Model:            "gpt-5-codex",
		Input:            json.RawMessage(`"hello"`),
		MaxOutputTokens:  lo.ToPtr(uint(128)),
		Temperature:      lo.ToPtr(1.0),
		FrequencyPenalty: json.RawMessage(`1.5`),
		PresencePenalty:  json.RawMessage(`1.5`),
	})
	require.NoError(t, err)

	request, ok := converted.(dto.OpenAIResponsesRequest)
	require.True(t, ok)
	assert.Nil(t, request.MaxOutputTokens)
	assert.Nil(t, request.Temperature)
	assert.Nil(t, request.FrequencyPenalty)
	assert.Nil(t, request.PresencePenalty)
}
