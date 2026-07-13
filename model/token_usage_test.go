package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetTokenUsageTotalsUsesPerTokenResetBoundaries(t *testing.T) {
	truncateTables(t)

	originalLogDB := LOG_DB
	dsn := fmt.Sprintf("file:%s_logs?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	logDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&Log{}))
	LOG_DB = logDB
	t.Cleanup(func() {
		LOG_DB = originalLogDB
		sqlDB, dbErr := logDB.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	resetToken := &Token{UserId: 1, Key: "usage-reset", Name: "renamed", UsageResetTime: 100, UsageResetTimeNano: 100_500}
	legacyResetToken := &Token{UserId: 1, Key: "usage-legacy-reset", Name: "legacy", UsageResetTime: 100}
	unresetToken := &Token{UserId: 1, Key: "usage-all", Name: "all-time"}
	otherUserToken := &Token{UserId: 2, Key: "usage-other", Name: "other"}
	require.NoError(t, DB.Create(resetToken).Error)
	require.NoError(t, DB.Create(legacyResetToken).Error)
	require.NoError(t, DB.Create(unresetToken).Error)
	require.NoError(t, DB.Create(otherUserToken).Error)

	logs := []Log{
		{UserId: 1, TokenId: resetToken.Id, TokenName: "old-name", CreatedAt: 100, WrittenAtNano: 100_400, Type: LogTypeConsume, Quota: 100, PromptTokens: 50, CompletionTokens: 50},
		{UserId: 1, TokenId: resetToken.Id, TokenName: "old-name", CreatedAt: 100, WrittenAtNano: 100_600, Type: LogTypeConsume, Quota: 20, PromptTokens: 7, CompletionTokens: 3},
		{UserId: 1, TokenId: resetToken.Id, TokenName: "old-name", CreatedAt: 101, WrittenAtNano: 101_000, Type: LogTypeConsume, Quota: 60, PromptTokens: 20, CompletionTokens: 10},
		{UserId: 1, TokenId: resetToken.Id, CreatedAt: 102, WrittenAtNano: 102_000, Type: LogTypeError, PromptTokens: 500, CompletionTokens: 500},
		{UserId: 1, TokenId: resetToken.Id, CreatedAt: 103, WrittenAtNano: 103_000, Type: LogTypeRefund, Quota: 10, PromptTokens: 500, CompletionTokens: 500},
		{UserId: 1, TokenId: legacyResetToken.Id, CreatedAt: 100, WrittenAtNano: 100_600, Type: LogTypeConsume, Quota: 200, PromptTokens: 100, CompletionTokens: 100},
		{UserId: 1, TokenId: legacyResetToken.Id, CreatedAt: 101, WrittenAtNano: 101_000, Type: LogTypeConsume, Quota: 30, PromptTokens: 2, CompletionTokens: 3},
		{UserId: 1, TokenId: unresetToken.Id, CreatedAt: 50, Type: LogTypeConsume, Quota: 15, PromptTokens: 7, CompletionTokens: 8},
		{UserId: 1, TokenId: unresetToken.Id, CreatedAt: 60, Type: LogTypeRefund, Quota: 3, PromptTokens: 100, CompletionTokens: 100},
		{UserId: 2, TokenId: resetToken.Id, CreatedAt: 104, Type: LogTypeConsume, PromptTokens: 999, CompletionTokens: 999},
		{UserId: 2, TokenId: otherUserToken.Id, CreatedAt: 104, Type: LogTypeConsume, PromptTokens: 11, CompletionTokens: 12},
	}
	require.NoError(t, logDB.Create(&logs).Error)

	totals, err := GetTokenUsageTotals(1, []*Token{resetToken, legacyResetToken, unresetToken, otherUserToken})
	require.NoError(t, err)
	assert.Equal(t, TokenUsageTotals{TotalTokens: 40, TotalQuota: 70}, totals[resetToken.Id])
	assert.Equal(t, TokenUsageTotals{TotalTokens: 5, TotalQuota: 30}, totals[legacyResetToken.Id])
	assert.Equal(t, TokenUsageTotals{TotalTokens: 15, TotalQuota: 12}, totals[unresetToken.Id])
	_, includesOtherUser := totals[otherUserToken.Id]
	assert.False(t, includesOtherUser)
}

func TestResetTokenUsageMovesBoundaryWithoutChangingAccounting(t *testing.T) {
	truncateTables(t)

	token := &Token{
		UserId:             1,
		Key:                "usage-accounting",
		Name:               "accounting",
		RemainQuota:        600,
		UsedQuota:          400,
		UsageResetTime:     50,
		UsageResetTimeNano: 50_000,
	}
	require.NoError(t, DB.Create(token).Error)
	require.NoError(t, DB.Create(&Log{UserId: 1, TokenId: token.Id, CreatedAt: 75, Type: LogTypeConsume, Quota: 400}).Error)

	require.NoError(t, ResetTokenUsage(token.Id, 1, 100, 100_500))
	require.NoError(t, ResetTokenUsage(token.Id, 1, 100, 100_500))

	var updated Token
	require.NoError(t, DB.First(&updated, token.Id).Error)
	assert.Equal(t, int64(100), updated.UsageResetTime)
	assert.Equal(t, int64(100_500), updated.UsageResetTimeNano)
	assert.Equal(t, 400, updated.UsedQuota)
	assert.Equal(t, 600, updated.RemainQuota)

	var logCount int64
	require.NoError(t, DB.Model(&Log{}).Where("token_id = ?", token.Id).Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)

	err := ResetTokenUsage(token.Id, 2, 200, 200_500)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, DB.First(&updated, token.Id).Error)
	assert.Equal(t, int64(100), updated.UsageResetTime)
	assert.Equal(t, int64(100_500), updated.UsageResetTimeNano)
}

func TestCreateLogStoresWriteBoundary(t *testing.T) {
	truncateTables(t)

	log := &Log{UserId: 1, TokenId: 1, CreatedAt: 100, Type: LogTypeConsume}
	require.NoError(t, createLog(log))
	assert.Positive(t, log.WrittenAtNano)

	var stored Log
	require.NoError(t, DB.First(&stored, log.Id).Error)
	assert.Equal(t, log.WrittenAtNano, stored.WrittenAtNano)
}
