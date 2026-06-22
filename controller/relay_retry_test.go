package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newRelayRetryTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return ctx
}

func TestShouldRetry_Codex429IgnoresRetryLimit(t *testing.T) {
	ctx := newRelayRetryTestContext(t)
	err := types.NewOpenAIError(errors.New("quota exceeded"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	require.True(t, shouldRetry(ctx, err, 0, true))
}

func TestShouldRetry_Codex429RespectsSpecificChannel(t *testing.T) {
	ctx := newRelayRetryTestContext(t)
	ctx.Set("specific_channel_id", "123")
	err := types.NewOpenAIError(errors.New("quota exceeded"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	require.False(t, shouldRetry(ctx, err, 0, true))
}

func TestShouldRetry_Codex429RespectsAffinitySkipRetry(t *testing.T) {
	ctx := newRelayRetryTestContext(t)
	ctx.Set("channel_affinity_skip_retry_on_failure", true)
	err := types.NewOpenAIError(errors.New("quota exceeded"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests)

	require.False(t, shouldRetry(ctx, err, 0, true))
}

func TestIsCodex429FailoverTrigger(t *testing.T) {
	ctx := newRelayRetryTestContext(t)
	common.SetContextKey(ctx, constant.ContextKeyCodexUpstream429, true)

	require.True(t, isCodex429FailoverTrigger(ctx, constant.ChannelTypeCodex))
	require.False(t, isCodex429FailoverTrigger(ctx, constant.ChannelTypeOpenAI))
}

func TestRelayRetryUsesNormalizedPlaygroundPath(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	service.InitHttpClient()

	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	previousRetryTimes := common.RetryTimes
	common.RetryTimes = 1
	t.Cleanup(func() {
		common.RetryTimes = previousRetryTimes
	})

	previousErrorLogEnabled := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = false
	t.Cleanup(func() {
		constant.ErrorLogEnabled = previousErrorLogEnabled
	})

	var firstHits atomic.Int32
	var secondHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/first":
			firstHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary upstream failure","type":"server_error","code":"server_error"}}`))
		case "/second":
			secondHits.Add(1)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	const modelName = "relay-retry-playground-model"
	require.NoError(t, db.Create(&model.User{
		Id:       1001,
		Username: "relay-retry",
		Password: "password123",
		Status:   common.UserStatusEnabled,
		Quota:    100000,
		Group:    "default",
	}).Error)

	firstChannel := insertAdvancedCustomRetryChannel(t, 1, modelName, 10, upstream.URL+"/first")
	insertAdvancedCustomRetryChannel(t, 2, modelName, 1, upstream.URL+"/second")

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/pg/chat/completions", strings.NewReader(`{"model":"`+modelName+`","messages":[{"role":"user","content":"hello"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	common.SetContextKey(ctx, constant.ContextKeyUserId, 1001)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyUserQuota, 100000)
	common.SetContextKey(ctx, constant.ContextKeyUserSetting, dto.UserSetting{
		AcceptUnsetRatioModel: true,
		BillingPreference:     "wallet_only",
	})
	require.Nil(t, middleware.SetupContextForSelectedChannel(ctx, firstChannel, modelName))

	Relay(ctx, types.RelayFormatOpenAI)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, int32(1), firstHits.Load())
	require.Equal(t, int32(1), secondHits.Load())
	require.Equal(t, []string{"1", "2"}, ctx.GetStringSlice("use_channel"))
	require.Contains(t, recorder.Body.String(), `"content":"ok"`)
}

func insertAdvancedCustomRetryChannel(t *testing.T, channelID int, modelName string, priority int64, upstreamPath string) *model.Channel {
	t.Helper()

	channel := &model.Channel{
		Id:       channelID,
		Type:     constant.ChannelTypeAdvancedCustom,
		Key:      "sk-test",
		Status:   common.ChannelStatusEnabled,
		Name:     "advanced-custom-retry",
		Models:   modelName,
		Group:    "default",
		Priority: &priority,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{
				{
					IncomingPath: "/v1/chat/completions",
					UpstreamPath: upstreamPath,
					Converter:    dto.AdvancedCustomConverterNone,
				},
			},
		},
	})
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     "default",
		Model:     modelName,
		ChannelId: channelID,
		Enabled:   true,
		Priority:  &priority,
		Weight:    0,
	}).Error)

	return channel
}
