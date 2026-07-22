package controller

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type tokenUsageTestData struct {
	TotalTokens *int64 `json:"total_tokens"`
	TotalQuota  int64  `json:"total_quota"`
	ResetAt     int64  `json:"reset_at"`
}

type tokenUsageTestItem struct {
	ID    int                 `json:"id"`
	Key   string              `json:"key"`
	Usage *tokenUsageTestData `json:"usage"`
}

type tokenUsageTestPage struct {
	Items []tokenUsageTestItem `json:"items"`
}

type tokenUsageResetTestData struct {
	Usage tokenUsageTestData `json:"usage"`
}

type tokenUsageDetailsTestData struct {
	Available           bool                    `json:"available"`
	RangeStart          int64                   `json:"range_start"`
	RangeStartNano      string                  `json:"range_start_nano"`
	RangeStartExclusive bool                    `json:"range_start_exclusive"`
	RangeEnd            int64                   `json:"range_end"`
	RangeEndNano        string                  `json:"range_end_nano"`
	Timezone            string                  `json:"timezone"`
	BucketUnit          string                  `json:"bucket_unit"`
	Summary             model.TokenUsageSummary `json:"summary"`
	Trend               []struct {
		Bucket string `json:"bucket"`
	} `json:"trend"`
}

type tokenLogTestPage struct {
	Items []model.Log `json:"items"`
	Total int64       `json:"total"`
}

func TestTokenListAndSearchIncludeUsageSinceReset(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	token := seedToken(t, db, 1, "usage-list", "usage1234masked5678")
	require.NoError(t, db.Model(token).Updates(map[string]interface{}{
		"used_quota":            900,
		"usage_reset_time":      int64(100),
		"usage_reset_time_nano": int64(100_500),
	}).Error)
	require.NoError(t, db.Create(&[]model.Log{
		{UserId: 1, TokenId: token.Id, CreatedAt: 100, WrittenAtNano: 100_400, Type: model.LogTypeConsume, Quota: 300, PromptTokens: 100, CompletionTokens: 100},
		{UserId: 1, TokenId: token.Id, CreatedAt: 101, WrittenAtNano: 100_600, Type: model.LogTypeConsume, Quota: 600, PromptTokens: 20, CompletionTokens: 10},
		{UserId: 1, TokenId: token.Id, CreatedAt: 102, WrittenAtNano: 100_700, Type: model.LogTypeError, PromptTokens: 500, CompletionTokens: 500},
	}).Error)

	listCtx, listRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(listCtx)

	listResponse := decodeAPIResponse(t, listRecorder)
	require.True(t, listResponse.Success, listResponse.Message)
	var listPage tokenUsageTestPage
	require.NoError(t, common.Unmarshal(listResponse.Data, &listPage))
	require.Len(t, listPage.Items, 1)
	require.NotNil(t, listPage.Items[0].Usage)
	require.NotNil(t, listPage.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(30), *listPage.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(600), listPage.Items[0].Usage.TotalQuota)
	assert.Equal(t, int64(100), listPage.Items[0].Usage.ResetAt)
	assert.Equal(t, token.GetMaskedKey(), listPage.Items[0].Key)
	assert.NotContains(t, listRecorder.Body.String(), token.Key)

	searchCtx, searchRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/search?keyword=usage-list&p=1&size=10", nil, 1)
	SearchTokens(searchCtx)

	searchResponse := decodeAPIResponse(t, searchRecorder)
	require.True(t, searchResponse.Success, searchResponse.Message)
	var searchPage tokenUsageTestPage
	require.NoError(t, common.Unmarshal(searchResponse.Data, &searchPage))
	require.Len(t, searchPage.Items, 1)
	require.NotNil(t, searchPage.Items[0].Usage)
	require.NotNil(t, searchPage.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(30), *searchPage.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(600), searchPage.Items[0].Usage.TotalQuota)
}

