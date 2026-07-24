package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoInitChannelMetaResetsUpstreamBodyMetadata(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &RelayInfo{
		UpstreamRequestBodySize:        128,
		UpstreamRequestContentEncoding: "zstd",
	}
	info.InitChannelMeta(ctx)
	require.NotNil(t, info.ChannelMeta)
	assert.Zero(t, info.UpstreamRequestBodySize)
	assert.Empty(t, info.UpstreamRequestContentEncoding)

	info.UpstreamRequestBodySize = 64
	info.UpstreamRequestContentEncoding = "gzip"
	info.InitChannelMeta(ctx)
	assert.Zero(t, info.UpstreamRequestBodySize)
	assert.Empty(t, info.UpstreamRequestContentEncoding)
}

func TestRelayInfoGetFinalRequestRelayFormatPrefersExplicitFinal(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		RequestConversionChain:  []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		FinalRequestRelayFormat: types.RelayFormatOpenAIResponses,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToConversionChain(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToRelayFormat(t *testing.T) {
	info := &RelayInfo{
		RelayFormat: types.RelayFormatGemini,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatGemini), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatNilReceiver(t *testing.T) {
	var info *RelayInfo
	require.Equal(t, types.RelayFormat(""), info.GetFinalRequestRelayFormat())
}

func TestNormalizeRequestURLPath(t *testing.T) {
	tests := []struct {
		name           string
		requestURLPath string
		want           string
	}{
		{
			name:           "playground chat completions path",
			requestURLPath: "/pg/chat/completions",
			want:           "/v1/chat/completions",
		},
		{
			name:           "playground path with query",
			requestURLPath: "/pg/chat/completions?client=playground",
			want:           "/v1/chat/completions?client=playground",
		},
		{
			name:           "regular relay path",
			requestURLPath: "/v1/chat/completions",
			want:           "/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, NormalizeRequestURLPath(tt.requestURLPath))
		})
	}
}
