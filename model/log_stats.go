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
	"gorm.io/driver/mysql"
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

type logTokenStatOptions struct {
	GroupByToken bool
	IncludeCache bool
}

func (stat *Stat) finalizeTokenCounts() error {
	if stat.InputTokens < 0 || stat.OutputTokens < 0 || stat.CacheReadTokens < 0 || stat.CacheReadTokens > stat.InputTokens || stat.InputTokens > math.MaxInt64-stat.OutputTokens {
		return errors.New("统计 Token 数量超出范围")
	}
	stat.TotalTokens = stat.InputTokens + stat.OutputTokens
	return nil
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
		options := logTokenStatOptions{IncludeCache: true}
		err = logTokenStatQuery(period, options).Scan(&stat).Error
		var pgErr *pgconn.PgError
		// All text-to-number casts are guarded by JSON numeric types. In this
		// query 22P02 can only come from historical invalid JSON in other.
		if common.UsingLogDatabase(common.DatabaseTypePostgreSQL) && errors.As(err, &pgErr) && pgErr.Code == "22P02" {
			common.SysError("invalid historical log JSON; using streaming token statistics")
			var grouped map[int]Stat
			grouped, err = streamLogTokenStats(period, options)
			stat = grouped[0]
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
	if err := stat.finalizeTokenCounts(); err != nil {
		return Stat{}, err
	}
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

func logStatSupportsJSONTable(db *gorm.DB) bool {
	if !common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		return false
	}
	var config *mysql.Config
	switch dialector := db.Dialector.(type) {
	case mysqlMigrationDialector:
		config = dialector.Config
	case *mysqlMigrationDialector:
		if dialector != nil {
			config = dialector.Config
		}
	case mysql.Dialector:
		config = dialector.Config
	case *mysql.Dialector:
		if dialector != nil {
			config = dialector.Config
		}
	}
	if config == nil {
		return false
	}
	version := strings.ToLower(config.ServerVersion)
	if strings.Contains(version, "mariadb") || strings.Contains(version, "tidb") {
		return false
	}
	version, _, _ = strings.Cut(version, "-")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]int
	for i, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return false
		}
		numbers[i] = number
	}
	// Earlier JSON_TABLE versions reject JSON null instead of returning SQL NULL.
	return numbers[0] > 8 || numbers[0] == 8 && (numbers[1] > 0 || numbers[2] >= 21)
}

