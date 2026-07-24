package codex

import (
	"context"
	"net/url"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/sjson"
)

var getRequestEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
})

// PrepareResponsesRequest applies Codex-specific bandwidth optimizations to a
// marshaled outbound Responses request. Callers must invoke it only for bodies
// produced by the relay, never for pass-through request bodies.
func PrepareResponsesRequest(ctx context.Context, info *relaycommon.RelayInfo, jsonData []byte) ([]byte, error) {
	if info == nil {
		return jsonData, nil
	}
	info.UpstreamRequestContentEncoding = ""
	if info.RelayMode != relayconstant.RelayModeResponses {
		return jsonData, nil
	}

	var err error
	if info.IsStream && !info.ChannelOtherSettings.AllowIncludeObfuscation {
		jsonData, err = sjson.SetBytes(jsonData, "stream_options.include_obfuscation", false)
		if err != nil {
			return nil, err
		}
	}

	if !isOfficialChatGPTEndpoint(info.ChannelBaseUrl) {
		return jsonData, nil
	}

	encoder, err := getRequestEncoder()
	if err != nil {
		return nil, err
	}
	compressed := encoder.EncodeAll(jsonData, nil)
	useCompressed := len(compressed) < len(jsonData)
	logger.LogDebug(ctx, "codex request compression: original_bytes=%d compressed_bytes=%d applied=%t", len(jsonData), len(compressed), useCompressed)
	if !useCompressed {
		return jsonData, nil
	}

	info.UpstreamRequestContentEncoding = "zstd"
	return compressed, nil
}

func isOfficialChatGPTEndpoint(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	return host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}
