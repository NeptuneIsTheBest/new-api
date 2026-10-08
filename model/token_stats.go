package model

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

type TokenUsageStat struct {
	TokenID      int   `json:"token_id"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	Quota        int64 `json:"quota"`
}

func GetTokenUsageStats(ctx context.Context, userID int, tokenIDs []int, startTimestamp, endTimestamp int64) ([]TokenUsageStat, error) {
	if userID <= 0 || len(tokenIDs) == 0 || len(tokenIDs) > 100 || startTimestamp < 0 || endTimestamp < startTimestamp {
		return nil, gorm.ErrRecordNotFound
	}
	// Check ownership in the primary database before querying the independently
	// configured log database. Deleted and foreign keys have the same result.
	var ownedIDs []int
	err := DB.WithContext(ctx).Model(&Token{}).
		Where("user_id = ? AND id IN ?", userID, tokenIDs).Pluck("id", &ownedIDs).Error
	if err != nil {
		common.SysError("failed to check token statistics ownership: " + sanitizeDBError(err).Error())
		return nil, errors.New("查询统计数据失败")
	}
	if len(ownedIDs) != len(tokenIDs) {
		return nil, gorm.ErrRecordNotFound
	}

	period := LOG_DB.WithContext(ctx).Table("logs").
		Where("user_id = ? AND token_id IN ? AND type = ?", userID, ownedIDs, LogTypeConsume).
		Where("created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp).
		Session(&gorm.Session{})
	var rows []struct {
		TokenID int
		Stat
	}
	err = logTokenStatQuery(period, true).Scan(&rows).Error
	grouped := make(map[int]Stat, len(rows))
	var pgErr *pgconn.PgError
	if common.UsingLogDatabase(common.DatabaseTypePostgreSQL) && errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		common.SysError("invalid historical log JSON; using streaming token statistics")
		grouped, err = streamLogTokenStats(period, true)
	} else if err == nil {
		for _, row := range rows {
			grouped[row.TokenID] = row.Stat
		}
	}
	if err != nil {
		common.SysError("failed to query token statistics: " + sanitizeDBError(err).Error())
		return nil, errors.New("查询统计数据失败")
	}

	stats := make([]TokenUsageStat, 0, len(tokenIDs))
	for _, tokenID := range tokenIDs {
		stat := grouped[tokenID]
		if err := stat.finalizeTokenCounts(); err != nil {
			return nil, err
		}
		stats = append(stats, TokenUsageStat{
			TokenID: tokenID, InputTokens: stat.InputTokens, OutputTokens: stat.OutputTokens,
			TotalTokens: stat.TotalTokens, Quota: stat.Quota,
		})
	}
	return stats, nil
}
