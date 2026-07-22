package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type TokenUsageTotals struct {
	TotalTokens int64
	TotalQuota  int64
}

type tokenUsageTotalsRow struct {
	TokenId     int   `gorm:"column:token_id"`
	TotalTokens int64 `gorm:"column:total_tokens"`
	TotalQuota  int64 `gorm:"column:total_quota"`
}

type TokenUsageSummary struct {
	SettledRequests  int64   `json:"settled_requests" gorm:"column:settled_requests"`
	FailedRequests   int64   `json:"failed_requests" gorm:"column:failed_requests"`
	PromptTokens     int64   `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens" gorm:"column:completion_tokens"`
	TotalTokens      int64   `json:"total_tokens" gorm:"column:total_tokens"`
	CacheTokens      int64   `json:"cache_tokens" gorm:"column:cache_tokens"`
	CacheInputTokens int64   `json:"cache_input_tokens" gorm:"column:cache_input_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate" gorm:"column:cache_hit_rate"`
	ChargedQuota     int64   `json:"charged_quota" gorm:"column:charged_quota"`
	RefundedQuota    int64   `json:"refunded_quota" gorm:"column:refunded_quota"`
	TotalQuota       int64   `json:"total_quota" gorm:"column:total_quota"`
}

type TokenUsageTrend struct {
	LocalDay         int64 `gorm:"column:local_day"`
	SettledRequests  int64 `json:"settled_requests" gorm:"column:settled_requests"`
	PromptTokens     int64 `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens" gorm:"column:completion_tokens"`
	TotalTokens      int64 `json:"total_tokens" gorm:"column:total_tokens"`
	TotalQuota       int64 `json:"total_quota" gorm:"column:total_quota"`
}

type TokenUsageModel struct {
	ModelName        string `json:"model_name" gorm:"column:model_name"`
	SettledRequests  int64  `json:"settled_requests" gorm:"column:settled_requests"`
	FailedRequests   int64  `json:"failed_requests" gorm:"column:failed_requests"`
	PromptTokens     int64  `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens" gorm:"column:completion_tokens"`
	TotalTokens      int64  `json:"total_tokens" gorm:"column:total_tokens"`
	TotalQuota       int64  `json:"total_quota" gorm:"column:total_quota"`
}

func tokenUsageLogQuery(userId int, token *Token, snapshotAt time.Time) *gorm.DB {
	timeRange := LogTimeRange{
		EndTimestamp:     snapshotAt.Unix(),
		EndWrittenAtNano: snapshotAt.UnixNano(),
	}
	if token.UsageResetTimeNano > 0 {
		timeRange.StartWrittenAtNano = token.UsageResetTimeNano
	} else if token.UsageResetTime > 0 {
		timeRange.StartTimestamp = token.UsageResetTime
		timeRange.StartTimestampExclusive = true
	} else if token.CreatedTime > 0 {
		timeRange.StartTimestamp = token.CreatedTime
	}

	query := LOG_DB.Table("logs").Where("user_id = ? AND token_id = ?", userId, token.Id)
	return applyLogTimeRange(query, "", timeRange)
}

func GetTokenUsageSummary(userId int, token *Token, snapshotAt time.Time) (TokenUsageSummary, error) {
	var summary TokenUsageSummary
	if userId <= 0 || token == nil || token.Id <= 0 || token.UserId != userId || snapshotAt.Unix() <= 0 {
		return summary, errors.New("invalid token usage parameters")
	}

	err := tokenUsageLogQuery(userId, token, snapshotAt).
		Select(`
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS settled_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS failed_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END), 0) AS prompt_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END), 0) AS completion_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS charged_quota,
			COALESCE(SUM(CASE WHEN type = ? THEN quota ELSE 0 END), 0) AS refunded_quota,
			COALESCE(SUM(CASE WHEN type = ? THEN quota WHEN type = ? THEN -quota ELSE 0 END), 0) AS total_quota`,
			LogTypeConsume,
			LogTypeError,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeRefund,
			LogTypeConsume,
			LogTypeRefund,
		).
		Where("type IN ?", []int{LogTypeConsume, LogTypeRefund, LogTypeError}).
		Scan(&summary).Error
	if err != nil {
		return TokenUsageSummary{}, err
	}

	summary.TotalTokens = summary.PromptTokens + summary.CompletionTokens
	cacheStat := Stat{}
	cacheQuery := tokenUsageLogQuery(userId, token, snapshotAt).Where("type = ?", LogTypeConsume)
	if err := fillCacheHitStat(cacheQuery, &cacheStat); err != nil {
		return TokenUsageSummary{}, err
	}
	summary.CacheTokens = int64(cacheStat.CacheTokens)
	summary.CacheInputTokens = int64(cacheStat.CacheInputTokens)
	summary.CacheHitRate = cacheStat.CacheHitRate
	return summary, nil
}

