package service

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func buildChannelForSelectTest(id int, channelType int, priority int64, weight uint) *model.Channel {
	return &model.Channel{
		Id:       id,
		Type:     channelType,
		Priority: &priority,
		Weight:   &weight,
	}
}

func TestFilterChannelsForRetry_OnlyOtherCodexChannels(t *testing.T) {
	codexType := constant.ChannelTypeCodex
	param := &RetryParam{
		OnlyChannelType: &codexType,
		ExcludeChannelIDs: map[int]struct{}{
			1: {},
		},
	}

	filtered := filterChannelsForRetry([]*model.Channel{
		buildChannelForSelectTest(1, constant.ChannelTypeCodex, 10, 1),
		buildChannelForSelectTest(2, constant.ChannelTypeOpenAI, 10, 1),
		buildChannelForSelectTest(3, constant.ChannelTypeCodex, 10, 1),
	}, param)

	require.Len(t, filtered, 1)
	require.Equal(t, 3, filtered[0].Id)
}

func TestPickWeightedChannelFromHighestPriority_NeverFallsThroughLowerPriority(t *testing.T) {
	channels := []*model.Channel{
		buildChannelForSelectTest(1, constant.ChannelTypeCodex, 20, 1),
		buildChannelForSelectTest(2, constant.ChannelTypeCodex, 20, 1),
		buildChannelForSelectTest(3, constant.ChannelTypeCodex, 10, 100),
	}

	for i := 0; i < 64; i++ {
		selected := pickWeightedChannelFromHighestPriority(channels)
		require.NotNil(t, selected)
		require.Contains(t, []int{1, 2}, selected.Id)
	}
}