func TestGetTokenUsageDetailsRequiresOwnershipAndValidTimezone(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	originalLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() {
		common.LogConsumeEnabled = originalLogConsumeEnabled
	})

	token := seedToken(t, db, 1, "usage-details", "details1234masked5678")
	require.NoError(t, db.Create(&model.Log{
		UserId:           1,
		TokenId:          token.Id,
		TokenName:        "usage-details",
		ModelName:        "gpt-test",
		CreatedAt:        2,
		WrittenAtNano:    2_000,
		Type:             model.LogTypeConsume,
		Quota:            75,
		PromptTokens:     12,
		CompletionTokens: 8,
	}).Error)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, fmt.Sprintf("/api/token/%d/usage?timezone=Asia%%2FShanghai", token.Id), nil, 1)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(token.Id)}}
	GetTokenUsageDetails(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var details tokenUsageDetailsTestData
	require.NoError(t, common.Unmarshal(response.Data, &details))
	assert.True(t, details.Available)
	assert.Equal(t, token.CreatedTime, details.RangeStart)
	assert.Empty(t, details.RangeStartNano)
	assert.False(t, details.RangeStartExclusive)
	assert.GreaterOrEqual(t, details.RangeEnd, int64(2))
	assert.NotEmpty(t, details.RangeEndNano)
	assert.Equal(t, "Asia/Shanghai", details.Timezone)
	assert.Equal(t, "day", details.BucketUnit)
	assert.Equal(t, int64(1), details.Summary.SettledRequests)
	assert.Equal(t, int64(20), details.Summary.TotalTokens)
	assert.Equal(t, int64(75), details.Summary.TotalQuota)
	require.Len(t, details.Trend, 1)
	assert.Equal(t, "1970-01-01", details.Trend[0].Bucket)

	unauthorizedCtx, unauthorizedRecorder := newAuthenticatedContext(t, http.MethodGet, fmt.Sprintf("/api/token/%d/usage", token.Id), nil, 2)
	unauthorizedCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(token.Id)}}
	GetTokenUsageDetails(unauthorizedCtx)
	assert.False(t, decodeAPIResponse(t, unauthorizedRecorder).Success)

	invalidTimezoneCtx, invalidTimezoneRecorder := newAuthenticatedContext(t, http.MethodGet, fmt.Sprintf("/api/token/%d/usage?timezone=Mars%%2FOlympus_Mons", token.Id), nil, 1)
	invalidTimezoneCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(token.Id)}}
	GetTokenUsageDetails(invalidTimezoneCtx)
	assert.False(t, decodeAPIResponse(t, invalidTimezoneRecorder).Success)
}

func TestUserLogsFilterByTokenIdAfterTokenRename(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	token := seedToken(t, db, 1, "renamed-token", "renamed1234masked5678")
	otherToken := seedToken(t, db, 1, "other-token", "other1234masked5678")
	require.NoError(t, db.Create(&[]model.Log{
		{UserId: 1, TokenId: token.Id, TokenName: "original-token", CreatedAt: 10, Type: model.LogTypeConsume},
		{UserId: 1, TokenId: otherToken.Id, TokenName: "renamed-token", CreatedAt: 11, Type: model.LogTypeConsume},
	}).Error)

	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodGet,
		fmt.Sprintf("/api/log/self?token_id=%d&token_name=renamed-token", token.Id),
		nil,
		1,
	)
	GetUserLogs(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var page tokenLogTestPage
	require.NoError(t, common.Unmarshal(response.Data, &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, token.Id, page.Items[0].TokenId)
	assert.Equal(t, "original-token", page.Items[0].TokenName)
}

func TestUserLogsRespectExactUsageWindow(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	token := seedToken(t, db, 1, "usage-window", "window1234masked5678")
	require.NoError(t, db.Create(&[]model.Log{
		{UserId: 1, TokenId: token.Id, CreatedAt: 100, WrittenAtNano: 100_400, Type: model.LogTypeConsume, Content: "before-reset"},
		{UserId: 1, TokenId: token.Id, CreatedAt: 100, WrittenAtNano: 100_600, Type: model.LogTypeConsume, Content: "inside-window"},
		{UserId: 1, TokenId: token.Id, CreatedAt: 100, WrittenAtNano: 100_800, Type: model.LogTypeConsume, Content: "after-snapshot"},
	}).Error)

	target := fmt.Sprintf(
		"/api/log/self?token_id=%d&start_timestamp=100&end_timestamp=100&start_written_at_nano=100500&end_written_at_nano=100700&start_timestamp_exclusive=true",
		token.Id,
	)
	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, target, nil, 1)
	GetUserLogs(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var page tokenLogTestPage
	require.NoError(t, common.Unmarshal(response.Data, &page))
	assert.Equal(t, int64(1), page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "inside-window", page.Items[0].Content)
}

func TestUserLogsRejectInvalidExactUsageWindow(t *testing.T) {
	tests := []string{
		"/api/log/self?start_written_at_nano=invalid",
		"/api/log/self?start_written_at_nano=9223372036854775808",
		"/api/log/self?end_written_at_nano=0",
		"/api/log/self?start_timestamp_exclusive=true",
	}

	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			ctx, recorder := newAuthenticatedContext(t, http.MethodGet, target, nil, 1)
			GetUserLogs(ctx)
			assert.False(t, decodeAPIResponse(t, recorder).Success)
		})
	}
}