func tokenUsageTimezoneOffsetExpr(startTime int64, snapshotAt time.Time, location *time.Location) string {
	if startTime < 0 {
		startTime = 0
	}
	if startTime > snapshotAt.Unix() {
		startTime = snapshotAt.Unix()
	}
	current := time.Unix(startTime, 0).In(location)
	_, offsetSeconds := current.Zone()
	_, zoneEnd := current.ZoneBounds()
	if zoneEnd.IsZero() || zoneEnd.After(snapshotAt) {
		return fmt.Sprint(offsetSeconds)
	}

	var expression strings.Builder
	expression.WriteString("(CASE")
	for !zoneEnd.IsZero() && !zoneEnd.After(snapshotAt) {
		fmt.Fprintf(&expression, " WHEN created_at < %d THEN %d", zoneEnd.Unix(), offsetSeconds)
		current = zoneEnd
		_, offsetSeconds = current.Zone()
		_, zoneEnd = current.ZoneBounds()
	}
	fmt.Fprintf(&expression, " ELSE %d END)", offsetSeconds)
	return expression.String()
}

func tokenUsageDayBucketExpr(token *Token, snapshotAt time.Time, location *time.Location) string {
	const daySeconds int64 = 24 * 60 * 60
	offsetExpr := tokenUsageTimezoneOffsetExpr(token.CreatedTime, snapshotAt, location)
	switch {
	case common.UsingLogDatabase(common.DatabaseTypeMySQL):
		return fmt.Sprintf("FLOOR((created_at + %s + %d) / %d) - 1", offsetExpr, daySeconds, daySeconds)
	case common.UsingLogDatabase(common.DatabaseTypeClickHouse):
		return fmt.Sprintf("intDiv(created_at + %s + %d, %d) - 1", offsetExpr, daySeconds, daySeconds)
	default:
		return fmt.Sprintf("((created_at + %s + %d) / %d) - 1", offsetExpr, daySeconds, daySeconds)
	}
}

func GetTokenUsageTrend(userId int, token *Token, snapshotAt time.Time, location *time.Location) ([]TokenUsageTrend, error) {
	if userId <= 0 || token == nil || token.Id <= 0 || token.UserId != userId || snapshotAt.Unix() <= 0 || location == nil {
		return nil, errors.New("invalid token usage parameters")
	}

	bucketExpr := tokenUsageDayBucketExpr(token, snapshotAt, location)
	rows := make([]TokenUsageTrend, 0)
	err := tokenUsageLogQuery(userId, token, snapshotAt).
		Select(fmt.Sprintf(`
			%s AS local_day,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS settled_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END), 0) AS prompt_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END), 0) AS completion_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN quota WHEN type = ? THEN -quota ELSE 0 END), 0) AS total_quota`, bucketExpr),
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeRefund,
		).
		Where("type IN ?", []int{LogTypeConsume, LogTypeRefund}).
		Group(bucketExpr).
		Order("local_day ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for index := range rows {
		rows[index].TotalTokens = rows[index].PromptTokens + rows[index].CompletionTokens
	}
	return rows, nil
}

