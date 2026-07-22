package service

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	tokenUsageDefaultTimezone = "UTC"
	tokenUsageBucketUnit      = "day"
	tokenUsageDaySeconds      = int64(24 * 60 * 60)
	tokenUsageModelLimit      = 10
)

type TokenUsageTrend struct {
	Bucket           string `json:"bucket"`
	SettledRequests  int64  `json:"settled_requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	TotalQuota       int64  `json:"total_quota"`
}

type TokenUsageDetails struct {
	Available           bool                    `json:"available"`
	ResetAt             int64                   `json:"reset_at"`
	RangeStart          int64                   `json:"range_start"`
	RangeStartNano      string                  `json:"range_start_nano,omitempty"`
	RangeStartExclusive bool                    `json:"range_start_exclusive"`
	RangeEnd            int64                   `json:"range_end"`
	RangeEndNano        string                  `json:"range_end_nano"`
	Timezone            string                  `json:"timezone"`
	BucketUnit          string                  `json:"bucket_unit"`
	Summary             model.TokenUsageSummary `json:"summary"`
	Trend               []TokenUsageTrend       `json:"trend"`
	Models              []model.TokenUsageModel `json:"models"`
}

func GetTokenUsageDetails(userId int, token *model.Token, snapshotAt time.Time, timezoneName string) (*TokenUsageDetails, error) {
	endTime := snapshotAt.Unix()
	if userId <= 0 || token == nil || token.Id <= 0 || token.UserId != userId || endTime <= 0 {
		return nil, errors.New("invalid token usage detail parameters")
	}
	timezoneName = strings.TrimSpace(timezoneName)
	if timezoneName == "" {
		timezoneName = tokenUsageDefaultTimezone
	}
	if timezoneName == "Local" {
		return nil, errors.New("invalid token usage timezone")
	}
	location, err := time.LoadLocation(timezoneName)
	if err != nil {
		return nil, errors.New("invalid token usage timezone")
	}

	rangeStart := token.CreatedTime
	if token.UsageResetTime > 0 {
		rangeStart = token.UsageResetTime
	}
	if rangeStart < 0 {
		rangeStart = 0
	}
	if rangeStart > endTime {
		rangeStart = endTime
	}
	rangeStartNano := ""
	if token.UsageResetTimeNano > 0 {
		rangeStartNano = strconv.FormatInt(token.UsageResetTimeNano, 10)
	}

	details := &TokenUsageDetails{
		Available:           common.LogConsumeEnabled,
		ResetAt:             token.UsageResetTime,
		RangeStart:          rangeStart,
		RangeStartNano:      rangeStartNano,
		RangeStartExclusive: token.UsageResetTime > 0 || token.UsageResetTimeNano > 0,
		RangeEnd:            endTime,
		RangeEndNano:        strconv.FormatInt(snapshotAt.UnixNano(), 10),
		Timezone:            timezoneName,
		BucketUnit:          tokenUsageBucketUnit,
		Trend:               make([]TokenUsageTrend, 0),
		Models:              make([]model.TokenUsageModel, 0),
	}
	if !details.Available {
		return details, nil
	}

	summary, err := model.GetTokenUsageSummary(userId, token, snapshotAt)
	if err != nil {
		return nil, err
	}
	trendRows, err := model.GetTokenUsageTrend(userId, token, snapshotAt, location)
	if err != nil {
		return nil, err
	}
	models, err := model.GetTokenUsageModels(userId, token, snapshotAt, tokenUsageModelLimit)
	if err != nil {
		return nil, err
	}

	summary.ChargedQuota = nonNegativeUsageValue(summary.ChargedQuota)
	summary.RefundedQuota = nonNegativeUsageValue(summary.RefundedQuota)
	summary.TotalQuota = nonNegativeUsageValue(summary.TotalQuota)
	if math.IsNaN(summary.CacheHitRate) || math.IsInf(summary.CacheHitRate, 0) || summary.CacheHitRate < 0 {
		summary.CacheHitRate = 0
	}
	trend := make([]TokenUsageTrend, 0, len(trendRows))
	for _, row := range trendRows {
		trend = append(trend, TokenUsageTrend{
			Bucket:           time.Unix(row.LocalDay*tokenUsageDaySeconds, 0).UTC().Format(time.DateOnly),
			SettledRequests:  row.SettledRequests,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			TotalTokens:      row.TotalTokens,
			TotalQuota:       nonNegativeUsageValue(row.TotalQuota),
		})
	}
	for index := range models {
		models[index].TotalQuota = nonNegativeUsageValue(models[index].TotalQuota)
	}

	details.Summary = summary
	details.Trend = trend
	details.Models = models
	return details, nil
}

func nonNegativeUsageValue(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
