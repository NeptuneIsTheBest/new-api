package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTokenUsageLogDB(t *testing.T) *gorm.DB {
	t.Helper()
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
	return logDB
}

func TestGetTokenUsageTotalsUsesPerTokenResetBoundaries(t *testing.T) {
	truncateTables(t)
	logDB := setupTokenUsageLogDB(t)

	resetToken := &Token{UserId: 1, Key: "usage-reset", Name: "renamed", UsageResetTime: 100, UsageResetTimeNano: 100_500}
	legacyResetToken := &Token{UserId: 1, Key: "usage-legacy-reset", Name: "legacy", UsageResetTime: 100}
	unresetToken := &Token{UserId: 1, Key: "usage-all", Name: "all-time", CreatedTime: 50}
	otherUserToken := &Token{UserId: 2, Key: "usage-other", Name: "other"}
	require.NoError(t, DB.Create(resetToken).Error)
	require.NoError(t, DB.Create(legacyResetToken).Error)
	require.NoError(t, DB.Create(unresetToken).Error)
	require.NoError(t, DB.Create(otherUserToken).Error)

	logs := []Log{
		{UserId: 1, TokenId: resetToken.Id, TokenName: "old-name", ModelName: "cheap-model", CreatedAt: 100, WrittenAtNano: 100_400, Type: LogTypeConsume, Quota: 100, PromptTokens: 50, CompletionTokens: 50},
		{UserId: 1, TokenId: resetToken.Id, TokenName: "old-name", ModelName: "cheap-model", CreatedAt: 100, WrittenAtNano: 100_600, Type: LogTypeConsume, Quota: 20, PromptTokens: 7, CompletionTokens: 3, Other: `{"cache_tokens":4}`},
		{UserId: 1, TokenId: resetToken.Id, TokenName: "old-name", ModelName: "expensive-model", CreatedAt: 101, WrittenAtNano: 101_000, Type: LogTypeConsume, Quota: 60, PromptTokens: 20, CompletionTokens: 10, Other: `{"cache_tokens":5,"usage_semantic":"anthropic"}`},
		{UserId: 1, TokenId: resetToken.Id, ModelName: "expensive-model", CreatedAt: 102, WrittenAtNano: 102_000, Type: LogTypeError, PromptTokens: 500, CompletionTokens: 500},
		{UserId: 1, TokenId: resetToken.Id, ModelName: "expensive-model", CreatedAt: 103, WrittenAtNano: 103_000, Type: LogTypeRefund, Quota: 10, PromptTokens: 500, CompletionTokens: 500},
		{UserId: 1, TokenId: legacyResetToken.Id, CreatedAt: 100, WrittenAtNano: 100_600, Type: LogTypeConsume, Quota: 200, PromptTokens: 100, CompletionTokens: 100},
		{UserId: 1, TokenId: legacyResetToken.Id, CreatedAt: 101, WrittenAtNano: 101_000, Type: LogTypeConsume, Quota: 30, PromptTokens: 2, CompletionTokens: 3},
		{UserId: 1, TokenId: unresetToken.Id, CreatedAt: 49, Type: LogTypeConsume, Quota: 999, PromptTokens: 999, CompletionTokens: 999},
		{UserId: 1, TokenId: unresetToken.Id, CreatedAt: 50, Type: LogTypeConsume, Quota: 15, PromptTokens: 7, CompletionTokens: 8},
		{UserId: 1, TokenId: unresetToken.Id, CreatedAt: 60, Type: LogTypeRefund, Quota: 3, PromptTokens: 100, CompletionTokens: 100},
		{UserId: 2, TokenId: resetToken.Id, CreatedAt: 104, Type: LogTypeConsume, PromptTokens: 999, CompletionTokens: 999},
		{UserId: 2, TokenId: otherUserToken.Id, CreatedAt: 104, Type: LogTypeConsume, PromptTokens: 11, CompletionTokens: 12},
	}
	require.NoError(t, logDB.Create(&logs).Error)
	require.NoError(t, logDB.Model(&Log{}).Where("user_id = ?", 1).Update("username", "alice").Error)
	require.NoError(t, logDB.Model(&Log{}).Where("user_id = ?", 2).Update("username", "bob").Error)

	totals, err := GetTokenUsageTotals(1, []*Token{resetToken, legacyResetToken, unresetToken, otherUserToken})
	require.NoError(t, err)
	assert.Equal(t, TokenUsageTotals{TotalTokens: 40, TotalQuota: 70}, totals[resetToken.Id])
	assert.Equal(t, TokenUsageTotals{TotalTokens: 5, TotalQuota: 30}, totals[legacyResetToken.Id])
	assert.Equal(t, TokenUsageTotals{TotalTokens: 15, TotalQuota: 12}, totals[unresetToken.Id])
	_, includesOtherUser := totals[otherUserToken.Id]
	assert.False(t, includesOtherUser)

	snapshotAt := time.Unix(103, 0)
	summary, err := GetTokenUsageSummary(1, resetToken, snapshotAt)
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.SettledRequests)
	assert.Equal(t, int64(1), summary.FailedRequests)
	assert.Equal(t, int64(27), summary.PromptTokens)
	assert.Equal(t, int64(13), summary.CompletionTokens)
	assert.Equal(t, int64(40), summary.TotalTokens)
	assert.Equal(t, int64(9), summary.CacheTokens)
	assert.Equal(t, int64(32), summary.CacheInputTokens)
	assert.InDelta(t, 9.0/32.0, summary.CacheHitRate, 0.000001)
	assert.Equal(t, int64(80), summary.ChargedQuota)
	assert.Equal(t, int64(10), summary.RefundedQuota)
	assert.Equal(t, int64(70), summary.TotalQuota)

	trend, err := GetTokenUsageTrend(1, resetToken, snapshotAt, time.FixedZone("UTC+8", 8*60*60))
	require.NoError(t, err)
	require.Len(t, trend, 1)
	assert.Equal(t, int64(0), trend[0].LocalDay)
	assert.Equal(t, int64(2), trend[0].SettledRequests)
	assert.Equal(t, int64(40), trend[0].TotalTokens)
	assert.Equal(t, int64(70), trend[0].TotalQuota)

	models, err := GetTokenUsageModels(1, resetToken, snapshotAt, 10)
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "expensive-model", models[0].ModelName)
	assert.Equal(t, int64(50), models[0].TotalQuota)
	assert.Equal(t, int64(30), models[0].TotalTokens)
	assert.Equal(t, int64(1), models[0].FailedRequests)
	assert.Equal(t, "cheap-model", models[1].ModelName)

	// Exact token ID filtering remains stable after the token is renamed and
	// takes precedence over the stale name filter supplied by the client.
	filteredLogs, filteredTotal, err := GetUserLogs(1, LogTypeConsume, LogTimeRange{}, "", "renamed", resetToken.Id, 0, 10, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(3), filteredTotal)
	require.Len(t, filteredLogs, 3)
	for _, log := range filteredLogs {
		assert.Equal(t, resetToken.Id, log.TokenId)
		assert.Equal(t, "old-name", log.TokenName)
	}

	stat, err := SumUsedQuota(LogTypeUnknown, LogTimeRange{}, "", "alice", "renamed", resetToken.Id, 0, "")
	require.NoError(t, err)
	assert.Equal(t, 180, stat.Quota)
	assert.Equal(t, 140, stat.Token)

	_, err = GetTokenUsageSummary(2, resetToken, snapshotAt)
	require.Error(t, err)
}

