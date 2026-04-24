package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
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
