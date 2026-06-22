package middleware

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestChannelSupportsRequestPathMatchesNormalizedPlaygroundPath(t *testing.T) {
	channel := &model.Channel{
		Type: constant.ChannelTypeAdvancedCustom,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{
				{
					IncomingPath: "/v1/chat/completions",
					UpstreamPath: "https://upstream.example/v1/chat/completions",
					Converter:    dto.AdvancedCustomConverterNone,
				},
			},
		},
	})

	require.False(t, channelSupportsRequestPath(channel, "/pg/chat/completions"))
	require.True(t, channelSupportsRequestPath(channel, relaycommon.NormalizeRequestURLPath("/pg/chat/completions")))
}