func TestLogQueriesRespectExactUsageWindow(t *testing.T) {
	truncateTables(t)
	logDB := setupTokenUsageLogDB(t)

	now := time.Now().Unix()
	token := &Token{UserId: 1, Key: "usage-window", Name: "usage-window", CreatedTime: now - 10}
	require.NoError(t, DB.Create(token).Error)
	require.NoError(t, logDB.Create(&[]Log{
		{UserId: 1, Username: "alice", TokenId: token.Id, CreatedAt: now, WrittenAtNano: 1_400, Type: LogTypeConsume, Quota: 10, PromptTokens: 4, CompletionTokens: 1, Other: `{"cache_tokens":1}`},
		{UserId: 1, Username: "alice", TokenId: token.Id, CreatedAt: now, WrittenAtNano: 2_000, Type: LogTypeConsume, Quota: 20, PromptTokens: 6, CompletionTokens: 4, Other: `{"cache_tokens":2}`},
		{UserId: 1, Username: "alice", TokenId: token.Id, CreatedAt: now, WrittenAtNano: 2_600, Type: LogTypeConsume, Quota: 40, PromptTokens: 8, CompletionTokens: 2, Other: `{"cache_tokens":3}`},
		{UserId: 1, Username: "alice", TokenId: token.Id, CreatedAt: now + 1, WrittenAtNano: 3_000, Type: LogTypeConsume, Quota: 30, PromptTokens: 5, CompletionTokens: 5},
	}).Error)

	exactWindow := LogTimeRange{
		StartTimestamp:          now,
		StartWrittenAtNano:      1_500,
		StartTimestampExclusive: true,
		EndTimestamp:            now,
		EndWrittenAtNano:        2_500,
	}
	userLogs, userTotal, err := GetUserLogs(1, LogTypeConsume, exactWindow, "", "", token.Id, 0, 10, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), userTotal)
	require.Len(t, userLogs, 1)
	assert.Equal(t, int64(2_000), userLogs[0].WrittenAtNano)

	allLogs, allTotal, err := GetAllLogs(LogTypeConsume, exactWindow, "", "alice", "", token.Id, 0, 10, 0, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), allTotal)
	require.Len(t, allLogs, 1)
	assert.Equal(t, int64(2_000), allLogs[0].WrittenAtNano)

	stat, err := SumUsedQuota(LogTypeUnknown, exactWindow, "", "alice", "", token.Id, 0, "")
	require.NoError(t, err)
	assert.Equal(t, 20, stat.Quota)
	assert.Equal(t, 10, stat.Token)
	assert.Equal(t, 1, stat.Rpm)
	assert.Equal(t, 10, stat.Tpm)
	assert.Equal(t, 2, stat.CacheTokens)

	legacyWindow := LogTimeRange{
		StartTimestamp:          now,
		StartTimestampExclusive: true,
		EndTimestamp:            now + 1,
	}
	legacyLogs, legacyTotal, err := GetUserLogs(1, LogTypeConsume, legacyWindow, "", "", token.Id, 0, 10, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), legacyTotal)
	require.Len(t, legacyLogs, 1)
	assert.Equal(t, now+1, legacyLogs[0].CreatedAt)
}

