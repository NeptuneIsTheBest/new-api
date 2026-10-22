package model

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Bind the capability to the initialized log handle, not the primary database
// or a process-wide version assumption. Replacing LOG_DB cannot reuse it.
var mysqlLogStatsDB atomic.Pointer[gorm.DB]

type mysqlLogTokenStats struct {
	InputTotal    int64
	CacheTotal    int64
	TotalTokens   int64
	NeedsFallback bool
}

func initMySQLLogStats() {
	if !common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Probe the complete expression without reading a table. Old MySQL and
	// incompatible servers are expected to reject it; do not log that as a
	// failed application query or prevent startup.
	db := LOG_DB.WithContext(ctx).Session(&gorm.Session{Logger: gormlogger.Discard})
	source := db.Table(`(SELECT 100 AS prompt_tokens, 50 AS completion_tokens, ? AS other) AS logs`,
		`{"usage_semantic":"anthropic","claude":true,"cache_tokens":300,"cache_creation_tokens":100,"cache_creation_tokens_5m":60,"cache_creation_tokens_1h":40}`)
	stats, err := sumMySQLLogTokens(source)
	if err == nil && !stats.NeedsFallback && stats.InputTotal == 500 && stats.CacheTotal == 300 && stats.TotalTokens == 550 {
		mysqlLogStatsDB.Store(LOG_DB)
	}
}

