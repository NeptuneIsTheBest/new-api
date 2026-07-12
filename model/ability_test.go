package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertAbilitySelectionCandidate(
	t *testing.T,
	channelID int,
	channelType int,
	priority int64,
	routePath string,
	routeModels ...string,
) {
	t.Helper()

	channel := &Channel{
		Id:     channelID,
		Type:   channelType,
		Key:    fmt.Sprintf("key-%d", channelID),
		Status: common.ChannelStatusEnabled,
		Name:   fmt.Sprintf("channel-%d", channelID),
		Models: "selection-model",
		Group:  "default",
	}
	if routePath != "" {
		channel.SetOtherSettings(dto.ChannelOtherSettings{
			AdvancedCustom: &dto.AdvancedCustomConfig{
				Routes: []dto.AdvancedCustomRoute{
					{
						IncomingPath: routePath,
						UpstreamPath: "/upstream",
						Converter:    relayconvert.ConverterNone,
						Models:       routeModels,
					},
				},
			},
		})
	}
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, DB.Create(&Ability{
		Group:     "default",
		Model:     "selection-model",
		ChannelId: channelID,
		Enabled:   true,
		Priority:  &priority,
		Weight:    0,
	}).Error)
}

func TestGetRandomSatisfiedChannelDBFiltersRequestPathBeforePriority(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() {
		clearPreferredOwnerTables(t)
	})
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	insertAbilitySelectionCandidate(t, 1, constant.ChannelTypeAdvancedCustom, 10, "/v1/messages")
	insertAbilitySelectionCandidate(t, 2, constant.ChannelTypeAdvancedCustom, 1, "/v1/chat/completions")

	channel, err := GetRandomSatisfiedChannel("default", "selection-model", 0, "/v1/chat/completions")
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 2, channel.Id)
}

func TestGetRandomSatisfiedChannelDBAppliesRetryAfterRequestPathFilter(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() {
		clearPreferredOwnerTables(t)
	})
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	insertAbilitySelectionCandidate(t, 1, constant.ChannelTypeAdvancedCustom, 10, "/v1/messages")
	insertAbilitySelectionCandidate(t, 2, constant.ChannelTypeAdvancedCustom, 5, "/v1/chat/completions")
	insertAbilitySelectionCandidate(t, 3, constant.ChannelTypeAdvancedCustom, 1, "/v1/chat/completions")

	channel, err := GetRandomSatisfiedChannel("default", "selection-model", 1, "/v1/chat/completions")
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3, channel.Id)
}

func TestGetRandomSatisfiedChannelDBFiltersRequestModelBeforePriority(t *testing.T) {
	clearPreferredOwnerTables(t)
	t.Cleanup(func() {
		clearPreferredOwnerTables(t)
	})
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	insertAbilitySelectionCandidate(t, 1, constant.ChannelTypeAdvancedCustom, 10, "/v1/chat/completions", "other-model")
	insertAbilitySelectionCandidate(t, 2, constant.ChannelTypeAdvancedCustom, 1, "/v1/chat/completions", "selection-model")

	channel, err := GetRandomSatisfiedChannel("default", "selection-model", 0, "/v1/chat/completions")
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 2, channel.Id)
}

func TestListSatisfiedChannelsFiltersRequestModelWithAndWithoutMemoryCache(t *testing.T) {
	for _, memoryCacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory_cache_%t", memoryCacheEnabled), func(t *testing.T) {
			clearPreferredOwnerTables(t)
			previousMemoryCacheEnabled := common.MemoryCacheEnabled
			common.MemoryCacheEnabled = memoryCacheEnabled
			t.Cleanup(func() {
				common.MemoryCacheEnabled = previousMemoryCacheEnabled
				clearPreferredOwnerTables(t)
				InitChannelCache()
			})

			insertAbilitySelectionCandidate(t, 1, constant.ChannelTypeAdvancedCustom, 10, "/v1/chat/completions", "other-model")
			insertAbilitySelectionCandidate(t, 2, constant.ChannelTypeAdvancedCustom, 1, "/v1/chat/completions", "selection-model")
			if memoryCacheEnabled {
				InitChannelCache()
			}

			channels, err := ListSatisfiedChannels("default", "selection-model", "/v1/chat/completions")
			require.NoError(t, err)
			require.Len(t, channels, 1)
			assert.Equal(t, 2, channels[0].Id)
		})
	}
}