func TestTokenUsageDetailQueriesShareWriteSnapshot(t *testing.T) {
	truncateTables(t)
	logDB := setupTokenUsageLogDB(t)

	token := &Token{Id: 1, UserId: 1, CreatedTime: 50}
	snapshotAt := time.Unix(100, 500)
	snapshotNano := snapshotAt.UnixNano()
	logs := []Log{
		{UserId: 1, TokenId: token.Id, ModelName: "before", CreatedAt: 100, WrittenAtNano: snapshotNano - 1, Type: LogTypeConsume, Quota: 10, PromptTokens: 4, CompletionTokens: 2, Other: `{"cache_tokens":2}`},
		{UserId: 1, TokenId: token.Id, ModelName: "legacy-zero", CreatedAt: 100, WrittenAtNano: 0, Type: LogTypeConsume, Quota: 20, PromptTokens: 6, CompletionTokens: 3, Other: `{"cache_tokens":3}`},
		{UserId: 1, TokenId: token.Id, ModelName: "legacy-null", CreatedAt: 100, WrittenAtNano: snapshotNano - 1, Type: LogTypeConsume, Quota: 30, PromptTokens: 8, CompletionTokens: 4, Other: `{"cache_tokens":4}`},
		{UserId: 1, TokenId: token.Id, ModelName: "after", CreatedAt: 100, WrittenAtNano: snapshotNano + 1, Type: LogTypeConsume, Quota: 1_000, PromptTokens: 100, CompletionTokens: 100, Other: `{"cache_tokens":100}`},
	}
	require.NoError(t, logDB.Create(&logs).Error)
	require.NoError(t, logDB.Model(&Log{}).Where("model_name = ?", "legacy-null").UpdateColumn("written_at_nano", nil).Error)

	summary, err := GetTokenUsageSummary(1, token, snapshotAt)
	require.NoError(t, err)
	assert.Equal(t, int64(3), summary.SettledRequests)
	assert.Equal(t, int64(18), summary.PromptTokens)
	assert.Equal(t, int64(9), summary.CompletionTokens)
	assert.Equal(t, int64(27), summary.TotalTokens)
	assert.Equal(t, int64(9), summary.CacheTokens)
	assert.Equal(t, int64(18), summary.CacheInputTokens)
	assert.InDelta(t, 0.5, summary.CacheHitRate, 0.000001)
	assert.Equal(t, int64(60), summary.ChargedQuota)
	assert.Equal(t, int64(60), summary.TotalQuota)

	trend, err := GetTokenUsageTrend(1, token, snapshotAt, time.UTC)
	require.NoError(t, err)
	require.Len(t, trend, 1)
	assert.Equal(t, int64(3), trend[0].SettledRequests)
	assert.Equal(t, int64(27), trend[0].TotalTokens)
	assert.Equal(t, int64(60), trend[0].TotalQuota)

	models, err := GetTokenUsageModels(1, token, snapshotAt, 10)
	require.NoError(t, err)
	require.Len(t, models, 3)
	assert.Equal(t, "legacy-null", models[0].ModelName)
	assert.Equal(t, "legacy-zero", models[1].ModelName)
	assert.Equal(t, "before", models[2].ModelName)
}

