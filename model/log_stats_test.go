package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func insertLogForStatsTest(t *testing.T, log Log) {
	t.Helper()
	require.NoError(t, LOG_DB.Create(&log).Error)
}

func TestSumUsedQuotaIncludesTokenTotal(t *testing.T) {
	truncateTables(t)
	initCol()

	now := time.Now().Unix()
	matching := Log{
		UserId:           1,
		Username:         "alice",
		CreatedAt:        now - 10,
		Type:             LogTypeConsume,
		ModelName:        "gpt-4o",
		TokenName:        "primary",
		Quota:            100,
		PromptTokens:     40,
		CompletionTokens: 60,
		ChannelId:        1,
		Group:            "default",
		Other:            `{"cache_tokens":10}`,
	}
	insertLogForStatsTest(t, matching)

	matching.Id = 0
	matching.CreatedAt = now - 5
	matching.Quota = 50
	matching.PromptTokens = 10
	matching.CompletionTokens = 20
	matching.Other = `{"cache_tokens":5,"usage_semantic":"anthropic"}`
	insertLogForStatsTest(t, matching)

	ignoredError := matching
	ignoredError.Id = 0
	ignoredError.Type = LogTypeError
	ignoredError.Quota = 999
	ignoredError.PromptTokens = 1000
	ignoredError.CompletionTokens = 1000
	insertLogForStatsTest(t, ignoredError)

	ignoredUser := matching
	ignoredUser.Id = 0
	ignoredUser.Username = "bob"
	ignoredUser.Quota = 500
	ignoredUser.PromptTokens = 500
	ignoredUser.CompletionTokens = 500
	insertLogForStatsTest(t, ignoredUser)

	stat, err := SumUsedQuota(
		LogTypeUnknown,
		now-60,
		now+60,
		"gpt-4o",
		"alice",
		"primary",
		1,
		"default",
	)
	require.NoError(t, err)
	require.Equal(t, 150, stat.Quota)
	require.Equal(t, 130, stat.Token)
	require.Equal(t, 2, stat.Rpm)
	require.Equal(t, 130, stat.Tpm)
	require.Equal(t, 15, stat.CacheTokens)
	require.Equal(t, 55, stat.CacheInputTokens)
	require.InDelta(t, 15.0/55.0, stat.CacheHitRate, 0.000001)
}

func TestSumUsedQuotaEmptyResultReturnsZeroToken(t *testing.T) {
	truncateTables(t)
	initCol()

	stat, err := SumUsedQuota(
		LogTypeUnknown,
		time.Now().Unix()-60,
		time.Now().Unix()+60,
		"missing-model",
		"missing-user",
		"missing-token",
		1,
		"default",
	)
	require.NoError(t, err)
	require.Equal(t, 0, stat.Quota)
	require.Equal(t, 0, stat.Token)
	require.Equal(t, 0, stat.Rpm)
	require.Equal(t, 0, stat.Tpm)
	require.Equal(t, 0, stat.CacheTokens)
	require.Equal(t, 0, stat.CacheInputTokens)
	require.Equal(t, 0.0, stat.CacheHitRate)
}

func TestSumUsedQuotaCacheHitStatsHandlesInvalidOtherAndFallback(t *testing.T) {
	truncateTables(t)
	initCol()

	now := time.Now().Unix()
	insertLogForStatsTest(t, Log{
		UserId:           1,
		Username:         "alice",
		CreatedAt:        now - 10,
		Type:             LogTypeConsume,
		ModelName:        "gpt-4o",
		TokenName:        "primary",
		PromptTokens:     20,
		CompletionTokens: 5,
		ChannelId:        1,
		Group:            "default",
		Other:            `{bad json`,
	})
	insertLogForStatsTest(t, Log{
		UserId:           1,
		Username:         "alice",
		CreatedAt:        now - 5,
		Type:             LogTypeConsume,
		ModelName:        "gpt-4o",
		TokenName:        "primary",
		PromptTokens:     0,
		CompletionTokens: 5,
		ChannelId:        1,
		Group:            "default",
		Other:            `{"cache_tokens":12}`,
	})

	stat, err := SumUsedQuota(
		LogTypeUnknown,
		now-60,
		now+60,
		"gpt-4o",
		"alice",
		"primary",
		1,
		"default",
	)
	require.NoError(t, err)
	require.Equal(t, 12, stat.CacheTokens)
	require.Equal(t, 32, stat.CacheInputTokens)
	require.InDelta(t, 12.0/32.0, stat.CacheHitRate, 0.000001)
}
