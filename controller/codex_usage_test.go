package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type codexWhamControllerResponse struct {
	Success        bool   `json:"success"`
	Message        string `json:"message"`
	UpstreamStatus int    `json:"upstream_status"`
	Data           any    `json:"data"`
}

func setupCodexUsageControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	oldDB := model.DB
	oldLogDB := model.LOG_DB
	oldUsingSQLite := common.UsingSQLite
	oldUsingMySQL := common.UsingMySQL
	oldUsingPostgreSQL := common.UsingPostgreSQL
	oldMemoryCacheEnabled := common.MemoryCacheEnabled

	gin.SetMode(gin.TestMode)
	common.UsingSQLite = true
	common.UsingMySQL = false
	common.UsingPostgreSQL = false
	common.MemoryCacheEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.Channel{}))

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
		model.DB = oldDB
		model.LOG_DB = oldLogDB
		common.UsingSQLite = oldUsingSQLite
		common.UsingMySQL = oldUsingMySQL
		common.UsingPostgreSQL = oldUsingPostgreSQL
		common.MemoryCacheEnabled = oldMemoryCacheEnabled
	})

	return db
}

func resetCodexWhamControllerStubs(t *testing.T) {
	t.Helper()

	originalUsageFetcher := codexWhamUsageFetcher
	originalResetCreditsFetcher := codexWhamResetCreditsFetcher
	originalResetCreditConsumer := codexWhamResetCreditConsumer
	originalTokenRefresher := codexOAuthTokenRefresher
	originalRequestIDGenerator := codexRedeemRequestIDGenerator

	t.Cleanup(func() {
		codexWhamUsageFetcher = originalUsageFetcher
		codexWhamResetCreditsFetcher = originalResetCreditsFetcher
		codexWhamResetCreditConsumer = originalResetCreditConsumer
		codexOAuthTokenRefresher = originalTokenRefresher
		codexRedeemRequestIDGenerator = originalRequestIDGenerator
	})
}

func createCodexTestChannel(t *testing.T, baseURL string, oauthKey codex.OAuthKey) model.Channel {
	t.Helper()

	key, err := common.Marshal(oauthKey)
	require.NoError(t, err)

	ch := model.Channel{
		Type:    constant.ChannelTypeCodex,
		Name:    "codex-test",
		Key:     string(key),
		BaseURL: common.GetPointer(baseURL),
	}
	require.NoError(t, model.DB.Create(&ch).Error)
	return ch
}

func performCodexWhamControllerRequest(method string, path string, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	router := gin.New()
	switch method {
	case http.MethodGet:
		router.GET(path, handler)
	case http.MethodPost:
		router.POST(path, handler)
	default:
		panic("unsupported method")
	}

	target := strings.Replace(path, ":id", "1", 1)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	return recorder
}

func decodeCodexWhamControllerResponse(t *testing.T, recorder *httptest.ResponseRecorder) codexWhamControllerResponse {
	t.Helper()

	require.Equal(t, http.StatusOK, recorder.Code)
	var response codexWhamControllerResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func TestConsumeCodexRateLimitResetCreditRejectsEmptyCreditIDBeforeUpstream(t *testing.T) {
	resetCodexWhamControllerStubs(t)

	called := false
	codexWhamResetCreditConsumer = func(ctx context.Context, client *http.Client, baseURL string, accessToken string, accountID string, creditID string, redeemRequestID string) (int, []byte, error) {
		called = true
		return http.StatusOK, []byte(`{"code":"reset"}`), nil
	}

	recorder := performCodexWhamControllerRequest(http.MethodPost, "/api/channel/:id/codex/rate-limit-reset-credits/consume", `{"credit_id":" "}`, ConsumeCodexChannelRateLimitResetCredit)
	response := decodeCodexWhamControllerResponse(t, recorder)

	require.False(t, response.Success)
	require.Equal(t, "credit_id is required", response.Message)
	require.False(t, called)
}

func TestGetCodexRateLimitResetCreditsRetriesAfterCredentialRefresh(t *testing.T) {
	setupCodexUsageControllerTestDB(t)
	resetCodexWhamControllerStubs(t)

	createCodexTestChannel(t, "https://unused.example", codex.OAuthKey{
		AccessToken:  "old-access",
		RefreshToken: "refresh-token",
		AccountID:    "account-123",
	})

	var fetchCount int
	codexWhamResetCreditsFetcher = func(ctx context.Context, client *http.Client, baseURL string, accessToken string, accountID string) (int, []byte, error) {
		fetchCount++
		require.Equal(t, "https://unused.example", baseURL)
		require.Equal(t, "account-123", accountID)
		if fetchCount == 1 {
			require.Equal(t, "old-access", accessToken)
			return http.StatusUnauthorized, []byte(`{"error":"expired"}`), nil
		}
		require.Equal(t, "new-access", accessToken)
		return http.StatusOK, []byte(`{"available_count":1,"credits":[{"id":"credit-1"}]}`), nil
	}

	var refreshCount int
	codexOAuthTokenRefresher = func(ctx context.Context, refreshToken string, proxyURL string) (*service.CodexOAuthTokenResult, error) {
		refreshCount++
		require.Equal(t, "refresh-token", refreshToken)
		require.Empty(t, proxyURL)
		return &service.CodexOAuthTokenResult{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}

	recorder := performCodexWhamControllerRequest(http.MethodGet, "/api/channel/:id/codex/rate-limit-reset-credits", "", GetCodexChannelRateLimitResetCredits)
	response := decodeCodexWhamControllerResponse(t, recorder)

	require.True(t, response.Success)
	require.Equal(t, http.StatusOK, response.UpstreamStatus)
	require.Equal(t, 2, fetchCount)
	require.Equal(t, 1, refreshCount)

	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, 1).Error)
	var savedKey codex.OAuthKey
	require.NoError(t, common.Unmarshal([]byte(channel.Key), &savedKey))
	require.Equal(t, "new-access", savedKey.AccessToken)
	require.Equal(t, "new-refresh", savedKey.RefreshToken)
}

func TestGetCodexRateLimitResetCreditsReturnsNonJSONBodyAsString(t *testing.T) {
	setupCodexUsageControllerTestDB(t)
	resetCodexWhamControllerStubs(t)

	createCodexTestChannel(t, "https://unused.example", codex.OAuthKey{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		AccountID:    "account-123",
	})

	codexWhamResetCreditsFetcher = func(ctx context.Context, client *http.Client, baseURL string, accessToken string, accountID string) (int, []byte, error) {
		return http.StatusOK, []byte("not-json"), nil
	}

	recorder := performCodexWhamControllerRequest(http.MethodGet, "/api/channel/:id/codex/rate-limit-reset-credits", "", GetCodexChannelRateLimitResetCredits)
	response := decodeCodexWhamControllerResponse(t, recorder)

	require.True(t, response.Success)
	require.Equal(t, http.StatusOK, response.UpstreamStatus)
	require.Equal(t, "not-json", response.Data)
}
