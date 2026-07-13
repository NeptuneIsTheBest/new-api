package model

import (
	"errors"

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

// GetTokenUsageTotals returns settled log totals for the supplied user-owned
// tokens, respecting each token's latest usage reset boundary.
func GetTokenUsageTotals(userId int, tokens []*Token) (map[int]TokenUsageTotals, error) {
	totals := make(map[int]TokenUsageTotals, len(tokens))
	if userId <= 0 || len(tokens) == 0 {
		return totals, nil
	}

	unresetTokenIds := make([]int, 0, len(tokens))
	var tokenScope *gorm.DB
	for _, token := range tokens {
		if token == nil || token.Id <= 0 || token.UserId != userId {
			continue
		}
		if token.UsageResetTime <= 0 {
			unresetTokenIds = append(unresetTokenIds, token.Id)
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

	if len(unresetTokenIds) > 0 {
		condition := LOG_DB.Where("token_id IN ?", unresetTokenIds)
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
	if id <= 0 || userId <= 0 || resetTime <= 0 || resetTimeNano <= 0 {
		return errors.New("invalid token usage reset parameters")
	}

	result := DB.Model(&Token{}).
		Where("id = ? AND user_id = ?", id, userId).
		Updates(map[string]interface{}{
			"usage_reset_time":      resetTime,
			"usage_reset_time_nano": resetTimeNano,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := DB.Model(&Token{}).Where("id = ? AND user_id = ?", id, userId).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return gorm.ErrRecordNotFound
		}
	}
	return nil
}