func TestGetTokenUsageTrendUsesTimezoneRulesAtEachTimestamp(t *testing.T) {
	logDB := setupTokenUsageLogDB(t)
	location, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	unix := func(year int, month time.Month, day int, hour int, minute int) int64 {
		return time.Date(year, month, day, hour, minute, 0, 0, time.UTC).Unix()
	}
	token := &Token{
		Id:          1,
		UserId:      1,
		CreatedTime: unix(2026, time.January, 14, 0, 0),
	}
	logs := []Log{
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.January, 15, 4, 30), Type: LogTypeConsume, PromptTokens: 1},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.January, 15, 5, 30), Type: LogTypeConsume, PromptTokens: 2},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.March, 8, 5, 30), Type: LogTypeConsume, PromptTokens: 4},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.March, 9, 3, 30), Type: LogTypeConsume, PromptTokens: 8},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.March, 9, 4, 30), Type: LogTypeConsume, PromptTokens: 16},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.November, 1, 5, 30), Type: LogTypeConsume, PromptTokens: 32},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.November, 1, 6, 30), Type: LogTypeConsume, PromptTokens: 64},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.November, 2, 4, 30), Type: LogTypeConsume, PromptTokens: 128},
		{UserId: 1, TokenId: token.Id, CreatedAt: unix(2026, time.November, 2, 5, 30), Type: LogTypeConsume, PromptTokens: 256},
	}
	require.NoError(t, logDB.Create(&logs).Error)

	trend, err := GetTokenUsageTrend(
		1,
		token,
		time.Unix(unix(2026, time.November, 2, 6, 0), 0),
		location,
	)
	require.NoError(t, err)
	require.Len(t, trend, 6)

	expected := []struct {
		date            string
		settledRequests int64
		promptTokens    int64
	}{
		{date: "2026-01-14", settledRequests: 1, promptTokens: 1},
		{date: "2026-01-15", settledRequests: 1, promptTokens: 2},
		{date: "2026-03-08", settledRequests: 2, promptTokens: 12},
		{date: "2026-03-09", settledRequests: 1, promptTokens: 16},
		{date: "2026-11-01", settledRequests: 3, promptTokens: 224},
		{date: "2026-11-02", settledRequests: 1, promptTokens: 256},
	}
	for index, want := range expected {
		bucketDate := time.Unix(trend[index].LocalDay*24*60*60, 0).UTC().Format(time.DateOnly)
		assert.Equal(t, want.date, bucketDate)
		assert.Equal(t, want.settledRequests, trend[index].SettledRequests)
		assert.Equal(t, want.promptTokens, trend[index].PromptTokens)
	}
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