func TestResetTokenUsageRequiresOwnershipAndStartsNewUsageWindow(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	token := seedToken(t, db, 1, "usage-reset", "reset1234masked5678")
	require.NoError(t, db.Model(token).Updates(map[string]interface{}{
		"remain_quota": 100,
		"used_quota":   900,
	}).Error)
	require.NoError(t, db.Create(&model.Log{
		UserId: 1, TokenId: token.Id, CreatedAt: 1, Type: model.LogTypeConsume, Quota: 900, PromptTokens: 40, CompletionTokens: 60,
	}).Error)

	unauthorizedCtx, unauthorizedRecorder := newAuthenticatedContext(t, http.MethodPost, fmt.Sprintf("/api/token/%d/usage/reset", token.Id), nil, 2)
	unauthorizedCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(token.Id)}}
	ResetTokenUsage(unauthorizedCtx)
	unauthorizedResponse := decodeAPIResponse(t, unauthorizedRecorder)
	assert.False(t, unauthorizedResponse.Success)

	authorizedCtx, authorizedRecorder := newAuthenticatedContext(t, http.MethodPost, fmt.Sprintf("/api/token/%d/usage/reset", token.Id), nil, 1)
	authorizedCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(token.Id)}}
	ResetTokenUsage(authorizedCtx)

	authorizedResponse := decodeAPIResponse(t, authorizedRecorder)
	require.True(t, authorizedResponse.Success, authorizedResponse.Message)
	var resetData tokenUsageResetTestData
	require.NoError(t, common.Unmarshal(authorizedResponse.Data, &resetData))
	require.NotNil(t, resetData.Usage.TotalTokens)
	assert.Zero(t, *resetData.Usage.TotalTokens)
	assert.Zero(t, resetData.Usage.TotalQuota)
	assert.Positive(t, resetData.Usage.ResetAt)

	var resetToken model.Token
	require.NoError(t, db.First(&resetToken, token.Id).Error)
	assert.Equal(t, resetData.Usage.ResetAt, resetToken.UsageResetTime)
	assert.Positive(t, resetToken.UsageResetTimeNano)
	assert.Equal(t, 900, resetToken.UsedQuota)
	assert.Equal(t, 100, resetToken.RemainQuota)
	var logCount int64
	require.NoError(t, db.Model(&model.Log{}).Where("token_id = ?", token.Id).Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)

	// A pre-reset settled deduction may reach the token row after reset when
	// batch updates are enabled. It must not reappear in the new usage window.
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("used_quota", 950).Error)

	listCtx, listRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(listCtx)
	listResponse := decodeAPIResponse(t, listRecorder)
	require.True(t, listResponse.Success, listResponse.Message)
	var page tokenUsageTestPage
	require.NoError(t, common.Unmarshal(listResponse.Data, &page))
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Items[0].Usage)
	require.NotNil(t, page.Items[0].Usage.TotalTokens)
	assert.Zero(t, *page.Items[0].Usage.TotalTokens)
	assert.Zero(t, page.Items[0].Usage.TotalQuota)

	require.NoError(t, db.Create(&model.Log{
		UserId: 1, TokenId: token.Id, CreatedAt: resetData.Usage.ResetAt, WrittenAtNano: resetToken.UsageResetTimeNano + 1,
		Type: model.LogTypeConsume, Quota: 100, PromptTokens: 10, CompletionTokens: 5,
	}).Error)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("used_quota", 1050).Error)

	postSettleCtx, postSettleRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(postSettleCtx)
	postSettleResponse := decodeAPIResponse(t, postSettleRecorder)
	require.True(t, postSettleResponse.Success, postSettleResponse.Message)
	var postSettlePage tokenUsageTestPage
	require.NoError(t, common.Unmarshal(postSettleResponse.Data, &postSettlePage))
	require.Len(t, postSettlePage.Items, 1)
	require.NotNil(t, postSettlePage.Items[0].Usage)
	require.NotNil(t, postSettlePage.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(15), *postSettlePage.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(100), postSettlePage.Items[0].Usage.TotalQuota)
}

