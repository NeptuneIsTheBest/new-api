package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
					})
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
					require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", "nullable").Updates(map[string]any{"quota": nil, "prompt_tokens": nil, "completion_tokens": nil, "other": nil}).Error)
					stat := requestLogStats(t, GetLogsStat, 3, common.RoleAdminUser, "request_id=nullable")
					assert.Zero(t, stat.Quota)
					assert.Zero(t, stat.TotalTokens)
					assert.Zero(t, stat.CacheHitRate)
				})
			}
			if database.kind == "mysql" {
				t.Run("quota accumulation with separate main database", func(t *testing.T) {
					mainDB, _ := newAuditTestDatabase(t, "sqlite", "")
					useLogExportDatabases(t, mainDB, model.LOG_DB, common.DatabaseTypeSQLite, common.DatabaseTypeMySQL)
					verifyLogStatsMySQLQuota(t)
				})
			}
			t.Run("canceled request fails statistics", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := model.SumUsedQuota(ctx, 0, model.LogFilter{})
				require.Error(t, err)
			})
		})
	}
}

func verifyLogStatsMySQLQuota(t *testing.T) {
	t.Helper()
	for i, tc := range []struct {
		name    string
		quotas  []int
		want    int
		wantErr bool
	}{
		{"exact large integer", []int{math.MaxInt / 2, 2}, math.MaxInt/2 + 2, false},
		{"upper int bound", []int{math.MaxInt - 1, 1}, math.MaxInt, false},
		{"lower int bound", []int{math.MinInt + 1, -1}, math.MinInt, false},
		{"positive final overflow", []int{math.MaxInt, 1}, 0, true},
		{"negative final overflow", []int{math.MinInt, -1}, 0, true},
		{"positive intermediate overflow cancels", []int{math.MaxInt, 1, -1}, math.MaxInt, false},
		{"negative intermediate overflow cancels", []int{math.MinInt, -1, 1}, math.MinInt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("quota-%d", i)
			for _, quota := range tc.quotas {
				require.NoError(t, model.LOG_DB.Create(&model.Log{
					Type: model.LogTypeConsume, ModelName: name, Quota: quota,
				}).Error)
			}
			stat, err := model.SumUsedQuota(context.Background(), 0, model.LogFilter{ModelName: name})
			if tc.wantErr {
				require.EqualError(t, err, "Failed to query statistics")
				assert.Zero(t, stat.Quota, "an overflowing quota must not be returned")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, stat.Quota)
		})
	}
}
