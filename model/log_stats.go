package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

type Stat struct {
	Quota           int64   `json:"quota"`
	Rpm             int64   `json:"rpm"`
	Tpm             int64   `json:"tpm"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
}

type LogStatParams struct {
	Type              int
	StartTimestamp    int64
	EndTimestamp      int64
	UserID            *int // nil selects the admin scope; even a zero ID restricts self queries.
	Username          string
	TokenName         string
	ModelName         string
	Channel           int
	Group             string
	RequestID         string
	UpstreamRequestID string
}

func SumUsedQuota(ctx context.Context, params LogStatParams) (stat Stat, err error) {
	base := LOG_DB.WithContext(ctx).Table("logs").Where("type = ?", LogTypeConsume)
	if params.UserID != nil {
		base = base.Where("user_id = ?", *params.UserID)
	} else if base, err = applyExplicitLogTextFilter(base, "username", params.Username); err != nil {
		return stat, err
	}
	if base, err = applyExplicitLogTextFilter(base, "model_name", params.ModelName); err != nil {
		return stat, err
	}
	for _, filter := range []struct{ column, value string }{
		{"token_name", params.TokenName},
		{logGroupCol, params.Group},
		{"request_id", params.RequestID},
		{"upstream_request_id", params.UpstreamRequestID},
	} {
		if filter.value != "" {
			base = base.Where(filter.column+" = ?", filter.value)
		}
	}
	if params.Channel != 0 {
		base = base.Where("channel_id = ?", params.Channel)
	}

	period := base.Session(&gorm.Session{})
	if params.StartTimestamp != 0 {
		period = period.Where("created_at >= ?", params.StartTimestamp)
	}
	if params.EndTimestamp != 0 {
		period = period.Where("created_at <= ?", params.EndTimestamp)
	}
	period = period.Session(&gorm.Session{})
	if params.Type == LogTypeUnknown || params.Type == LogTypeConsume {
		err = logTokenStatQuery(period).Scan(&stat).Error
		var pgErr *pgconn.PgError
		// All text-to-number casts are guarded by JSON numeric types. In this
		// query 22P02 can only come from historical invalid JSON in other.
		if common.UsingLogDatabase(common.DatabaseTypePostgreSQL) && errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			common.SysError("invalid historical log JSON; using streaming token statistics")
			stat, err = streamLogTokenStats(period)
		}
	} else {
		// Preserve the existing quota/rate behavior for other log types while
		// the new token statistics only describe matching consumption rows.
		err = period.Select("COALESCE(SUM(quota), 0) AS quota").Scan(&stat).Error
	}
	if err != nil {
		common.SysError("failed to query log stat: " + sanitizeDBError(err).Error())
		return Stat{}, errors.New("查询统计数据失败")
	}
	if stat.InputTokens < 0 || stat.OutputTokens < 0 || stat.CacheReadTokens < 0 || stat.CacheReadTokens > stat.InputTokens || stat.InputTokens > math.MaxInt64-stat.OutputTokens {
		return Stat{}, errors.New("统计 Token 数量超出范围")
	}
	stat.TotalTokens = stat.InputTokens + stat.OutputTokens
	if stat.InputTokens > 0 {
		stat.CacheHitRate = float64(stat.CacheReadTokens) / float64(stat.InputTokens) * 100
	}

	// RPM/TPM retain their current rolling 60-second window, independent
	// of the selected historical time range.
	var rateStat Stat
	err = base.Session(&gorm.Session{}).
		Where("created_at >= ?", time.Now().Add(-60*time.Second).Unix()).
		Select("COUNT(*) AS rpm, COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0) AS tpm").
		Scan(&rateStat).Error
	if err != nil {
		common.SysError("failed to query rpm/tpm stat: " + sanitizeDBError(err).Error())
		return Stat{}, errors.New("查询统计数据失败")
	}
	stat.Rpm, stat.Tpm = rateStat.Rpm, rateStat.Tpm
	return stat, nil
}

// logStatJSONText only receives fixed metadata paths, never user input.
func logStatJSONText(keys ...string) string {
	switch common.LogDatabaseType() {
	case common.DatabaseTypeMySQL:
		return "NULLIF(JSON_UNQUOTE(JSON_EXTRACT(other, '$." + strings.Join(keys, ".") + "')), 'null')"
	case common.DatabaseTypePostgreSQL:
		return "(other::json #>> '{" + strings.Join(keys, ",") + "}')"
	case common.DatabaseTypeClickHouse:
		if len(keys) == 1 && keys[0] == "claude" {
			return "toString(JSONExtractBool(other, 'claude'))"
		}
		return "JSONExtractString(other, '" + strings.Join(keys, "', '") + "')"
	default:
		return "CAST(json_extract(other, '$." + strings.Join(keys, ".") + "') AS TEXT)"
	}
}

// Missing/non-numeric fields remain NULL so an explicit zero cache-write total
// takes precedence over old cache creation breakdowns.
func logStatJSONNumber(key string) string {
	value := logStatJSONText(key)
	switch common.LogDatabaseType() {
	case common.DatabaseTypeMySQL:
		return fmt.Sprintf("CASE WHEN JSON_TYPE(JSON_EXTRACT(other, '$.%s')) IN ('INTEGER', 'DOUBLE') THEN GREATEST(0, CAST(%s AS SIGNED)) END", key, value)
	case common.DatabaseTypePostgreSQL:
		return fmt.Sprintf("CASE WHEN json_typeof(other::json -> '%s') = 'number' THEN GREATEST(0, TRUNC((%s)::numeric)) END", key, value)
	case common.DatabaseTypeClickHouse:
		return fmt.Sprintf("CASE WHEN toString(JSONType(other, '%s')) IN ('Int64', 'UInt64', 'Float64') THEN greatest(0, JSONExtractInt(other, '%s')) END", key, key)
	default:
		return fmt.Sprintf("CASE WHEN json_type(other, '$.%s') IN ('integer', 'real') THEN MAX(0, CAST(json_extract(other, '$.%s') AS INTEGER)) END", key, key)
	}
}

func logTokenStatQuery(period *gorm.DB) *gorm.DB {
	safeJSON := "CASE WHEN json_valid(other) THEN other ELSE '{}' END"
	switch common.LogDatabaseType() {
	case common.DatabaseTypeMySQL:
		safeJSON = "CASE WHEN JSON_VALID(other) THEN other ELSE '{}' END"
	case common.DatabaseTypePostgreSQL:
		safeJSON = "COALESCE(NULLIF(TRIM(other), ''), '{}')"
	case common.DatabaseTypeClickHouse:
		safeJSON = "if(isValidJSON(logs.other), logs.other, '{}')"
	}
	documents := period.Select("quota, CASE WHEN prompt_tokens > 0 THEN prompt_tokens ELSE 0 END AS prompt_tokens, CASE WHEN completion_tokens > 0 THEN completion_tokens ELSE 0 END AS output_tokens, " + safeJSON + " AS other")
	fields := []string{"quota", "prompt_tokens", "output_tokens"}
	for _, key := range []string{"input_tokens_total", "cache_tokens", "cache_creation_tokens", "cache_creation_tokens_5m", "cache_creation_tokens_1h"} {
		fields = append(fields, "COALESCE("+logStatJSONNumber(key)+", 0) AS "+key)
	}
	fields = append(fields,
		logStatJSONNumber("cache_write_tokens")+" AS cache_write_tokens",
		logStatJSONText("usage_semantic")+" AS usage_semantic",
		logStatJSONText("admin_info", "usage_billing_path")+" AS usage_billing_path",
		logStatJSONText("claude")+" AS claude",
	)
	db := LOG_DB.WithContext(period.Statement.Context)
	metadata := db.Table("(?) AS log_documents", documents).Select(strings.Join(fields, ", "))
	cacheWrite := "COALESCE(cache_write_tokens, CASE WHEN cache_creation_tokens > cache_creation_tokens_5m + cache_creation_tokens_1h THEN cache_creation_tokens ELSE cache_creation_tokens_5m + cache_creation_tokens_1h END)"
	// The explicit billing semantic wins over the transport's claude flag.
	// Old Claude-to-OpenAI conversions can only be recognized by split writes.
	anthropic := `CASE
		WHEN COALESCE(usage_semantic, '') <> '' THEN CASE WHEN usage_semantic = 'anthropic' THEN 1 ELSE 0 END
		WHEN usage_billing_path IN ('billing-usage-anthropic', 'billing-usage-anthropic-estimated') THEN 1
		WHEN usage_billing_path IN ('billing-usage-openai', 'billing-usage-openai-estimated', 'billing-usage-gemini', 'billing-usage-gemini-estimated') THEN 0
		WHEN claude IN ('true', '1') OR cache_creation_tokens_5m > 0 OR cache_creation_tokens_1h > 0 THEN 1
		ELSE 0 END`
	input := "CASE WHEN input_tokens_total > 0 THEN input_tokens_total WHEN (" + anthropic + ") = 1 THEN prompt_tokens + cache_tokens + " + cacheWrite + " ELSE prompt_tokens END"
	normalized := LOG_DB.WithContext(period.Statement.Context).Table("(?) AS log_metadata", metadata).
		Select("quota, output_tokens, cache_tokens, " + input + " AS input_tokens")
	return LOG_DB.WithContext(period.Statement.Context).Table("(?) AS log_tokens", normalized).
		Select(`COALESCE(SUM(log_tokens.quota), 0) AS quota,
			COALESCE(SUM(log_tokens.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(log_tokens.output_tokens), 0) AS output_tokens,
			COALESCE(SUM(CASE WHEN log_tokens.cache_tokens > log_tokens.input_tokens THEN log_tokens.input_tokens ELSE log_tokens.cache_tokens END), 0) AS cache_read_tokens`)
}

func logMetadataTokenCount(metadata map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := metadata[key]
	if !ok || len(raw) == 0 || raw[0] == '"' || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if common.Unmarshal(raw, &number) != nil {
		return 0, false
	}
	if value, err := number.Int64(); err == nil {
		return max(value, 0), true
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value >= math.MaxInt64 {
		return 0, false
	}
	return int64(max(value, 0)), true
}

// PostgreSQL 9.6 cannot validate arbitrary JSON text without casting it. Only
// malformed historical JSON triggers this bounded-memory fallback.
func streamLogTokenStats(period *gorm.DB) (Stat, error) {
	rows, err := period.Select("COALESCE(quota, 0), COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), other").Rows()
	if err != nil {
		return Stat{}, err
	}
	defer rows.Close()
	var stat Stat
	for rows.Next() {
		var quota, prompt, output int64
		var other *string
		if err := rows.Scan(&quota, &prompt, &output, &other); err != nil {
			return Stat{}, err
		}
		var metadata map[string]json.RawMessage
		if other != nil && common.UnmarshalJsonStr(*other, &metadata) != nil {
			metadata = nil
		}
		input, _ := logMetadataTokenCount(metadata, "input_tokens_total")
		cacheRead, _ := logMetadataTokenCount(metadata, "cache_tokens")
		cacheWrite, hasWrite := logMetadataTokenCount(metadata, "cache_write_tokens")
		creation, _ := logMetadataTokenCount(metadata, "cache_creation_tokens")
		creation5m, _ := logMetadataTokenCount(metadata, "cache_creation_tokens_5m")
		creation1h, _ := logMetadataTokenCount(metadata, "cache_creation_tokens_1h")
		if creation5m > math.MaxInt64-creation1h {
			return Stat{}, errors.New("cache token count exceeds int64")
		}
		if !hasWrite {
			cacheWrite = max(creation, creation5m+creation1h)
		}
		var semantic, billingPath string
		var claude bool
		var adminInfo map[string]json.RawMessage
		_ = common.Unmarshal(metadata["usage_semantic"], &semantic)
		_ = common.Unmarshal(metadata["claude"], &claude)
		_ = common.Unmarshal(metadata["admin_info"], &adminInfo)
		_ = common.Unmarshal(adminInfo["usage_billing_path"], &billingPath)
		anthropic := claude || creation5m > 0 || creation1h > 0
		if semantic != "" {
			anthropic = semantic == "anthropic"
		} else {
			switch billingPath {
			case "billing-usage-anthropic", "billing-usage-anthropic-estimated":
				anthropic = true
			case "billing-usage-openai", "billing-usage-openai-estimated", "billing-usage-gemini", "billing-usage-gemini-estimated":
				anthropic = false
			}
		}
		if input == 0 {
			input = max(prompt, 0)
			if anthropic {
				if cacheRead > math.MaxInt64-cacheWrite || input > math.MaxInt64-cacheRead-cacheWrite {
					return Stat{}, errors.New("input token count exceeds int64")
				}
				input += cacheRead + cacheWrite
			}
		}
		output = max(output, 0)
		cacheRead = min(cacheRead, input)
		if stat.InputTokens > math.MaxInt64-input || stat.OutputTokens > math.MaxInt64-output || stat.CacheReadTokens > math.MaxInt64-cacheRead ||
			(quota > 0 && stat.Quota > math.MaxInt64-quota) || (quota < 0 && stat.Quota < math.MinInt64-quota) {
			return Stat{}, errors.New("log statistics exceed int64")
		}
		stat.Quota += quota
		stat.InputTokens += input
		stat.OutputTokens += output
		stat.CacheReadTokens += cacheRead
	}
	return stat, rows.Err()
}
