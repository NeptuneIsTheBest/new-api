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
	}
	insertLogForStatsTest(t, matching)

	matching.Id = 0
	matching.CreatedAt = now - 5
	matching.Quota = 50
	matching.PromptTokens = 10
	matching.CompletionTokens = 20
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
}