// logStatJSONText only receives fixed metadata paths, never user input.
func logStatJSONText(useJSONTable bool, keys ...string) string {
	switch common.LogDatabaseType() {
	case common.DatabaseTypeMySQL:
		value := "JSON_EXTRACT(other, '$." + strings.Join(keys, ".") + "')"
		if useJSONTable {
			value = "log_metadata." + strings.Join(keys, "_")
		}
		return "NULLIF(JSON_UNQUOTE(" + value + "), 'null')"
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
func logStatJSONNumber(useJSONTable bool, key string) string {
	value := logStatJSONText(useJSONTable, key)
	switch common.LogDatabaseType() {
	case common.DatabaseTypeMySQL:
		raw := "JSON_EXTRACT(other, '$." + key + "')"
		if useJSONTable {
			raw = "log_metadata." + key
		}
		return fmt.Sprintf("CASE WHEN JSON_TYPE(%s) IN ('INTEGER', 'DOUBLE') THEN GREATEST(0, CAST(%s AS SIGNED)) END", raw, value)
	case common.DatabaseTypePostgreSQL:
		return fmt.Sprintf("CASE WHEN json_typeof(other::json -> '%s') = 'number' THEN GREATEST(0, TRUNC((%s)::numeric)) END", key, value)
	case common.DatabaseTypeClickHouse:
		return fmt.Sprintf("CASE WHEN toString(JSONType(other, '%s')) IN ('Int64', 'UInt64', 'Float64') THEN greatest(0, JSONExtractInt(other, '%s')) END", key, key)
	default:
		return fmt.Sprintf("CASE WHEN json_type(other, '$.%s') IN ('integer', 'real') THEN MAX(0, CAST(json_extract(other, '$.%s') AS INTEGER)) END", key, key)
	}
}

func logTokenStatQuery(period *gorm.DB, options logTokenStatOptions) *gorm.DB {
	safeJSON := "CASE WHEN json_valid(other) THEN other ELSE '{}' END"
	switch common.LogDatabaseType() {
	case common.DatabaseTypeMySQL:
		safeJSON = "CASE WHEN JSON_VALID(other) THEN other ELSE '{}' END"
	case common.DatabaseTypePostgreSQL:
		safeJSON = "COALESCE(NULLIF(TRIM(other), ''), '{}')"
	case common.DatabaseTypeClickHouse:
		safeJSON = "if(isValidJSON(logs.other), logs.other, '{}')"
	}
	columns := "quota, CASE WHEN prompt_tokens > 0 THEN prompt_tokens ELSE 0 END AS prompt_tokens, CASE WHEN completion_tokens > 0 THEN completion_tokens ELSE 0 END AS output_tokens, " + safeJSON + " AS other"
	if options.GroupByToken {
		columns = "token_id, " + columns
	}
	documents := period.Select(columns)
	least, greatest := "least", "greatest"
	if common.UsingLogDatabase(common.DatabaseTypeSQLite) {
		least, greatest = "min", "max"
	}
	useJSONTable := logStatSupportsJSONTable(period)
	inputTotal := "NULLIF(" + logStatJSONNumber(useJSONTable, "input_tokens_total") + ", 0)"
	cacheRead := "COALESCE(" + logStatJSONNumber(useJSONTable, "cache_tokens") + ", 0)"
	creation := "COALESCE(" + logStatJSONNumber(useJSONTable, "cache_creation_tokens") + ", 0)"
	creation5m := "COALESCE(" + logStatJSONNumber(useJSONTable, "cache_creation_tokens_5m") + ", 0)"
	creation1h := "COALESCE(" + logStatJSONNumber(useJSONTable, "cache_creation_tokens_1h") + ", 0)"
	cacheWrite := "COALESCE(" + logStatJSONNumber(useJSONTable, "cache_write_tokens") + ", " + greatest + "(" + creation + ", " + creation5m + " + " + creation1h + "))"
	// The explicit billing semantic wins over the transport's claude flag.
	// Old Claude-to-OpenAI conversions can only be recognized by split writes.
	anthropic := fmt.Sprintf(`CASE COALESCE(%s, '')
		WHEN 'anthropic' THEN 1
		WHEN '' THEN CASE %s
			WHEN 'billing-usage-anthropic' THEN 1
			WHEN 'billing-usage-anthropic-estimated' THEN 1
			WHEN 'billing-usage-openai' THEN 0
			WHEN 'billing-usage-openai-estimated' THEN 0
			WHEN 'billing-usage-gemini' THEN 0
			WHEN 'billing-usage-gemini-estimated' THEN 0
			ELSE CASE WHEN %s IN ('true', '1') OR %s > 0 OR %s > 0 THEN 1 ELSE 0 END
		END
		ELSE 0 END`, logStatJSONText(useJSONTable, "usage_semantic"), logStatJSONText(useJSONTable, "admin_info", "usage_billing_path"), logStatJSONText(useJSONTable, "claude"), creation5m, creation1h)
	// Keep historical metadata inside the fallback expressions: a derived-table
	// alias alone does not prevent the optimizer from repeating JSON extraction.
	input := "COALESCE(" + inputTotal + ", prompt_tokens + CASE WHEN (" + anthropic + ") = 1 THEN " + cacheRead + " + " + cacheWrite + " ELSE 0 END)"
	fields := "quota, output_tokens, " + input + " AS input_tokens"
	selection := `COALESCE(SUM(log_tokens.quota), 0) AS quota,
			COALESCE(SUM(log_tokens.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(log_tokens.output_tokens), 0) AS output_tokens`
	if options.IncludeCache {
		// Reconstructed Anthropic input already includes every cache-read token.
		// Its cache cap needs neither cache-write metadata nor a second input sum.
		cacheCap := "COALESCE(" + inputTotal + ", CASE WHEN (" + anthropic + ") = 1 THEN " + cacheRead + " ELSE prompt_tokens END)"
		fields += ", " + least + "(" + cacheRead + ", " + cacheCap + ") AS cache_read_tokens"
		selection += ", COALESCE(SUM(log_tokens.cache_read_tokens), 0) AS cache_read_tokens"
	}
	if options.GroupByToken {
		fields = "token_id, " + fields
	}
	normalized := LOG_DB.WithContext(period.Statement.Context).Table("(?) AS log_documents", documents).Select(fields)
	if useJSONTable {
		// Cast the derived-table source to avoid MySQL JSON_TABLE error 1210.
		// JSON columns preserve numeric types and explicit zero values. The root
		// path and left join keep exactly one row per log, including JSON null.
		normalized = normalized.Joins(`LEFT JOIN JSON_TABLE(CAST(log_documents.other AS JSON), '$' COLUMNS (
			input_tokens_total JSON PATH '$.input_tokens_total' NULL ON EMPTY NULL ON ERROR,
			cache_tokens JSON PATH '$.cache_tokens' NULL ON EMPTY NULL ON ERROR,
			cache_creation_tokens JSON PATH '$.cache_creation_tokens' NULL ON EMPTY NULL ON ERROR,
			cache_creation_tokens_5m JSON PATH '$.cache_creation_tokens_5m' NULL ON EMPTY NULL ON ERROR,
			cache_creation_tokens_1h JSON PATH '$.cache_creation_tokens_1h' NULL ON EMPTY NULL ON ERROR,
			cache_write_tokens JSON PATH '$.cache_write_tokens' NULL ON EMPTY NULL ON ERROR,
			usage_semantic JSON PATH '$.usage_semantic' NULL ON EMPTY NULL ON ERROR,
			admin_info_usage_billing_path JSON PATH '$.admin_info.usage_billing_path' NULL ON EMPTY NULL ON ERROR,
			claude JSON PATH '$.claude' NULL ON EMPTY NULL ON ERROR
		)) AS log_metadata ON TRUE`)
	}
	query := LOG_DB.WithContext(period.Statement.Context).Table("(?) AS log_tokens", normalized)
	if options.GroupByToken {
		selection = "log_tokens.token_id, " + selection
		query = query.Group("log_tokens.token_id")
	}
	return query.Select(selection)
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

// logMetadataTokenUsage applies historical input semantics only when no
// normalized input total is available. Cache metrics are optional for key stats.
func logMetadataTokenUsage(metadata map[string]json.RawMessage, prompt int64, includeCache bool) (int64, int64, error) {
	input, _ := logMetadataTokenCount(metadata, "input_tokens_total")
	var cacheRead int64
	if includeCache {
		cacheRead, _ = logMetadataTokenCount(metadata, "cache_tokens")
	}
	if input > 0 {
		return input, min(cacheRead, input), nil
	}
	input = max(prompt, 0)
	var semantic string
	_ = common.Unmarshal(metadata["usage_semantic"], &semantic)
	anthropic := semantic == "anthropic"
	var creation5m, creation1h int64
	var hasCreationBreakdown bool
	if semantic == "" {
		var billingPath string
		var adminInfo map[string]json.RawMessage
		_ = common.Unmarshal(metadata["admin_info"], &adminInfo)
		_ = common.Unmarshal(adminInfo["usage_billing_path"], &billingPath)
		switch billingPath {
		case "billing-usage-anthropic", "billing-usage-anthropic-estimated":
			anthropic = true
		case "billing-usage-openai", "billing-usage-openai-estimated", "billing-usage-gemini", "billing-usage-gemini-estimated":
			anthropic = false
		default:
			_ = common.Unmarshal(metadata["claude"], &anthropic)
			if !anthropic {
				creation5m, _ = logMetadataTokenCount(metadata, "cache_creation_tokens_5m")
				creation1h, _ = logMetadataTokenCount(metadata, "cache_creation_tokens_1h")
				hasCreationBreakdown = true
				anthropic = creation5m > 0 || creation1h > 0
			}
		}
	}
	if !anthropic {
		return input, min(cacheRead, input), nil
	}
	if !includeCache {
		cacheRead, _ = logMetadataTokenCount(metadata, "cache_tokens")
	}
	cacheWrite, hasWrite := logMetadataTokenCount(metadata, "cache_write_tokens")
	if !hasWrite {
		creation, _ := logMetadataTokenCount(metadata, "cache_creation_tokens")
		if !hasCreationBreakdown {
			creation5m, _ = logMetadataTokenCount(metadata, "cache_creation_tokens_5m")
			creation1h, _ = logMetadataTokenCount(metadata, "cache_creation_tokens_1h")
		}
		if creation5m > math.MaxInt64-creation1h {
			return 0, 0, errors.New("cache token count exceeds int64")
		}
		cacheWrite = max(creation, creation5m+creation1h)
	}
	if cacheRead > math.MaxInt64-cacheWrite || input > math.MaxInt64-cacheRead-cacheWrite {
		return 0, 0, errors.New("input token count exceeds int64")
	}
	input += cacheRead + cacheWrite
	if !includeCache {
		cacheRead = 0
	}
	return input, cacheRead, nil
}

// PostgreSQL 9.6 cannot validate arbitrary JSON text without casting it. Only
// malformed historical JSON triggers this bounded-memory fallback.
func streamLogTokenStats(period *gorm.DB, options logTokenStatOptions) (map[int]Stat, error) {
	tokenIDColumn := "0"
	if options.GroupByToken {
		tokenIDColumn = "COALESCE(token_id, 0)"
	}
	rows, err := period.Select(tokenIDColumn + ", COALESCE(quota, 0), COALESCE(prompt_tokens, 0), COALESCE(completion_tokens, 0), other").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grouped := make(map[int]Stat)
	for rows.Next() {
		if err := period.Statement.Context.Err(); err != nil {
			return nil, err
		}
		var tokenID int
		var quota, prompt, output int64
		var other *string
		if err := rows.Scan(&tokenID, &quota, &prompt, &output, &other); err != nil {
			return nil, err
		}
		stat := grouped[tokenID]
		var metadata map[string]json.RawMessage
		if other != nil && common.UnmarshalJsonStr(*other, &metadata) != nil {
			metadata = nil
		}
		input, cacheRead, err := logMetadataTokenUsage(metadata, prompt, options.IncludeCache)
		if err != nil {
			return nil, err
		}
		output = max(output, 0)
		if stat.InputTokens > math.MaxInt64-input || stat.OutputTokens > math.MaxInt64-output || stat.CacheReadTokens > math.MaxInt64-cacheRead ||
			(quota > 0 && stat.Quota > math.MaxInt64-quota) || (quota < 0 && stat.Quota < math.MinInt64-quota) {
			return nil, errors.New("log statistics exceed int64")
		}
		stat.Quota += quota
		stat.InputTokens += input
		stat.OutputTokens += output
		stat.CacheReadTokens += cacheRead
		grouped[tokenID] = stat
	}
	return grouped, rows.Err()
}