func GetTokenUsageModels(userId int, token *Token, snapshotAt time.Time, limit int) ([]TokenUsageModel, error) {
	if userId <= 0 || token == nil || token.Id <= 0 || token.UserId != userId || snapshotAt.Unix() <= 0 {
		return nil, errors.New("invalid token usage parameters")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}

	rows := make([]TokenUsageModel, 0, limit)
	err := tokenUsageLogQuery(userId, token, snapshotAt).
		Select(`
			model_name,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS settled_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN 1 ELSE 0 END), 0) AS failed_requests,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END), 0) AS prompt_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END), 0) AS completion_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens + completion_tokens ELSE 0 END), 0) AS total_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN quota WHEN type = ? THEN -quota ELSE 0 END), 0) AS total_quota`,
			LogTypeConsume,
			LogTypeError,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeConsume,
			LogTypeRefund,
		).
		Where("type IN ?", []int{LogTypeConsume, LogTypeRefund, LogTypeError}).
		Group("model_name").
		Order("total_quota DESC, total_tokens DESC, model_name ASC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetTokenUsageTotals returns settled log totals for the supplied user-owned
// tokens, respecting each token's latest usage reset or creation boundary.
func GetTokenUsageTotals(userId int, tokens []*Token) (map[int]TokenUsageTotals, error) {
	totals := make(map[int]TokenUsageTotals, len(tokens))
	if userId <= 0 || len(tokens) == 0 {
		return totals, nil
	}

	unboundedTokenIds := make([]int, 0, len(tokens))
	var tokenScope *gorm.DB
	for _, token := range tokens {
		if token == nil || token.Id <= 0 || token.UserId != userId {
			continue
		}
		if token.UsageResetTime <= 0 {
			if token.CreatedTime <= 0 {
				unboundedTokenIds = append(unboundedTokenIds, token.Id)
				continue
			}
			condition := LOG_DB.Where("token_id = ? AND created_at >= ?", token.Id, token.CreatedTime)
			if tokenScope == nil {
				tokenScope = condition
			} else {
				tokenScope = tokenScope.Or(condition)
			}
			continue
		}

		var condition *gorm.DB
		if token.UsageResetTimeNano > 0 {
			condition = LOG_DB.Where("token_id = ? AND written_at_nano > ?", token.Id, token.UsageResetTimeNano)
		} else {
			condition = LOG_DB.Where("token_id = ? AND created_at > ?", token.Id, token.UsageResetTime)
		}
		if tokenScope == nil {
			tokenScope = condition
		} else {
			tokenScope = tokenScope.Or(condition)
		}
	}

	if len(unboundedTokenIds) > 0 {
		condition := LOG_DB.Where("token_id IN ?", unboundedTokenIds)
		if tokenScope == nil {
			tokenScope = condition
		} else {
			tokenScope = tokenScope.Or(condition)
		}
	}
	if tokenScope == nil {
		return totals, nil
	}

	rows := make([]tokenUsageTotalsRow, 0, len(tokens))
	err := LOG_DB.Table("logs").
		Select(`token_id,
			COALESCE(SUM(CASE WHEN type = ? THEN prompt_tokens ELSE 0 END), 0) +
				COALESCE(SUM(CASE WHEN type = ? THEN completion_tokens ELSE 0 END), 0) AS total_tokens,
			COALESCE(SUM(CASE WHEN type = ? THEN quota WHEN type = ? THEN -quota ELSE 0 END), 0) AS total_quota`,
			LogTypeConsume, LogTypeConsume, LogTypeConsume, LogTypeRefund).
		Where("user_id = ? AND type IN ?", userId, []int{LogTypeConsume, LogTypeRefund}).
		Where(tokenScope).
		Group("token_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		totals[row.TokenId] = TokenUsageTotals{
			TotalTokens: row.TotalTokens,
			TotalQuota:  row.TotalQuota,
		}
	}
	return totals, nil
}

// ResetTokenUsage moves a token's display-only usage baseline without
// changing its quota, billing total, status, or historical logs.
func ResetTokenUsage(id int, userId int, resetTime int64, resetTimeNano int64) error {
	count, err := ResetTokenUsages([]int{id}, userId, resetTime, resetTimeNano)
	if err != nil {
		return err
	}
	if count == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ResetTokenUsages moves the display-only usage baseline for user-owned
// tokens and returns the number of matching tokens.
func ResetTokenUsages(ids []int, userId int, resetTime int64, resetTimeNano int64) (int64, error) {
	if len(ids) == 0 || userId <= 0 || resetTime <= 0 || resetTimeNano <= 0 {
		return 0, errors.New("invalid token usage reset parameters")
	}
	for _, id := range ids {
		if id <= 0 {
			return 0, errors.New("invalid token usage reset parameters")
		}
	}

	var count int64
	if err := DB.Model(&Token{}).
		Where("id IN ? AND user_id = ?", ids, userId).
		Count(&count).Error; err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	if err := DB.Model(&Token{}).
		Where("id IN ? AND user_id = ?", ids, userId).
		Updates(map[string]interface{}{
			"usage_reset_time":      resetTime,
			"usage_reset_time_nano": resetTimeNano,
		}).Error; err != nil {
		return 0, err
	}
	return count, nil
}
