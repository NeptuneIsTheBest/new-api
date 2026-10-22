package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func requestLogStats(t *testing.T, handler gin.HandlerFunc, userID, role int, query string) model.Stat {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/log/stat?"+query, nil)
	ctx.Set("id", userID)
	ctx.Set("role", role)
	// A changed username must not change ownership of historical logs.
	ctx.Set("username", "renamed-user")
	handler(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool       `json:"success"`
		Data    model.Stat `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, recorder.Body.String())
	return response.Data
}

func TestLogStatsDatabaseMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousMaster := common.IsMasterNode
	common.IsMasterNode = true
	t.Cleanup(func() { common.IsMasterNode = previousMaster })
	for _, database := range []struct {
		kind, env string
		typ       common.DatabaseType
	}{
		{"sqlite", "", common.DatabaseTypeSQLite},
		{"mysql", "AUDIT_MYSQL_DSN", common.DatabaseTypeMySQL},
		{"postgres", "AUDIT_POSTGRES_DSN", common.DatabaseTypePostgreSQL},
		{"clickhouse", "AUDIT_CLICKHOUSE_DSN", common.DatabaseTypeClickHouse},
	} {
		t.Run(database.kind, func(t *testing.T) {
			dsn := os.Getenv(database.env)
			if database.env != "" && dsn == "" {
				t.Skip(database.env + " is not configured")
			}
			db, isolatedDSN := newAuditTestDatabase(t, database.kind, dsn)
			queries := &logStatsQueryRecorder{Interface: gormlogger.Discard}
			db = db.Session(&gorm.Session{Logger: queries})
			mainDB, mainType := db, database.typ
			if database.kind == "clickhouse" {
				mainDB, _ = newAuditTestDatabase(t, "sqlite", "")
				mainType = common.DatabaseTypeSQLite
			}
			useLogExportDatabases(t, mainDB, db, mainType, database.typ)
			if database.kind == "clickhouse" {
				t.Setenv("LOG_SQL_DSN", isolatedDSN)
				require.NoError(t, model.InitLogDB())
			} else {
				// Initialize dialect-specific column quoting without migrating
				// unrelated tables. The relational fixtures share their main/log DB.
				t.Setenv("LOG_SQL_DSN", "")
				common.IsMasterNode = false
				err := model.InitLogDB()
				common.IsMasterNode = true
				require.NoError(t, err)
				require.NoError(t, db.AutoMigrate(&model.Log{}))
			}
			versionSQL := "SELECT version()"
			if database.kind == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, model.LOG_DB.Raw(versionSQL).Scan(&version).Error)
			t.Logf("log database: %s %s", database.kind, version)
			jsonValueSupported := false
			if database.kind == "mysql" {
				var value int
				err := db.Session(&gorm.Session{Logger: gormlogger.Discard}).
					Raw(`SELECT JSON_VALUE('{"n":1}', '$.n' RETURNING SIGNED)`).Scan(&value).Error
				jsonValueSupported = err == nil && value == 1
			}

			t.Run("token semantics", func(t *testing.T) {
				for i, tc := range []struct {
					name               string
					prompt, completion int
					other              string
					total              int64
					rate               float64
				}{
					{"OpenAI cache details are included in input", 1000, 200, `{"cache_tokens":500,"image_cache_tokens":200,"cache_creation_tokens":100}`, 1200, 50},
					{"Anthropic adds separate cache reads and writes", 100, 50, `{"usage_semantic":"anthropic","cache_tokens":300,"cache_creation_tokens":100}`, 550, 60},
					{"legacy Claude logs add separate cache quantities", 100, 50, `{"claude":true,"cache_tokens":300,"cache_creation_tokens":100}`, 550, 60},
					{"explicit semantic overrides legacy Claude flag", 1000, 200, `{"usage_semantic":"openai","claude":true,"cache_tokens":500}`, 1200, 50},
					{"normalized input overrides separate quantities", 100, 50, `{"input_tokens_total":1000,"claude":true,"cache_tokens":500,"cache_creation_tokens":100}`, 1050, 50},
					{"explicit zero input has zero hit rate", 100, 50, `{"input_tokens_total":0,"cache_tokens":500}`, 50, 0},
					{"invalid normalized input falls back to prompt", 100, 50, `{"input_tokens_total":-1,"cache_tokens":50}`, 150, 50},
					{"normalized cache writes override historical fields", 100, 50, `{"claude":true,"cache_tokens":300,"cache_write_tokens":100,"cache_creation_tokens":200,"cache_creation_tokens_5m":300}`, 550, 60},
					{"explicit zero cache writes stays zero", 100, 50, `{"claude":true,"cache_tokens":300,"cache_write_tokens":0,"cache_creation_tokens":200}`, 450, 75},
					{"split cache writes count once", 100, 50, `{"claude":true,"cache_tokens":300,"cache_creation_tokens":50,"cache_creation_tokens_5m":60,"cache_creation_tokens_1h":40}`, 550, 60},
					{"larger aggregate cache write count is retained", 100, 50, `{"claude":true,"cache_tokens":300,"cache_creation_tokens":200,"cache_creation_tokens_5m":60,"cache_creation_tokens_1h":40}`, 650, 50},
					{"cache-only usage counts as input", 0, 0, `{"claude":true,"cache_tokens":300,"cache_creation_tokens":100}`, 400, 75},
					{"negative quantities do not reduce token totals", -50, -5, `{"claude":true,"cache_tokens":-40,"cache_creation_tokens":-20,"cache_creation_tokens_5m":-10,"cache_creation_tokens_1h":100}`, 100, 0},
					{"empty metadata retains stored tokens", 100, 20, "", 120, 0},
					{"malformed metadata discards partially decoded cache", 100, 20, `{"cache_tokens":50,`, 120, 0},
					{"invalid metadata types retain stored tokens", 100, 20, `{"cache_tokens":"invalid","claude":true}`, 120, 0},
					{"excess cache reads cap the hit rate", 100, 20, `{"cache_tokens":200}`, 120, 100},
					{"zero input does not divide by zero", 0, 20, `{"cache_tokens":200}`, 20, 0},
					{"oversized historical cache counts do not wrap", 100, 20, `{"claude":true,"cache_tokens":9223372036854775807,"cache_creation_tokens":100}`, 9223372036854775807, 100},
					{"numeric strings invalidate all metadata", 100, 20, `{"cache_tokens":50,"cache_creation_tokens":"10"}`, 120, 0},
					{"fractional numbers invalidate all metadata", 100, 20, `{"cache_tokens":50,"input_tokens_total":100.5}`, 120, 0},
					{"integer valued floats are not int64 fields", 100, 20, `{"cache_tokens":50.0}`, 120, 0},
					{"exponents are not int64 fields", 100, 20, `{"cache_tokens":5e1}`, 120, 0},
					{"out of range integers invalidate all metadata", 100, 20, `{"cache_tokens":50,"input_tokens_total":9223372036854775808}`, 120, 0},
					{"negative out of range integers invalidate metadata", 100, 20, `{"cache_tokens":50,"cache_creation_tokens":-9223372036854775809}`, 120, 0},
					{"minimum int64 quantities clamp to zero", 100, 20, `{"cache_tokens":-9223372036854775808}`, 120, 0},
					{"null fields preserve default values", 100, 20, `{"cache_tokens":null,"input_tokens_total":null,"claude":null,"usage_semantic":null}`, 120, 0},
					{"null document preserves stored tokens", 100, 20, `null`, 120, 0},
					{"array metadata preserves stored tokens", 100, 20, `[{"cache_tokens":50}]`, 120, 0},
					{"boolean numeric fields invalidate metadata", 100, 20, `{"cache_tokens":50,"cache_write_tokens":false}`, 120, 0},
					{"invalid semantic type discards cache", 100, 20, `{"cache_tokens":50,"usage_semantic":false}`, 120, 0},
					{"invalid Claude flag discards cache", 100, 20, `{"cache_tokens":50,"claude":"true"}`, 120, 0},
					{"duplicate null retains preceding nonpointer count", 100, 20, `{"cache_tokens":50,"cache_tokens":null}`, 120, 50},
					{"duplicate null clears preceding pointer count", 100, 20, `{"input_tokens_total":200,"input_tokens_total":null,"cache_tokens":50}`, 120, 50},
					{"overwritten invalid fields still invalidate metadata", 100, 20, `{"cache_tokens":"bad","cache_tokens":50}`, 120, 0},
					{"case folded keys retain Go semantics", 100, 20, `{"CACHE_TOKENS":50}`, 120, 50},
					{"mixed case duplicate keys retain input order", 100, 20, `{"cache_tokens":50,"Cache_Tokens":60}`, 120, 60},
					{"escaped keys retain Go semantics", 100, 20, `{"cache\u005ftokens":50}`, 120, 50},
					{"Unicode folded keys retain Go semantics", 100, 20, `{"cache_tokenſ":50}`, 120, 50},
					{"semantic comparison is case sensitive", 100, 20, `{"usage_semantic":"ANTHROPIC","claude":true,"cache_tokens":50}`, 120, 50},
					{"semantic trailing spaces are significant", 100, 20, `{"usage_semantic":"anthropic ","claude":true,"cache_tokens":50}`, 120, 50},
					{"semantic whitespace is not empty", 100, 20, `{"usage_semantic":" ","claude":true,"cache_tokens":50}`, 120, 50},
					{"long semantic strings do not truncate to empty", 100, 20, `{"usage_semantic":"` + strings.Repeat("x", 600) + `","claude":true,"cache_tokens":50}`, 120, 50},
				} {
					t.Run(tc.name, func(t *testing.T) {
						name := fmt.Sprintf("semantics-%d", i)
						require.NoError(t, model.LOG_DB.Create(&model.Log{
							UserId: 1, Username: "alice", CreatedAt: time.Now().Unix() - 120,
							Type: model.LogTypeConsume, ModelName: name, RequestId: name,
							PromptTokens: tc.prompt, CompletionTokens: tc.completion, Other: tc.other,
						}).Error)
						stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name="+name)
						assert.Equal(t, tc.total, stat.TotalTokens)
						assert.InDelta(t, tc.rate, stat.CacheHitRate, 0.000001)
						if jsonValueSupported {
							// A fresh, uninitialized handle uses the original Go path on
							// the same data, independently of MySQL's JSON normalization.
							initializedDB := model.LOG_DB
							model.LOG_DB = initializedDB.Session(&gorm.Session{})
							baseline, err := model.SumUsedQuota(context.Background(), 0, model.LogFilter{ModelName: name})
							model.LOG_DB = initializedDB
							require.NoError(t, err)
							assert.Equal(t, baseline, stat)
						}
					})
				}
			})

			t.Run("multiple rows saturate totals independently", func(t *testing.T) {
				for i := range 2 {
					require.NoError(t, model.LOG_DB.Create(&model.Log{
						Type: model.LogTypeConsume, ModelName: "saturation", RequestId: fmt.Sprintf("saturation-%d", i),
						PromptTokens: 100, CompletionTokens: 20,
						Other: `{"input_tokens_total":6000000000000000000,"cache_tokens":3000000000000000000}`,
					}).Error)
				}
				stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name=saturation")
				assert.Equal(t, int64(math.MaxInt64), stat.TotalTokens)
				assert.InDelta(t, float64(6000000000000000000)/float64(math.MaxInt64)*100, stat.CacheHitRate, 0.000001)
			})

			t.Run("aggregate path and historical fallback", func(t *testing.T) {
				queries.queries = nil
				stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name=semantics-0")
				assert.Equal(t, int64(1200), stat.TotalTokens)
				assert.Len(t, queries.queries, 3, "quota, rolling rates, and token totals each execute once")
				if jsonValueSupported {
					require.Len(t, queries.queries, 3)
					assert.Contains(t, queries.queries[2].sql, "JSON_VALUE(")
					assert.EqualValues(t, 1, queries.queries[2].rows, "SQL returns a single aggregate row")

					require.NoError(t, model.LOG_DB.Create(&model.Log{
						Type: model.LogTypeConsume, ModelName: "mixed-metadata", RequestId: "mixed-valid",
						PromptTokens: 100, CompletionTokens: 20, Other: `{"cache_tokens":50}`,
					}).Error)
					require.NoError(t, model.LOG_DB.Create(&model.Log{
						Type: model.LogTypeConsume, ModelName: "mixed-metadata", RequestId: "mixed-duplicate",
						PromptTokens: 100, CompletionTokens: 20, Other: `{"cache_tokens":50,"cache_tokens":null}`,
					}).Error)
					queries.queries = nil
					stat = requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name=mixed-metadata")
					assert.Equal(t, int64(240), stat.TotalTokens, "fallback replaces the whole token aggregate")
					assert.Equal(t, 50.0, stat.CacheHitRate)
					assert.Len(t, queries.queries, 4, "ambiguous metadata triggers one streaming fallback")
				} else {
					for _, query := range queries.queries {
						assert.NotContains(t, query.sql, "JSON_VALUE(")
					}
				}
			})

			t.Run("filters and scope", func(t *testing.T) {
				now := time.Now().Unix()
				logs := []model.Log{
					{UserId: 1, Username: "alice", CreatedAt: now - 1200, Type: model.LogTypeConsume, TokenName: "main", ModelName: "gpt-a", ChannelId: 9, Group: "default", Quota: 10, PromptTokens: 1000, CompletionTokens: 200, RequestId: "openai", UpstreamRequestId: "up-openai", Other: `{"cache_tokens":500}`},
					{UserId: 1, Username: "alice", CreatedAt: now - 900, Type: model.LogTypeConsume, TokenName: "main", ModelName: "claude-a", ChannelId: 9, Group: "default", Quota: 20, PromptTokens: 100, CompletionTokens: 50, RequestId: "claude", UpstreamRequestId: "up-claude", Other: `{"claude":true,"cache_tokens":300,"cache_creation_tokens":100}`},
					{UserId: 1, Username: "alice", CreatedAt: now - 10, Type: model.LogTypeConsume, TokenName: "main", ModelName: "gpt-a", ChannelId: 9, Group: "default", Quota: 7, PromptTokens: 10, CompletionTokens: 2, RequestId: "recent", Other: `{"cache_tokens":5}`},
					{UserId: 2, Username: "bob", CreatedAt: now - 10, Type: model.LogTypeConsume, TokenName: "other", ModelName: "gpt-b", ChannelId: 10, Group: "premium", Quota: 90, PromptTokens: 2000, CompletionTokens: 200, RequestId: "bob", Other: `{"cache_tokens":1000}`},
					{UserId: 1, Username: "alice", CreatedAt: now - 8000, Type: model.LogTypeConsume, TokenName: "main", ModelName: "gpt-a", ChannelId: 9, Group: "default", Quota: 30, PromptTokens: 100, CompletionTokens: 10, RequestId: "old"},
					{UserId: 1, Username: "alice", CreatedAt: now - 1100, Type: model.LogTypeError, TokenName: "main", ModelName: "gpt-a", ChannelId: 9, Group: "default", Quota: 123, PromptTokens: 999, CompletionTokens: 99, RequestId: "error"},
					{UserId: 1, Username: "alice", CreatedAt: now - 1000, Type: model.LogTypeConsume, TokenName: "secondary", ModelName: "gpt-a", ChannelId: 10, Group: "premium", Quota: 40, PromptTokens: 200, CompletionTokens: 20, RequestId: "secondary", Other: `{"cache_tokens":100}`},
				}
				for i := range logs {
					require.NoError(t, model.LOG_DB.Create(&logs[i]).Error)
				}
				period := fmt.Sprintf("start_timestamp=%d&end_timestamp=%d", now-3600, now-60)
				for _, tc := range []struct {
					name, query     string
					total           int64
					quota, rpm, tpm int
					rate            float64
				}{
					{"weighted totals ignore pagination and selected log type", "token_name=main&page_size=1&p=2&type=5", 1750, 30, 1, 12, 800.0 / 1500 * 100},
					{"model and username wildcards", "model_name=gpt-%25&username=ali%25", 1420, 50, 1, 12, 50},
					{"channel filter", "channel=9", 1750, 30, 1, 12, 800.0 / 1500 * 100},
					{"group filter", "group=premium", 220, 40, 1, 2200, 50},
					{"request id", "request_id=openai", 1200, 10, 0, 0, 50},
					{"upstream request id", "upstream_request_id=up-claude", 550, 20, 0, 0, 60},
					{"empty result", "request_id=missing", 0, 0, 0, 0, 0},
				} {
					t.Run(tc.name, func(t *testing.T) {
						stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, period+"&"+tc.query)
						assert.Equal(t, tc.total, stat.TotalTokens)
						assert.Equal(t, tc.quota, stat.Quota)
						assert.Equal(t, tc.rpm, stat.Rpm)
						assert.Equal(t, tc.tpm, stat.Tpm)
						assert.InDelta(t, tc.rate, stat.CacheHitRate, 0.000001)
					})
				}
				for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser} {
					stat := requestLogStats(t, GetLogsSelfStat, 1, role, period+"&token_name=main&username=bob&channel=10")
					assert.Equal(t, int64(1750), stat.TotalTokens)
					assert.Equal(t, 30, stat.Quota)
					assert.Equal(t, 1, stat.Rpm)
					assert.Equal(t, 12, stat.Tpm)
					assert.InDelta(t, 800.0/1500*100, stat.CacheHitRate, 0.000001)
				}
			})

			if database.kind != "clickhouse" {
				t.Run("historical null columns", func(t *testing.T) {
					require.NoError(t, model.LOG_DB.Create(&model.Log{Type: model.LogTypeConsume, RequestId: "nullable"}).Error)
					require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", "nullable").Updates(map[string]any{"prompt_tokens": nil, "completion_tokens": nil, "other": nil}).Error)
					stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "request_id=nullable")
					assert.Zero(t, stat.TotalTokens)
					assert.Zero(t, stat.CacheHitRate)
				})
			}
			t.Run("canceled request fails statistics", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := model.SumUsedQuota(ctx, 0, model.LogFilter{})
				require.Error(t, err)
			})
			if jsonValueSupported {
				t.Run("failed capability probe resets the initialized handle", func(t *testing.T) {
					previousMaster := common.IsMasterNode
					common.IsMasterNode = false
					t.Cleanup(func() { common.IsMasterNode = previousMaster })
					t.Run("probe failure permits startup", func(t *testing.T) {
						require.NoError(t, db.Callback().Row().Before("gorm:row").Register("test:unsupported_log_aggregate", func(tx *gorm.DB) {
							tx.AddError(errors.New("unsupported aggregation capability"))
						}))
						t.Cleanup(func() { assert.NoError(t, db.Callback().Row().Remove("test:unsupported_log_aggregate")) })
						require.NoError(t, model.InitLogDB())
					})
					queries.queries = nil
					stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name=semantics-0")
					assert.Equal(t, int64(1200), stat.TotalTokens)
					for _, query := range queries.queries {
						assert.NotContains(t, query.sql, "JSON_VALUE(")
					}
					require.NoError(t, model.InitLogDB())
					queries.queries = nil
					stat = requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name=semantics-0")
					assert.Equal(t, int64(1200), stat.TotalTokens)
					require.Len(t, queries.queries, 3)
					assert.Contains(t, queries.queries[2].sql, "JSON_VALUE(")
				})
				t.Run("aggregate errors do not retry with a scan", func(t *testing.T) {
					for _, failure := range []error{errors.New("aggregate query failed"), context.DeadlineExceeded, context.Canceled} {
						t.Run(failure.Error(), func(t *testing.T) {
							attempts := 0
							require.NoError(t, db.Callback().Row().Before("gorm:row").Register("test:fail_log_aggregate", func(tx *gorm.DB) {
								attempts++
								if attempts == 3 {
									tx.AddError(failure)
								}
							}))
							t.Cleanup(func() { assert.NoError(t, db.Callback().Row().Remove("test:fail_log_aggregate")) })
							_, err := model.SumUsedQuota(context.Background(), 0, model.LogFilter{ModelName: "semantics-0"})
							require.Error(t, err)
							assert.Equal(t, 3, attempts)
						})
					}
				})
			}
			if database.kind == "mysql" {
				t.Run("independent log database and reinitialization", func(t *testing.T) {
					mainDB, _ := newAuditTestDatabase(t, "sqlite", "")
					require.NoError(t, mainDB.AutoMigrate(&model.Log{}))
					useLogExportDatabases(t, mainDB, db, common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
					t.Setenv("LOG_SQL_DSN", isolatedDSN)
					previousMaster := common.IsMasterNode
					common.IsMasterNode = false
					t.Cleanup(func() { common.IsMasterNode = previousMaster })
					require.NoError(t, model.InitLogDB())
					connection, err := model.LOG_DB.DB()
					require.NoError(t, err)
					t.Cleanup(func() { assert.NoError(t, connection.Close()) })
					model.LOG_DB.Config.Logger = queries
					queries.queries = nil
					stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "model_name=semantics-0")
					assert.Equal(t, int64(1200), stat.TotalTokens)
					require.Len(t, queries.queries, 3)
					assert.Equal(t, jsonValueSupported, strings.Contains(queries.queries[2].sql, "JSON_VALUE("))

					t.Setenv("LOG_SQL_DSN", "")
					require.NoError(t, model.InitLogDB())
					stat = requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "")
					assert.Zero(t, stat.TotalTokens, "reinitialized SQLite must not reuse MySQL capability")
				})
			}
		})
	}
}

type logStatsQueryRecorder struct {
	gormlogger.Interface
	queries []struct {
		sql  string
		rows int64
	}
}

func (r *logStatsQueryRecorder) Trace(_ context.Context, _ time.Time, query func() (string, int64), _ error) {
	sql, rows := query()
	r.queries = append(r.queries, struct {
		sql  string
		rows int64
	}{sql, rows})
}
