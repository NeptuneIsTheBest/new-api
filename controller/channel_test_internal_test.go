package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelTagListsDatabaseMatrix(t *testing.T) {
	previousGinMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousGinMode) })
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			database, dsn := newAuditTestDatabase(t, dialect.kind, os.Getenv(dialect.env))
			connection, err := database.DB()
			require.NoError(t, err)
			require.NoError(t, connection.Close())
			previousDB, previousLogDB := model.DB, model.LOG_DB
			previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
			previousMaster, previousSQLite := common.IsMasterNode, common.SQLitePath
			common.IsMasterNode = false
			if dialect.kind == "sqlite" {
				common.SQLitePath, dsn = dsn, "local"
			}
			t.Setenv("SQL_DSN", dsn)
			t.Setenv("LOG_SQL_DSN", "")
			t.Cleanup(func() {
				common.IsMasterNode, common.SQLitePath = previousMaster, previousSQLite
				common.SetDatabaseTypes(previousMain, previousLog)
				model.DB, model.LOG_DB = previousDB, previousLogDB
			})
			require.NoError(t, model.InitDB())
			db := model.DB
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&model.Channel{}))
			var version string
			versionQuery := "SELECT version()"
			if dialect.kind == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)

			queries, failAt := 0, 0
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register("channel_tag_queries", func(tx *gorm.DB) {
				if tx.DryRun {
					return
				}
				queries++
				if failAt > 0 && queries == failAt {
					tx.AddError(errors.New("tag channel query failed"))
				}
			}))

			t.Run("filters_sorting_and_pagination", func(t *testing.T) {
				channels := []model.Channel{
					{Name: "match alpha", Tag: common.GetPointer("Alpha"), Priority: common.GetPointer(int64(10)), Group: "default,vip", Models: "model-a", Type: 1},
					{Name: "alpha second", Tag: common.GetPointer("Alpha"), Priority: common.GetPointer(int64(20)), Group: "default", Models: "other", Type: 1},
					{Name: "alpha first", Tag: common.GetPointer("Alpha"), Priority: common.GetPointer(int64(20)), Group: "default", Type: 2},
					{Name: "match beta", Tag: common.GetPointer("Beta"), Priority: common.GetPointer(int64(5)), Group: "default", Models: "model-a", Type: 1},
					{Name: "beta manual", Tag: common.GetPointer("Beta"), Priority: common.GetPointer(int64(99)), Group: "default", Status: common.ChannelStatusManuallyDisabled, Type: 1},
					{Name: "beta auto", Tag: common.GetPointer("Beta"), Priority: common.GetPointer(int64(90)), Group: "default", Status: common.ChannelStatusAutoDisabled, Type: 2},
					{Name: "match outside", Tag: common.GetPointer("Beta"), Priority: common.GetPointer(int64(100)), Group: "default-extra", Models: "model-a", Type: 1},
					{Name: "gamma", Tag: common.GetPointer("Gamma"), Group: "default", Type: 1},
					{Name: "untagged", Group: "default", Type: 1},
					{Name: "empty tag", Tag: common.GetPointer(""), Group: "default", Type: 1},
				}
				for i := range channels {
					channels[i].Key = "secret-channel-key"
					channels[i].ChannelInfo = model.ChannelInfo{
						IsMultiKey: true, MultiKeySize: 2,
						MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
						MultiKeyDisabledReason: map[int]string{1: "private reason"},
						MultiKeyDisabledTime:   map[int]int64{1: 123},
					}
				}
				require.NoError(t, db.Create(&channels).Error)
				t.Cleanup(func() { require.NoError(t, db.Where("id > 0").Delete(&model.Channel{}).Error) })

				// SearchTags has its own database-defined order; preserve it when expanding tags.
				tags, err := model.SearchTags("match", "default", "model-a", false)
				require.NoError(t, err)
				require.Len(t, tags, 2)
				searchOrder := make([]int, 0, 3)
				searchChannelsByTag := map[string][]int{"Alpha": {1, 0}, "Beta": {3}}
				for _, tag := range tags {
					searchOrder = append(searchOrder, searchChannelsByTag[*tag]...)
				}
				allCounts := map[int]int{1: 7, 2: 2}
				enabledCounts := map[int]int{1: 6, 2: 1}
				for _, tc := range []struct {
					name, query string
					search      bool
					indices     []int
					total       int
					counts      map[int]int
					queries     int
				}{
					{"default", "", false, []int{2, 1, 0, 4, 5, 3, 7}, 3, allCounts, 4},
					{"id sort", "&id_sort=true", false, []int{2, 1, 0, 5, 4, 3, 7}, 3, allCounts, 4},
					{"name sort", "&sort_by=name&sort_order=asc", false, []int{2, 1, 0, 5, 4, 3, 7}, 3, allCounts, 4},
					{"priority asc", "&sort_by=priority&sort_order=asc", false, []int{0, 2, 1, 3, 5, 4, 7}, 3, allCounts, 4},
					{"tag page", "&p=2&page_size=1", false, []int{4, 5, 3}, 3, allCounts, 4},
					{"past last tag", "&p=4&page_size=1", false, []int{}, 3, allCounts, 3},
					{"enabled type", "&status=enabled&type=1", false, []int{1, 0, 3, 7}, 3, enabledCounts, 4},
					{"both disabled statuses", "&status=disabled", false, []int{4, 5}, 1, map[int]int{1: 1, 2: 1}, 4},
					{"no matching type", "&type=999", false, []int{}, 0, allCounts, 3},
					{"search expands matching tags", "&keyword=match&model=model-a&status=enabled&type=1", true, searchOrder, 3, map[int]int{1: 3, 2: 1}, 2},
					{"search channel page", "&keyword=match&model=model-a&status=enabled&type=1&p=2&page_size=1", true, searchOrder[1:2], 3, map[int]int{1: 3, 2: 1}, 2},
					{"search empty", "&keyword=absent", true, []int{}, 0, map[int]int{}, 1},
					{"search name sort", "&keyword=match+alpha&sort_by=name&sort_order=desc", true, []int{0, 1, 2}, 3, map[int]int{1: 2, 2: 1}, 2},
				} {
					t.Run(tc.name, func(t *testing.T) {
						queries = 0
						response := requestChannelTagList(t, tc.search, "tag_mode=true&group=default"+tc.query)
						require.True(t, response.Success, response.Message)
						assert.Equal(t, tc.queries, queries)
						assert.Equal(t, tc.total, response.Data.Total)
						assert.Equal(t, tc.counts, response.Data.TypeCounts)
						expectedIDs := make([]int, 0, len(tc.indices))
						for _, index := range tc.indices {
							expectedIDs = append(expectedIDs, channels[index].Id)
						}
						assert.Equal(t, expectedIDs, channelIDsFromChannels(response.Data.Items))
						require.NotNil(t, response.Data.Items)
						for _, channel := range response.Data.Items {
							assert.Empty(t, channel.Key)
							assert.Nil(t, channel.ChannelInfo.MultiKeyDisabledReason)
							assert.Nil(t, channel.ChannelInfo.MultiKeyDisabledTime)
							assert.Equal(t, channels[0].ChannelInfo.MultiKeyStatusList, channel.ChannelInfo.MultiKeyStatusList)
						}
					})
				}
				for _, search := range []bool{false, true} {
					queries, failAt = 0, 3
					expectedMessage := "Failed to fetch channels with this tag. Please try again later"
					if search {
						failAt, expectedMessage = 2, "tag channel query failed"
					}
					response := requestChannelTagList(t, search, "tag_mode=true&group=default")
					assert.False(t, response.Success)
					assert.Equal(t, expectedMessage, response.Message)
					assert.Nil(t, response.Data.Items)
					failAt = 0
				}
			})

			t.Run("batch_boundary", func(t *testing.T) {
				channels := make([]model.Channel, 201)
				for i := range channels {
					channels[i] = model.Channel{Name: "batch", Tag: common.GetPointer(fmt.Sprintf("batch-%03d", i)), Group: "default"}
				}
				require.NoError(t, db.CreateInBatches(&channels, 20).Error)
				t.Cleanup(func() { require.NoError(t, db.Where("id > 0").Delete(&model.Channel{}).Error) })
				tags, err := model.SearchTags("batch", "default", "", false)
				require.NoError(t, err)
				require.Len(t, tags, 201)
				queries = 0
				response := requestChannelTagList(t, true, "tag_mode=true&group=default&keyword=batch&page_size=201")
				require.True(t, response.Success, response.Message)
				assert.Equal(t, 3, queries)
				assert.Equal(t, 201, response.Data.Total)
				require.Len(t, response.Data.Items, 201)
				for i, tag := range tags {
					assert.Equal(t, tag, response.Data.Items[i].Tag)
				}
				queries, failAt = 0, 3
				response = requestChannelTagList(t, true, "tag_mode=true&group=default&keyword=batch&page_size=201")
				assert.False(t, response.Success)
				assert.Equal(t, "tag channel query failed", response.Message)
				assert.Equal(t, 3, queries)
				assert.Nil(t, response.Data.Items, "a later batch failure must not return partial results")
				failAt = 0
			})

			t.Run("tag_order_and_collation", func(t *testing.T) {
				query := db.Table("collation_channels")
				if dialect.kind == "sqlite" {
					require.NoError(t, query.AutoMigrate(&struct {
						model.Channel
						Tag *string `gorm:"type:text COLLATE NOCASE"`
					}{}))
				} else {
					require.NoError(t, query.AutoMigrate(&model.Channel{}))
				}
				channels := []model.Channel{
					{Tag: common.GetPointer("Alpha"), Key: "private", Priority: common.GetPointer(int64(10))},
					{Tag: common.GetPointer("alpha"), Key: "private", Priority: common.GetPointer(int64(20))},
					{Tag: common.GetPointer("quoted'tag"), Key: "private"},
				}
				require.NoError(t, query.Create(&channels).Error)
				tags := []*string{nil, common.GetPointer(""), channels[2].Tag, channels[0].Tag}
				order := model.NewChannelSortOptions("", "", false)
				expectedIDs := []int{channels[2].Id, channels[1].Id, channels[0].Id}
				if dialect.kind == "postgres" {
					expectedIDs = []int{channels[2].Id, channels[0].Id}
				}
				queries = 0
				result, err := model.GetChannelsByTags(db.Table("collation_channels").Model(&model.Channel{}), tags, order)
				require.NoError(t, err)
				assert.Equal(t, expectedIDs, channelIDsFromChannels(result))
				assert.Equal(t, 1, queries)
				for _, channel := range result {
					assert.Empty(t, channel.Key)
				}
				queries = 0
				result, err = model.GetChannelsByTags(db.Model(&model.Channel{}), tags[:2], order)
				require.NoError(t, err)
				assert.NotNil(t, result)
				assert.Empty(t, result)
				assert.Zero(t, queries)
			})
		})
	}
}

type channelTagListResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Items      []*model.Channel `json:"items"`
		Total      int              `json:"total"`
		TypeCounts map[int]int      `json:"type_counts"`
	} `json:"data"`
}

func requestChannelTagList(t *testing.T, search bool, query string) channelTagListResponse {
	t.Helper()
	path, handler := "/api/channel/", GetAllChannels
	if search {
		path, handler = "/api/channel/search", SearchChannels
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, path+"?"+query, nil)
	handler(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response channelTagListResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func TestGetChannelDefaultBaseURLsUsesBuiltInDefaults(t *testing.T) {
	originalBaseURLs := constant.ChannelBaseURLs
	constant.ChannelBaseURLs = append([]string(nil), originalBaseURLs...)
	constant.ChannelBaseURLs[constant.ChannelTypeDeepSeek] = "https://deepseek.server.example"
	t.Cleanup(func() {
		constant.ChannelBaseURLs = originalBaseURLs
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/default_base_urls", nil)
	GetChannelDefaultBaseURLs(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[int]string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, "https://deepseek.server.example", response.Data[constant.ChannelTypeDeepSeek])
	assert.Equal(t, "https://api.openai.com", response.Data[constant.ChannelTypeOpenAI])
	assert.NotContains(t, response.Data, constant.ChannelTypeAzure)
	assert.NotContains(t, response.Data, constant.ChannelTypeNewAPI)
	assert.NotContains(t, response.Data, constant.ChannelTypeTaskPlugin)
}

func TestValidateChannelProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", proxy: "http://proxy.example:8080"},
		{name: "https", proxy: "https://proxy.example:8443"},
		{name: "socks5", proxy: "socks5://proxy.example"},
		{name: "socks5h", proxy: "socks5h://proxy.example:1080/"},
		{name: "unsupported", proxy: "ftp://proxy.example", wantErr: true},
		{name: "path", proxy: "socks5://proxy.example:1080/path", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setting, err := common.Marshal(dto.ChannelSettings{Proxy: test.proxy})
			require.NoError(t, err)
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenAI,
				Setting: common.GetPointer(string(setting)),
			}

			err = validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "invalid channel proxy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateChannelRequiresNewAPIBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", baseURL: common.GetPointer("  "), wantErr: true},
		{name: "configured", baseURL: common.GetPointer("https://new-api.example")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeNewAPI,
				BaseURL: test.baseURL,
			}

			err := validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "New API channel base URL cannot be empty")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewAPIChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeNewAPI, apiType)
	assert.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeNewAPI)
	assert.Empty(t, constant.ChannelBaseURLs[constant.ChannelTypeNewAPI])
}

