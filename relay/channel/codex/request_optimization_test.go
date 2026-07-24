package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrepareResponsesRequestDefaultsIncludeObfuscationToFalse(t *testing.T) {
	input := []byte(`{"model":"gpt-5","stream":true,"stream_options":{"include_usage":true}}`)
	info := &relaycommon.RelayInfo{
		IsStream:  true,
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: "https://codex-proxy.example.com",
		},
	}

	result, err := PrepareResponsesRequest(context.Background(), info, input)
	require.NoError(t, err)

	includeObfuscation := gjson.GetBytes(result, "stream_options.include_obfuscation")
	assert.True(t, includeObfuscation.Exists())
	assert.False(t, includeObfuscation.Bool())
	assert.True(t, gjson.GetBytes(result, "stream_options.include_usage").Bool())
	assert.Empty(t, info.UpstreamRequestContentEncoding)
}

func TestPrepareResponsesRequestAllowsIncludeObfuscationPassthrough(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "absent",
			input: []byte(`{"model":"gpt-5","stream":true}`),
		},
		{
			name:  "explicit false",
			input: []byte(`{"model":"gpt-5","stream":true,"stream_options":{"include_obfuscation":false}}`),
		},
		{
			name:  "explicit true",
			input: []byte(`{"model":"gpt-5","stream":true,"stream_options":{"include_obfuscation":true}}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				IsStream:  true,
				RelayMode: relayconstant.RelayModeResponses,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelBaseUrl: "https://codex-proxy.example.com",
					ChannelOtherSettings: dto.ChannelOtherSettings{
						AllowIncludeObfuscation: true,
					},
				},
			}

			result, err := PrepareResponsesRequest(context.Background(), info, test.input)
			require.NoError(t, err)
			assert.Equal(t, test.input, result)
		})
	}
}

func TestPrepareResponsesRequestLeavesNonStreamingAndCompactBodiesUnchanged(t *testing.T) {
	input := []byte(`{"model":"gpt-5","instructions":"` + strings.Repeat("repeatable ", 128) + `"}`)
	tests := []struct {
		name      string
		relayMode int
		isStream  bool
		baseURL   string
	}{
		{
			name:      "non-streaming custom endpoint",
			relayMode: relayconstant.RelayModeResponses,
			baseURL:   "https://codex-proxy.example.com",
		},
		{
			name:      "compact official endpoint",
			relayMode: relayconstant.RelayModeResponsesCompact,
			isStream:  true,
			baseURL:   "https://chatgpt.com",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				IsStream:                       test.isStream,
				RelayMode:                      test.relayMode,
				UpstreamRequestContentEncoding: "stale",
				ChannelMeta:                    &relaycommon.ChannelMeta{ChannelBaseUrl: test.baseURL},
			}

			result, err := PrepareResponsesRequest(context.Background(), info, input)
			require.NoError(t, err)
			assert.Equal(t, input, result)
			assert.Empty(t, info.UpstreamRequestContentEncoding)
		})
	}
}

func TestPrepareResponsesRequestCompressesOfficialChatGPTEndpoint(t *testing.T) {
	input := []byte(`{"model":"gpt-5","instructions":"` + strings.Repeat("compress this request payload ", 256) + `"}`)
	info := &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: "https://chatgpt.com",
		},
	}

	result, err := PrepareResponsesRequest(context.Background(), info, input)
	require.NoError(t, err)
	require.Equal(t, "zstd", info.UpstreamRequestContentEncoding)
	assert.Less(t, len(result), len(input))

	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	t.Cleanup(decoder.Close)
	decoded, err := decoder.DecodeAll(result, nil)
	require.NoError(t, err)
	assert.Equal(t, input, decoded)
}

func TestPrepareResponsesRequestSkipsIneligibleOrNonBeneficialCompression(t *testing.T) {
	repetitiveInput := []byte(`{"model":"gpt-5","instructions":"` + strings.Repeat("compressible ", 128) + `"}`)
	tests := []struct {
		name    string
		baseURL string
		input   []byte
	}{
		{
			name:    "custom endpoint",
			baseURL: "https://codex-proxy.example.com",
			input:   repetitiveInput,
		},
		{
			name:    "non-TLS ChatGPT endpoint",
			baseURL: "http://chatgpt.com",
			input:   repetitiveInput,
		},
		{
			name:    "lookalike hostname",
			baseURL: "https://chatgpt.com.example.com",
			input:   repetitiveInput,
		},
		{
			name:    "compressed body is not smaller",
			baseURL: "https://chatgpt.com",
			input:   []byte(`{}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				RelayMode: relayconstant.RelayModeResponses,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelBaseUrl: test.baseURL,
				},
			}

			result, err := PrepareResponsesRequest(context.Background(), info, test.input)
			require.NoError(t, err)
			assert.Equal(t, test.input, result)
			assert.Empty(t, info.UpstreamRequestContentEncoding)
		})
	}
}
