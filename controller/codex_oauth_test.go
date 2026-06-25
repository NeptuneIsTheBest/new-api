package controller

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexOAuthAPIResponse struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func setupCodexOAuthRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("codex-oauth-test-secret"))))
	router.POST("/api/channel/codex/oauth/start", StartCodexOAuth)
	router.POST("/api/channel/codex/oauth/complete", CompleteCodexOAuth)
	router.POST("/api/channel/:id/codex/oauth/start", StartCodexOAuthForChannel)
	router.POST("/api/channel/:id/codex/oauth/complete", CompleteCodexOAuthForChannel)
	return router
}

func performCodexOAuthRequest(router *gin.Engine, method string, target string, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		request.AddCookie(c)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeCodexOAuthResponse(t *testing.T, recorder *httptest.ResponseRecorder) codexOAuthAPIResponse {
	t.Helper()

	require.Equal(t, http.StatusOK, recorder.Code)
	var response codexOAuthAPIResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func buildCodexOAuthTestJWT(t *testing.T, accountID string, email string) string {
	t.Helper()

	header, err := common.Marshal(map[string]any{"alg": "none"})
	require.NoError(t, err)
	payload, err := common.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"email": email,
	})
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestStartCodexOAuthStoresSessionAndReturnsAuthorizeURL(t *testing.T) {
	router := setupCodexOAuthRouter()

	recorder := performCodexOAuthRequest(router, http.MethodPost, "/api/channel/codex/oauth/start", `{}`, nil)
	response := decodeCodexOAuthResponse(t, recorder)

	require.True(t, response.Success)
	authorizeURL, ok := response.Data["authorize_url"].(string)
	require.True(t, ok)
	parsed, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	assert.Equal(t, "https", parsed.Scheme)
	assert.Equal(t, "auth.openai.com", parsed.Host)
	assert.NotEmpty(t, parsed.Query().Get("state"))
	assert.NotEmpty(t, parsed.Query().Get("code_challenge"))
	assert.NotEmpty(t, recorder.Result().Cookies())
}

func TestCompleteCodexOAuthRejectsStateMismatch(t *testing.T) {
	router := setupCodexOAuthRouter()

	startRecorder := performCodexOAuthRequest(router, http.MethodPost, "/api/channel/codex/oauth/start", `{}`, nil)
	require.True(t, decodeCodexOAuthResponse(t, startRecorder).Success)

	called := false
	originalExchanger := codexOAuthCodeExchanger
	codexOAuthCodeExchanger = func(ctx context.Context, code string, verifier string, proxyURL string) (*service.CodexOAuthTokenResult, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() {
		codexOAuthCodeExchanger = originalExchanger
	})

	completeRecorder := performCodexOAuthRequest(
		router,
		http.MethodPost,
		"/api/channel/codex/oauth/complete",
		`{"input":"http://localhost:1455/auth/callback?code=abc&state=wrong-state"}`,
		startRecorder.Result().Cookies(),
	)
	response := decodeCodexOAuthResponse(t, completeRecorder)

	assert.False(t, response.Success)
	assert.Equal(t, "state mismatch", response.Message)
	assert.False(t, called)
}

func TestCompleteCodexOAuthGeneratesCredential(t *testing.T) {
	router := setupCodexOAuthRouter()
	startRecorder := performCodexOAuthRequest(router, http.MethodPost, "/api/channel/codex/oauth/start", `{}`, nil)
	startResponse := decodeCodexOAuthResponse(t, startRecorder)
	require.True(t, startResponse.Success)
	authorizeURL, ok := startResponse.Data["authorize_url"].(string)
	require.True(t, ok)
	parsedAuthorizeURL, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	state := parsedAuthorizeURL.Query().Get("state")
	require.NotEmpty(t, state)

	originalExchanger := codexOAuthCodeExchanger
	codexOAuthCodeExchanger = func(ctx context.Context, code string, verifier string, proxyURL string) (*service.CodexOAuthTokenResult, error) {
		assert.Equal(t, "auth-code", code)
		assert.NotEmpty(t, verifier)
		assert.Empty(t, proxyURL)
		return &service.CodexOAuthTokenResult{
			AccessToken:  buildCodexOAuthTestJWT(t, "account-123", "codex@example.com"),
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}
	t.Cleanup(func() {
		codexOAuthCodeExchanger = originalExchanger
	})

	completeRecorder := performCodexOAuthRequest(
		router,
		http.MethodPost,
		"/api/channel/codex/oauth/complete",
		`{"input":"http://localhost:1455/auth/callback?code=auth-code&state=`+state+`"}`,
		startRecorder.Result().Cookies(),
	)
	response := decodeCodexOAuthResponse(t, completeRecorder)

	require.True(t, response.Success)
	require.Equal(t, "generated", response.Message)
	rawKey, ok := response.Data["key"].(string)
	require.True(t, ok)
	oauthKey, err := codex.ParseOAuthKey(rawKey)
	require.NoError(t, err)
	assert.Equal(t, "account-123", oauthKey.AccountID)
	assert.Equal(t, "codex@example.com", oauthKey.Email)
	assert.Equal(t, "refresh-token", oauthKey.RefreshToken)
	assert.Equal(t, "codex", oauthKey.Type)
}

func TestCompleteCodexOAuthForChannelSavesCredential(t *testing.T) {
	setupCodexUsageControllerTestDB(t)
	router := setupCodexOAuthRouter()

	ch := model.Channel{
		Type: constant.ChannelTypeCodex,
		Name: "codex-oauth-channel",
		Key:  "{}",
	}
	require.NoError(t, model.DB.Create(&ch).Error)

	channelID := strconv.Itoa(ch.Id)
	startPath := "/api/channel/" + channelID + "/codex/oauth/start"
	completePath := "/api/channel/" + channelID + "/codex/oauth/complete"
	startRecorder := performCodexOAuthRequest(router, http.MethodPost, startPath, `{}`, nil)
	startResponse := decodeCodexOAuthResponse(t, startRecorder)
	require.True(t, startResponse.Success)
	authorizeURL, ok := startResponse.Data["authorize_url"].(string)
	require.True(t, ok)
	parsedAuthorizeURL, err := url.Parse(authorizeURL)
	require.NoError(t, err)
	state := parsedAuthorizeURL.Query().Get("state")
	require.NotEmpty(t, state)

	originalExchanger := codexOAuthCodeExchanger
	codexOAuthCodeExchanger = func(ctx context.Context, code string, verifier string, proxyURL string) (*service.CodexOAuthTokenResult, error) {
		assert.Equal(t, "auth-code", code)
		return &service.CodexOAuthTokenResult{
			AccessToken:  buildCodexOAuthTestJWT(t, "account-456", "saved@example.com"),
			RefreshToken: "saved-refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}
	t.Cleanup(func() {
		codexOAuthCodeExchanger = originalExchanger
	})

	completeRecorder := performCodexOAuthRequest(
		router,
		http.MethodPost,
		completePath,
		`{"input":"http://localhost:1455/auth/callback?code=auth-code&state=`+state+`"}`,
		startRecorder.Result().Cookies(),
	)
	response := decodeCodexOAuthResponse(t, completeRecorder)
	require.True(t, response.Success)
	assert.Equal(t, "saved", response.Message)

	var saved model.Channel
	require.NoError(t, model.DB.First(&saved, ch.Id).Error)
	oauthKey, err := codex.ParseOAuthKey(saved.Key)
	require.NoError(t, err)
	assert.Equal(t, "account-456", oauthKey.AccountID)
	assert.Equal(t, "saved@example.com", oauthKey.Email)
	assert.Equal(t, "saved-refresh-token", oauthKey.RefreshToken)
}