func TestResetTokenUsageBatchResetsOwnedKeysOnly(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	firstToken := seedToken(t, db, 1, "usage-batch-first", "batch1234first5678")
	secondToken := seedToken(t, db, 1, "usage-batch-second", "batch1234second5678")
	otherUserToken := seedToken(t, db, 2, "usage-batch-other", "batch1234other5678")
	require.NoError(t, db.Model(&model.Token{}).
		Where("id IN ?", []int{firstToken.Id, secondToken.Id}).
		Updates(map[string]interface{}{
			"remain_quota": 400,
			"used_quota":   600,
		}).Error)
	require.NoError(t, db.Create(&[]model.Log{
		{UserId: 1, TokenId: firstToken.Id, CreatedAt: 1, Type: model.LogTypeConsume, Quota: 300, PromptTokens: 20, CompletionTokens: 10},
		{UserId: 1, TokenId: secondToken.Id, CreatedAt: 1, Type: model.LogTypeConsume, Quota: 300, PromptTokens: 15, CompletionTokens: 5},
	}).Error)

	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/batch/usage/reset", TokenBatch{
		Ids: []int{firstToken.Id, secondToken.Id, otherUserToken.Id},
	}, 1)
	ResetTokenUsageBatch(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var resetCount int64
	require.NoError(t, common.Unmarshal(response.Data, &resetCount))
	assert.Equal(t, int64(2), resetCount)

	var resetTokens []model.Token
	require.NoError(t, db.Where("id IN ?", []int{firstToken.Id, secondToken.Id}).Order("id").Find(&resetTokens).Error)
	require.Len(t, resetTokens, 2)
	assert.Positive(t, resetTokens[0].UsageResetTime)
	assert.Positive(t, resetTokens[0].UsageResetTimeNano)
	assert.Equal(t, resetTokens[0].UsageResetTime, resetTokens[1].UsageResetTime)
	assert.Equal(t, resetTokens[0].UsageResetTimeNano, resetTokens[1].UsageResetTimeNano)
	for _, token := range resetTokens {
		assert.Equal(t, 600, token.UsedQuota)
		assert.Equal(t, 400, token.RemainQuota)
		assert.Equal(t, common.TokenStatusEnabled, token.Status)
	}

	listCtx, listRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(listCtx)
	listResponse := decodeAPIResponse(t, listRecorder)
	require.True(t, listResponse.Success, listResponse.Message)
	var page tokenUsageTestPage
	require.NoError(t, common.Unmarshal(listResponse.Data, &page))
	require.Len(t, page.Items, 2)
	for _, item := range page.Items {
		require.NotNil(t, item.Usage)
		require.NotNil(t, item.Usage.TotalTokens)
		assert.Zero(t, *item.Usage.TotalTokens)
		assert.Zero(t, item.Usage.TotalQuota)
	}

	var unchangedOtherUserToken model.Token
	require.NoError(t, db.First(&unchangedOtherUserToken, otherUserToken.Id).Error)
	assert.Zero(t, unchangedOtherUserToken.UsageResetTime)
	assert.Zero(t, unchangedOtherUserToken.UsageResetTimeNano)

	var logCount int64
	require.NoError(t, db.Model(&model.Log{}).Where("user_id = ?", 1).Count(&logCount).Error)
	assert.Equal(t, int64(2), logCount)
}

func TestResetTokenUsageBatchValidatesRequest(t *testing.T) {
	tooManyIds := make([]int, 101)
	for index := range tooManyIds {
		tooManyIds[index] = index + 1
	}

	tests := []struct {
		name string
		body TokenBatch
	}{
		{name: "empty", body: TokenBatch{}},
		{name: "too many", body: TokenBatch{Ids: tooManyIds}},
		{name: "non-positive id", body: TokenBatch{Ids: []int{0}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/batch/usage/reset", test.body, 1)
			ResetTokenUsageBatch(ctx)

			response := decodeAPIResponse(t, recorder)
			assert.False(t, response.Success)
		})
	}
}