func TestResponsesCompactChannelSupport(t *testing.T) {
	tests := []struct {
		name        string
		channelType int
		apiType     int
		want        bool
	}{
		{name: "OpenAI", channelType: constant.ChannelTypeOpenAI, apiType: constant.APITypeOpenAI, want: true},
		{name: "Azure", channelType: constant.ChannelTypeAzure, apiType: constant.APITypeOpenAI, want: true},
		{name: "Codex", channelType: constant.ChannelTypeCodex, apiType: constant.APITypeCodex, want: true},
		{name: "Advanced Custom", channelType: constant.ChannelTypeAdvancedCustom, apiType: constant.APITypeAdvancedCustom, want: true},
		{name: "Sub2API", channelType: constant.ChannelTypeSub2API, apiType: constant.APITypeSub2API, want: true},
		{name: "New API", channelType: constant.ChannelTypeNewAPI, apiType: constant.APITypeNewAPI, want: true},
		{name: "Anthropic", channelType: constant.ChannelTypeAnthropic, apiType: constant.APITypeAnthropic, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, common.SupportsResponsesCompact(test.channelType, test.apiType))
		})
	}
}

func TestMultiprotocolGatewayEndpointTypes(t *testing.T) {
	want := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeOpenAIResponseCompact,
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeGemini,
		constant.EndpointTypeOpenAIAlphaSearch,
	}

	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5"))
	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeSub2API, "gpt-5"))
}