// sumMySQLLogTokens mirrors logInputTokenUsage and sumLogTokens. It only
// accepts unambiguous metadata: MySQL normalizes JSON before extraction,
// whereas Go preserves duplicate-key order, folds field-name case, and rejects
// numeric strings and fractional numbers when decoding int64 fields.
func sumMySQLLogTokens(tx *gorm.DB) (stats mysqlLogTokenStats, err error) {
	db := tx.Session(&gorm.Session{NewDB: true})
	source := tx.Select(`
		CAST(GREATEST(COALESCE(prompt_tokens, 0), 0) AS DECIMAL(65, 0)) AS prompt_tokens,
		CAST(GREATEST(COALESCE(completion_tokens, 0), 0) AS DECIMAL(65, 0)) AS completion_tokens,
		COALESCE(other, '') AS other`)

	// NULL ON ERROR does not protect JSON_VALUE from invalid documents. Feed
	// every JSON function a validated document even if MySQL merges subqueries.
	metadata := db.Table("(?) AS token_logs", source).Select(`
		prompt_tokens, completion_tokens, other,
		CASE WHEN JSON_VALID(other) THEN other ELSE '{}' END AS metadata,
		(OCTET_LENGTH(other) > 0 AND NOT JSON_VALID(other)) AS invalid_json`)

	numericFields := []string{
		"input_tokens_total", "cache_tokens", "cache_write_tokens",
		"cache_creation_tokens", "cache_creation_tokens_5m", "cache_creation_tokens_1h",
	}
	fields := append(numericFields, "usage_semantic", "claude")
	// These checks deliberately allow false positives (e.g. a repeated key in
	// nested admin metadata). They must not miss escaped or case-folded keys or
	// duplicate known keys, including an invalid value overwritten by a valid one.
	// Escapes in values also fall back instead of introducing a second JSON lexer.
	duplicateKeys := `"(` + strings.Join(fields, "|") + `)"[[:space:]]*:.*"\1"[[:space:]]*:`
	nonCanonicalKeys := `"(?=[^"]*"[[:space:]]*:)[^"]*[^a-z0-9_"][^"]*"`
	var extraction strings.Builder
	extraction.WriteString(`prompt_tokens, completion_tokens,
		CASE WHEN invalid_json OR JSON_TYPE(metadata) NOT IN ('OBJECT', 'NULL')
		 OR INSTR(other, CHAR(92)) > 0 THEN TRUE
		 ELSE REGEXP_LIKE(other, ?, 'cn') OR REGEXP_LIKE(other, ?, 'c')
		END AS ambiguous_metadata`)
	var invalidFields []string
	for _, field := range numericFields {
		fmt.Fprintf(&extraction, `,
			JSON_TYPE(JSON_EXTRACT(metadata, '$.%[1]s')) AS %[1]s_type,
			CASE WHEN JSON_TYPE(JSON_EXTRACT(metadata, '$.%[1]s')) = 'INTEGER'
			 THEN JSON_VALUE(metadata, '$.%[1]s' RETURNING DECIMAL(65, 0) NULL ON EMPTY NULL ON ERROR)
			 END AS %[1]s`, field)
		invalidFields = append(invalidFields, fmt.Sprintf(`
			(COALESCE(%[1]s_type, 'NULL') NOT IN ('NULL', 'INTEGER')
			 OR (COALESCE(%[1]s_type, 'NULL') = 'INTEGER' AND
			     (%[1]s IS NULL OR %[1]s < -9223372036854775808 OR %[1]s > 9223372036854775807)))`, field))
	}
	extraction.WriteString(`,
		JSON_TYPE(JSON_EXTRACT(metadata, '$.usage_semantic')) AS usage_semantic_type,
		CASE WHEN JSON_TYPE(JSON_EXTRACT(metadata, '$.usage_semantic')) = 'STRING'
		 THEN JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.usage_semantic')) ELSE '' END AS usage_semantic,
		JSON_TYPE(JSON_EXTRACT(metadata, '$.claude')) AS claude_type,
		COALESCE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.claude')) = 'true', FALSE) AS claude`)
	extracted := db.Table("(?) AS token_metadata", metadata).
		Select(extraction.String(), duplicateKeys, nonCanonicalKeys)
	invalidFields = append(invalidFields,
		`COALESCE(usage_semantic_type, 'NULL') NOT IN ('NULL', 'STRING')`,
		`COALESCE(claude_type, 'NULL') NOT IN ('NULL', 'BOOLEAN')`)

	// All arithmetic is decimal before addition, including cache breakdowns.
	// Binary comparison preserves Go's case- and trailing-space-sensitive strings.
	usage := db.Table("(?) AS token_fields", extracted).Select(`
		completion_tokens,
		(ambiguous_metadata OR ` + strings.Join(invalidFields, " OR ") + `) AS needs_fallback,
		GREATEST(COALESCE(cache_tokens, 0), 0) AS cache_tokens,
		CASE
		 WHEN input_tokens_total >= 0 THEN input_tokens_total
		 WHEN CAST(usage_semantic AS BINARY) = CAST('anthropic' AS BINARY)
		   OR (OCTET_LENGTH(usage_semantic) = 0 AND claude)
		 THEN LEAST(9223372036854775807,
		   prompt_tokens + GREATEST(COALESCE(cache_tokens, 0), 0) +
		   CASE WHEN cache_write_tokens IS NOT NULL THEN GREATEST(cache_write_tokens, 0)
		   ELSE GREATEST(COALESCE(cache_creation_tokens, 0),
		     LEAST(9223372036854775807,
		       GREATEST(COALESCE(cache_creation_tokens_5m, 0), 0) +
		       GREATEST(COALESCE(cache_creation_tokens_1h, 0), 0))) END)
		 ELSE prompt_tokens
		END AS input_tokens`)
	err = db.Table("(?) AS token_usage", usage).Select(`
		CAST(LEAST(COALESCE(SUM(input_tokens), 0), 9223372036854775807) AS SIGNED) AS input_total,
		CAST(LEAST(COALESCE(SUM(cache_tokens), 0), 9223372036854775807) AS SIGNED) AS cache_total,
		CAST(LEAST(COALESCE(SUM(input_tokens + completion_tokens), 0), 9223372036854775807) AS SIGNED) AS total_tokens,
		COALESCE(MAX(needs_fallback), FALSE) AS needs_fallback`).Scan(&stats).Error
	return stats, err
}