func TestTokenUsageCountsFullSettlementAfterResetDuringPreConsume(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	token := seedToken(t, db, 1, "usage-in-flight", "inflight1234masked5678")
	require.NoError(t, db.Model(token).Update("used_quota", 700).Error)
	require.NoError(t, model.ResetTokenUsage(token.Id, 1, 100, 100_500))

	// The request pre-consumed 300 before reset, then settled at a total cost of
	// 500 after reset. The new window must contain the full settled event, not
	// only the 200 quota delta applied after the reset snapshot.
	require.NoError(t, db.Create(&model.Log{
		UserId: 1, TokenId: token.Id, CreatedAt: 100, WrittenAtNano: 100_600,
		Type: model.LogTypeConsume, Quota: 500, PromptTokens: 20, CompletionTokens: 10,
	}).Error)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("used_quota", 900).Error)

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var page tokenUsageTestPage
	require.NoError(t, common.Unmarshal(response.Data, &page))
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Items[0].Usage)
	require.NotNil(t, page.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(30), *page.Items[0].Usage.TotalTokens)
	assert.Equal(t, int64(500), page.Items[0].Usage.TotalQuota)
}

func TestTokenListAndSearchOmitUsageWhenConsumeLoggingDisabled(t *testing.T) {
	originalLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() {
		common.LogConsumeEnabled = originalLogConsumeEnabled
	})

	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	loggedToken := seedToken(t, db, 1, "usage-disabled-logged", "disabled1234logged5678")
	emptyToken := seedToken(t, db, 1, "usage-disabled-empty", "disabled1234empty5678")
	require.NoError(t, db.Model(loggedToken).Update("used_quota", 500).Error)
	require.NoError(t, db.Create(&model.Log{
		UserId: 1, TokenId: loggedToken.Id, CreatedAt: 100, Type: model.LogTypeConsume,
		Quota: 500, PromptTokens: 20, CompletionTokens: 10,
	}).Error)

	listCtx, listRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(listCtx)

	listResponse := decodeAPIResponse(t, listRecorder)
	require.True(t, listResponse.Success, listResponse.Message)
	var listPage tokenUsageTestPage
	require.NoError(t, common.Unmarshal(listResponse.Data, &listPage))
	require.Len(t, listPage.Items, 2)
	for _, item := range listPage.Items {
		assert.Nil(t, item.Usage)
	}
	assert.NotContains(t, listRecorder.Body.String(), `"usage"`)
	assert.NotContains(t, listRecorder.Body.String(), loggedToken.Key)
	assert.NotContains(t, listRecorder.Body.String(), emptyToken.Key)

	searchCtx, searchRecorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/search?keyword=usage-disabled-logged&p=1&size=10", nil, 1)
	SearchTokens(searchCtx)

	searchResponse := decodeAPIResponse(t, searchRecorder)
	require.True(t, searchResponse.Success, searchResponse.Message)
	var searchPage tokenUsageTestPage
	require.NoError(t, common.Unmarshal(searchResponse.Data, &searchPage))
	require.Len(t, searchPage.Items, 1)
	for _, item := range searchPage.Items {
		assert.Nil(t, item.Usage)
	}
	assert.NotContains(t, searchRecorder.Body.String(), `"usage"`)
	assert.NotContains(t, searchRecorder.Body.String(), loggedToken.Key)
	assert.NotContains(t, searchRecorder.Body.String(), emptyToken.Key)
}

func TestTokenListOmitsUsageWhenLogStatsAreUnavailable(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "usage-fallback", "fallback1234masked5678")
	require.NoError(t, db.Model(token).Update("used_quota", 250).Error)

	originalLogDB := model.LOG_DB
	logDB, err := gorm.Open(sqlite.Open("file:closed_token_usage?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	model.LOG_DB = logDB
	t.Cleanup(func() {
		model.LOG_DB = originalLogDB
	})
	sqlDB, err := logDB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	var page tokenUsageTestPage
	require.NoError(t, common.Unmarshal(response.Data, &page))
	require.Len(t, page.Items, 1)
	assert.Nil(t, page.Items[0].Usage)
	assert.NotContains(t, recorder.Body.String(), strings.TrimPrefix(token.Key, "sk-"))
}