func TestCopyChannelRejectsInvalidLegacyProxySettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	settingBytes, err := common.Marshal(dto.ChannelSettings{
		Proxy: "socks5://proxy.example/legacy-path",
	})
	require.NoError(t, err)
	setting := string(settingBytes)
	origin := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "legacy proxy channel",
		Key:     "test-key",
		Models:  "gpt-test",
		Group:   "default",
		Setting: &setting,
	}
	require.NoError(t, db.Create(origin).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "invalid channel settings")
	var channelCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)
}

func TestDeleteChannelResetsProxyCacheWhenPreReadFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	service.ResetProxyClientCache()
	t.Cleanup(service.ResetProxyClientCache)

	proxyURL := "http://proxy.example:8080"
	beforeDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/999999", nil)

	DeleteChannel(ctx)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	afterDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	assert.NotSame(t, beforeDelete, afterDelete)
}

func TestDeleteChannelBatchReportsAndAuditsActualDeletedCount(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	channel := &model.Channel{Name: "existing", Key: "test-key"}
	require.NoError(t, db.Create(channel).Error)

	requestBody, err := common.Marshal(ChannelBatch{Ids: []int{channel.Id, 999999}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/batch", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	DeleteChannelBatch(ctx)

	var response struct {
		Success bool  `json:"success"`
		Data    int64 `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, int64(1), response.Data)

	var auditLog model.AuditLog
	require.NoError(t, db.Order("id desc").First(&auditLog).Error)
	var auditData struct {
		Operation struct {
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	encodedAudit, err := common.Marshal(auditLog.Other)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(encodedAudit, &auditData))
	assert.Equal(t, float64(1), auditData.Operation.Params["count"])
}

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	requestRules := []billingexpr.RequestRuleTrace{{
		Cond:       `param("service_tier") == "fast"`,
		Multiplier: 2,
		Matched:    true,
	}}
	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier:  "base",
		RequestRules: requestRules,
	})

	fields := other.Snapshot()
	require.Equal(t, "tiered_expr", fields["billing_mode"])
	require.Equal(t, "base", fields["matched_tier"])
	require.Equal(t, requestRules, fields["request_rules"])
	require.NotEmpty(t, fields["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestSelectChannelsForAutomaticTestAutoBanOnlyUsesEligibleChannels(t *testing.T) {
	autoBanEnabled := 1
	autoBanDisabled := 0
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled, AutoBan: &autoBanEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled, AutoBan: &autoBanDisabled},
		{Id: 3, Status: common.ChannelStatusAutoDisabled, AutoBan: &autoBanEnabled},
		{Id: 4, Status: common.ChannelStatusManuallyDisabled, AutoBan: &autoBanEnabled},
		{Id: 5, Status: common.ChannelStatusEnabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeAutoBanOnly)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 3, selected[1].Id)
}

func TestRunChannelTestWorkersHonorsConfiguredConcurrency(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	release := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32
	progress := make([]int, 0, len(channels)+1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			context.Background(),
			channels,
			2,
			func(_ context.Context, _ *model.Channel) channelTestSummary {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := maxActive.Load()
					if current <= observed || maxActive.CompareAndSwap(observed, current) {
						break
					}
				}
				started <- struct{}{}
				<-release
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	select {
	case <-started:
		t.Fatal("started more channel tests than the configured concurrency")
	default:
	}
	close(release)

	summary := <-summaryResult

	assert.Equal(t, int32(2), maxActive.Load())
	assert.Equal(t, channelTestSummary{Tested: 4, Succeeded: 4}, summary)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, progress)
}

func TestRunChannelTestWorkersStopsAfterCancellation(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	ctx, cancel := context.WithCancel(context.Background())
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	progress := make([]int, 0, 1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			ctx,
			channels,
			2,
			func(ctx context.Context, _ *model.Channel) channelTestSummary {
				started <- struct{}{}
				<-ctx.Done()
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	cancel()

	summary := <-summaryResult

	select {
	case <-started:
		t.Fatal("started another channel test after cancellation")
	default:
	}
	assert.Equal(t, channelTestSummary{Tested: 2, Succeeded: 2}, summary)
	assert.Equal(t, []int{0}, progress)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "A channel test task is already running or queued")
}
