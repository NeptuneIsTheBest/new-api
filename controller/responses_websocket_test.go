package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var responsesWSTestUserSequence atomic.Int64

func setupResponsesWSRequestTest(t *testing.T) (*model.User, *model.Token) {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	previousRedis := common.RedisEnabled
	previousRDB := common.RDB
	previousMaster, previousMemory, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.SQLitePath
	previousEnabled := setting.ModelRequestRateLimitEnabled
	previousDuration := setting.ModelRequestRateLimitDurationMinutes
	previousTotal := setting.ModelRequestRateLimitCount
	previousSuccess := setting.ModelRequestRateLimitSuccessCount
	setting.ModelRequestRateLimitMutex.Lock()
	previousGroups := setting.ModelRequestRateLimitGroup
	setting.ModelRequestRateLimitGroup = nil
	setting.ModelRequestRateLimitMutex.Unlock()
	t.Setenv("SQL_DSN", os.Getenv("TEST_RESPONSES_SQL_DSN"))
	t.Setenv("LOG_SQL_DSN", os.Getenv("TEST_RESPONSES_LOG_SQL_DSN"))
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "responses.db")
	require.NoError(t, model.InitDB())
	db := model.DB
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, model.InitLogDB())
	if model.LOG_DB != db {
		logSQL, err := model.LOG_DB.DB()
		require.NoError(t, err)
		logSQL.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, logSQL.Close()) })
	}
	setting.ModelRequestRateLimitEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.RedisEnabled = previousRedis
		common.RDB = previousRDB
		common.IsMasterNode, common.MemoryCacheEnabled, common.SQLitePath = previousMaster, previousMemory, previousSQLite
		setting.ModelRequestRateLimitEnabled = previousEnabled
		setting.ModelRequestRateLimitDurationMinutes = previousDuration
		setting.ModelRequestRateLimitCount = previousTotal
		setting.ModelRequestRateLimitSuccessCount = previousSuccess
		setting.ModelRequestRateLimitMutex.Lock()
		setting.ModelRequestRateLimitGroup = previousGroups
		setting.ModelRequestRateLimitMutex.Unlock()
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))
	// The shared in-memory limiter outlives each database fixture. Give every
	// user a separate quota bucket, including when the tests run with -count.
	user := &model.User{Id: 5062000 + int(responsesWSTestUserSequence.Add(1)), Username: "responses-ws-user", Status: common.UserStatusEnabled, Group: "default", Quota: 1000, AuthVersion: 1}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "responseswstoken", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100}
	require.NoError(t, db.Create(token).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Unscoped().Delete(token).Error)
		require.NoError(t, db.Unscoped().Delete(user).Error)
	})
	return user, token
}

func newResponsesWSTestRunner(t *testing.T, token *model.Token) (relay.ResponsesWSRequestRunner, *http.Request) {
	t.Helper()
	var runner relay.ResponsesWSRequestRunner
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies([]string{"127.0.0.1"}))
	engine.GET("/v1/responses", middleware.TokenAuth(), func(c *gin.Context) {
		runner = newResponsesWSRequestRunner(c)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Key", "handshake-key")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
	request.Header.Set("Sec-WebSocket-Protocol", "responses")
	request.Header.Set("Content-Length", "99")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("X-Application-Header", "preserved")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.NotNil(t, runner)
	return runner, request
}

func TestResponsesWSRequestRunnerRefreshesBillingContextAndCleansBody(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	allowedIP := "203.0.113.8"
	require.NoError(t, model.DB.Model(token).Update("allow_ips", allowedIP).Error)
	runner, handshake := newResponsesWSTestRunner(t, token)
	// A later upstream header mutation must not replace the client's credential.
	handshake.Header.Set("Authorization", "Bearer upstream-secret")
	handshake.Header.Set("X-Forwarded-For", "198.51.100.99")
	var storage common.BodyStorage
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"first"}`))
	require.Nil(t, runner(request, "ws-first", func(c *gin.Context) *types.NewAPIError {
		assert.Equal(t, user.Id, c.GetInt("id"))
		assert.Equal(t, 100, c.GetInt("token_quota"))
		assert.Equal(t, "203.0.113.8", c.ClientIP())
		assert.Equal(t, "ws-first", c.GetString(common.RequestIdKey))
		assert.Equal(t, "ws-first", c.Request.Context().Value(common.RequestIdKey))
		for _, header := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol", "Content-Length", "Content-Encoding"} {
			assert.Empty(t, c.GetHeader(header))
		}
		assert.Equal(t, "preserved", c.GetHeader("X-Application-Header"))
		c.Set("previous_turn", true)
		var err error
		storage, err = common.GetBodyStorage(c)
		require.NoError(t, err)
		data, err := storage.Bytes()
		require.NoError(t, err)
		assert.JSONEq(t, `{"input":"first"}`, string(data))
		return nil
	}))
	_, err := storage.Bytes()
	assert.ErrorIs(t, err, common.ErrStorageClosed)
	require.NoError(t, model.DB.Model(token).Updates(map[string]any{
		"remain_quota": 2, "model_limits_enabled": true, "model_limits": "gpt-5.1",
	}).Error)
	require.NoError(t, model.DB.Model(user).Updates(map[string]any{
		"group": "updated", "setting": `{"billing_preference":"wallet_only"}`,
	}).Error)
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"second"}`))
	require.Nil(t, runner(request, "ws-second", func(c *gin.Context) *types.NewAPIError {
		assert.Equal(t, 2, c.GetInt("token_quota"))
		assert.True(t, c.GetBool("token_model_limit_enabled"))
		modelLimits, _ := c.Get("token_model_limit")
		assert.Equal(t, map[string]bool{"gpt-5.1": true}, modelLimits)
		assert.Equal(t, "updated", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
		userSetting, ok := common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting)
		require.True(t, ok)
		assert.Equal(t, "wallet_only", userSetting.BillingPreference)
		assert.Equal(t, "ws-second", c.GetString(common.RequestIdKey))
		_, leaked := c.Get("previous_turn")
		assert.False(t, leaked)
		_, leaked = c.Get(common.KeyBodyStorage)
		assert.False(t, leaked)
		return nil
	}))
}

func TestResponsesWSRequestRunnerRejectsRevokedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		update func(*testing.T, *model.User, *model.Token)
	}{
		{name: "disabled token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("status", common.TokenStatusDisabled).Error)
		}},
		{name: "expired token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("expired_time", time.Now().Add(-time.Minute).Unix()).Error)
		}},
		{name: "exhausted token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("remain_quota", 0).Error)
		}},
		{name: "disabled user", status: http.StatusForbidden, update: func(t *testing.T, user *model.User, _ *model.Token) {
			require.NoError(t, model.DB.Model(user).Update("status", common.UserStatusDisabled).Error)
		}},
		{name: "deleted token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Delete(token).Error)
		}},
		{name: "changed IP restriction", status: http.StatusForbidden, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("allow_ips", "198.51.100.0/24").Error)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			runner, _ := newResponsesWSTestRunner(t, token)
			require.Nil(t, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "before", func(*gin.Context) *types.NewAPIError { return nil }))
			tc.update(t, user, token)
			called := false
			apiError := runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "after", func(*gin.Context) *types.NewAPIError {
				called = true
				return nil
			})
			require.NotNil(t, apiError)
			assert.Equal(t, tc.status, apiError.StatusCode)
			assert.False(t, called)
			assert.NotContains(t, apiError.Error(), token.Key)
		})
	}
}

func TestResponsesWSRequestRunnerUsesExistingMemoryRateLimit(t *testing.T) {
	require.NoError(t, i18n.Init())
	_, token := setupResponsesWSRequestTest(t)
	setting.ModelRequestRateLimitEnabled = true
	setting.ModelRequestRateLimitDurationMinutes = 1
	setting.ModelRequestRateLimitCount = 1
	setting.ModelRequestRateLimitSuccessCount = 100
	runner, _ := newResponsesWSTestRunner(t, token)
	called := 0
	handle := func(*gin.Context) *types.NewAPIError { called++; return nil }
	require.Nil(t, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "first", handle))
	apiError := runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "limited", handle)
	require.NotNil(t, apiError)
	assert.Equal(t, http.StatusTooManyRequests, apiError.StatusCode)
	message := i18n.Translate("", i18n.MsgRateLimitTotalReached, map[string]any{"Minutes": 1, "Max": 1})
	assert.Equal(t, common.MessageWithRequestId(message, "limited"), apiError.Error())
	assert.Equal(t, 1, called)
}

func TestResponsesWSRequestRunnerSharesRedisSuccessLimitWithHTTP(t *testing.T) {
	_, token := setupResponsesWSRequestTest(t)
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	common.RedisEnabled = true
	common.RDB = client
	setting.ModelRequestRateLimitEnabled = true
	setting.ModelRequestRateLimitDurationMinutes = 1
	setting.ModelRequestRateLimitCount = 0
	setting.ModelRequestRateLimitSuccessCount = 1
	runner, _ := newResponsesWSTestRunner(t, token)
	failed := types.NewErrorWithStatusCode(errors.New("invalid input"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	assert.Same(t, failed, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "failed", func(*gin.Context) *types.NewAPIError { return failed }))
	require.Nil(t, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "successful", func(*gin.Context) *types.NewAPIError { return nil }))
	engine := gin.New()
	called := false
	engine.POST("/v1/responses", middleware.TokenAuth(), middleware.ModelRequestRateLimit(), func(c *gin.Context) { called = true })
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	assert.Equal(t, http.StatusTooManyRequests, response.Code)
	assert.False(t, called)
}

type responsesWSBillingTest struct {
	httpUpstream http.HandlerFunc
	httpDone     chan struct{}
	user         *model.User
	token        *model.Token
	channel      *model.Channel
	client       *websocket.Conn
	done         chan struct{}
	upstreamDone chan struct{}
	connections  atomic.Int32
	gatewayURL   string
}

func (fixture *responsesWSBillingTest) closeAndWait(t *testing.T) {
	t.Helper()
	_ = fixture.client.Close()
	select {
	case <-fixture.done:
	case <-time.After(3 * time.Second):
		t.Error("gateway handler did not stop after closing its client")
	}
	if fixture.connections.Load() > 0 {
		select {
		case <-fixture.upstreamDone:
		case <-time.After(3 * time.Second):
			t.Error("upstream connection was not closed")
		}
	}
	// Health classification is synchronous at the request boundary; only the
	// Redis write is asynchronous, see waitPerfCounters.
}

// waitPerfCounters waits for the asynchronous health samples of the fixture
// model to reach wantRequests and returns the request and success counters.
func waitPerfCounters(t *testing.T, wantRequests int64) (requests, successes int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		requests, successes = 0, 0
		keys, err := common.RDB.Keys(context.Background(), "perf:ws-billing:*").Result()
		if err != nil {
			return false
		}
		for _, key := range keys {
			n, _ := common.RDB.HGet(context.Background(), key, "req").Int64()
			requests += n
			n, _ = common.RDB.HGet(context.Background(), key, "ok").Int64()
			successes += n
		}
		return requests >= wantRequests
	}, 3*time.Second, 10*time.Millisecond)
	return requests, successes
}

func newResponsesWSBillingTest(t *testing.T, expression string, handle func(*websocket.Conn, *http.Request), httpEvents ...string) *responsesWSBillingTest {
	t.Helper()
	return newResponsesWSBillingTestWithHeaders(t, expression, handle, nil, nil, httpEvents...)
}

func newResponsesWSBillingTestWithHeaders(t *testing.T, expression string, handle func(*websocket.Conn, *http.Request), clientHeaders http.Header, upstreamHeaders func(http.ResponseWriter, *http.Request) (http.Header, bool), httpEvents ...string) *responsesWSBillingTest {
	t.Helper()
	user, token := setupResponsesWSRequestTest(t)
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	previousBatch, previousLogs, previousCount, previousQuota := common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit
	common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit = false, true, false, 500000
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		switch key {
		case "billing_setting.billing_mode", "billing_setting.billing_expr", "group_ratio_setting.group_ratio", "perf_metrics_setting.enabled":
			saved[key] = value
		}
		return nil
	}))
	t.Cleanup(func() {
		common.BatchUpdateEnabled, common.LogConsumeEnabled, constant.CountToken, common.QuotaPerUnit = previousBatch, previousLogs, previousCount, previousQuota
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
	expressions, err := common.Marshal(map[string]string{"ws-billing": expression})
	require.NoError(t, err)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"ws-billing":"tiered_expr"}`,
		"billing_setting.billing_expr":    string(expressions),
		"group_ratio_setting.group_ratio": `{"default":1}`,
		"perf_metrics_setting.enabled":    "true",
	}))
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	require.NoError(t, model.DB.Model(user).Updates(map[string]any{"quota": 100000, "setting": `{"billing_preference":"wallet_only"}`}).Error)
	require.NoError(t, model.DB.Model(token).Update("remain_quota", 3000).Error)

	fixture := &responsesWSBillingTest{user: user, token: token, done: make(chan struct{}), upstreamDone: make(chan struct{}), httpDone: make(chan struct{}, 1)}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RDB, common.RedisEnabled = redisClient, true
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	var upstreamClosed sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var responseHeaders http.Header
		if upstreamHeaders != nil {
			var proceed bool
			responseHeaders, proceed = upstreamHeaders(w, r)
			if !proceed {
				return
			}
		}
		if !websocket.IsWebSocketUpgrade(r) {
			if fixture.httpUpstream != nil {
				fixture.httpUpstream(w, r)
				return
			}
			for name, values := range responseHeaders {
				w.Header()[name] = values
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range httpEvents {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, responseHeaders)
		if !assert.NoError(t, err) {
			return
		}
		fixture.connections.Add(1)
		defer ws.Close()
		defer upstreamClosed.Do(func() { close(fixture.upstreamDone) })
		handle(ws, r)
	}))
	t.Cleanup(upstream.Close)
	channel := &model.Channel{Name: "responses-ws-upstream", Key: "upstream-first\nupstream-second", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeOpenAI, Group: "default", Models: "ws-billing", BaseURL: &upstream.URL,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
	modelMapping := `{"ws-billing":"gpt-4o"}`
	channel.ModelMapping = &modelMapping
	channel.SetSetting(dto.ChannelSettings{ResponsesWebSocketEnabled: true})
	channel.SetOtherSettings(dto.ChannelOtherSettings{AllowServiceTier: true})
	require.NoError(t, model.DB.Create(channel).Error)
	fixture.channel = channel
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: channel.Id, Model: "ws-billing", Group: "default", Enabled: true}).Error)
	t.Cleanup(func() {
		require.NoError(t, model.LOG_DB.Where("token_id = ?", token.Id).Delete(&model.Log{}).Error)
		require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).Delete(&model.Ability{}).Error)
		require.NoError(t, model.DB.Delete(channel).Error)
	})
	engine := gin.New()
	engine.GET("/v1/responses", middleware.TokenAuth(), func(c *gin.Context) {
		defer close(fixture.done)
		c.Set(common.RequestIdKey, "responses-ws-billing")
		ResponsesWebSocket(c)
	})
	engine.POST("/v1/responses", middleware.TokenAuth(), middleware.ModelRequestRateLimit(), middleware.Distribute(), func(c *gin.Context) {
		defer func() { fixture.httpDone <- struct{}{} }()
		c.Set(common.RequestIdKey, "responses-http-billing")
		c.Header(common.RequestIdKey, "responses-http-billing")
		Relay(c, types.RelayFormatOpenAIResponses)
	})
	gateway := httptest.NewServer(engine)
	fixture.gatewayURL = gateway.URL
	t.Cleanup(gateway.Close)
	headers := make(http.Header)
	if clientHeaders != nil {
		headers = clientHeaders.Clone()
	}
	headers.Set("Authorization", "Bearer sk-"+token.Key)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", headers)
	require.NoError(t, err)
	fixture.client = client
	t.Cleanup(func() { fixture.closeAndWait(t) })
	return fixture
}

func codexMetadataTestHeaders() (upstream, expected http.Header) {
	expected = http.Header{
		"Openai-Model":                           {"actual-model", "second-model"},
		"X-Models-Etag":                          {"models-v1", "models-v2"},
		"X-Reasoning-Included":                   {""},
		"X-Codex-Turn-State":                     {"turn-state"},
		"X-Request-Id":                           {"upstream-request"},
		"X-Codex-Safety-Buffering-Enabled":       {"false"},
		"X-Codex-Safety-Buffering-Faster-Model":  {"faster-model"},
		"X-Codex-Credits-Has-Credits":            {"0"},
		"X-Codex-Credits-Unlimited":              {"FALSE"},
		"X-Codex-Credits-Balance":                {"0"},
		"X-Codex-Primary-Used-Percent":           {"0"},
		"X-Codex-Primary-Window-Minutes":         {"60"},
		"X-Codex-Primary-Reset-At":               {"0"},
		"X-Codex-Secondary-Used-Percent":         {"50.5"},
		"X-Codex-Secondary-Window-Minutes":       {"10080"},
		"X-Codex-Secondary-Reset-At":             {"1700000000"},
		"X-Other-Model-Primary-Used-Percent":     {"12.5"},
		"X-Other-Model-Primary-Window-Minutes":   {"invalid"},
		"X-Other-Model-Secondary-Used-Percent":   {"0"},
		"X-Other-Model-Secondary-Window-Minutes": {"0"},
		"X-Other-Model-Secondary-Reset-At":       {"0"},
		"X-Other-Model-Limit-Name":               {"Other model"},
	}
	upstream = expected.Clone()
	upstream["oPeNaI-mOdEl"] = upstream["Openai-Model"]
	delete(upstream, "Openai-Model")
	upstream.Set("Authorization", "Bearer upstream-secret")
	upstream.Set("Set-Cookie", "upstream-session=secret")
	upstream.Set(common.RequestIdKey, "upstream-instance")
	upstream.Set("X-Codex-Secret", "secret")
	upstream.Set("Sec-WebSocket-Version", "13")
	upstream.Set("Connection", "Upgrade, X-Hop-Primary-Used-Percent")
	upstream.Set("X-Hop-Primary-Used-Percent", "90")
	return upstream, expected
}

func TestResponsesCodexMetadataSSE(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval, previousRetries := settings.PingIntervalEnabled, settings.PingIntervalSeconds, common.RetryTimes
	settings.PingIntervalEnabled, settings.PingIntervalSeconds, common.RetryTimes = true, 1, 1
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds, common.RetryTimes = previousEnabled, previousInterval, previousRetries
	})
	for _, test := range []struct {
		name         string
		delayHeaders bool
	}{
		{name: "headers before keepalive"},
		{name: "keepalive before headers", delayHeaders: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream, expected := codexMetadataTestHeaders()
			var attempts atomic.Int32
			releaseUpstream := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(releaseUpstream) })
			terminal := `{"type":"response.completed","response":{"id":"sse-metadata","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
			fixture := newResponsesWSBillingTestWithHeaders(t, `tier("request", fixed(0.002))`, nil, nil, func(w http.ResponseWriter, r *http.Request) (http.Header, bool) {
				if attempts.Add(1) == 1 {
					w.Header().Set("X-Stale-Primary-Used-Percent", "90")
					w.Header().Set("Retry-After", "99")
					w.WriteHeader(http.StatusServiceUnavailable)
					return nil, false
				}
				if test.delayHeaders {
					// The client must receive a keepalive before upstream headers arrive.
					select {
					case <-releaseUpstream:
					case <-r.Context().Done():
						return nil, false
					}
				}
				for name, values := range upstream {
					w.Header()[name] = values
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				select {
				case <-releaseUpstream:
					_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
				case <-r.Context().Done():
				}
				return nil, false
			})
			request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hello","stream":true}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response, err := (&http.Client{Timeout: 6 * time.Second}).Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			for name, values := range expected {
				if test.delayHeaders {
					assert.NotContains(t, response.Header, name, "keepalive already committed the response headers")
				} else {
					assert.Equal(t, values, response.Header.Values(name), name)
				}
			}
			assert.Equal(t, "responses-http-billing", response.Header.Get(common.RequestIdKey))
			for _, name := range []string{"Authorization", "Set-Cookie", "X-Codex-Secret", "Sec-WebSocket-Version", "X-Hop-Primary-Used-Percent", "X-Stale-Primary-Used-Percent", "Retry-After"} {
				assert.NotContains(t, response.Header, name)
			}
			reader := bufio.NewReader(response.Body)
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, ": PING\n", line)
			line, err = reader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "\n", line)
			releaseOnce.Do(func() { close(releaseUpstream) })
			body, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Contains(t, string(body), terminal)
			select {
			case <-fixture.httpDone:
			case <-time.After(3 * time.Second):
				t.Fatal("SSE request did not finish")
			}
			assert.Equal(t, int32(2), attempts.Load())
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000})
		})
	}
}

func TestResponsesCodexMetadataWebSocket(t *testing.T) {
	upstream, expected := codexMetadataTestHeaders()
	nativeMetadata := map[string]any{"type": "codex.response.metadata", "headers": map[string]any{"x-models-etag": "native-update"}}
	nativeLimits := map[string]any{"type": "codex.rate_limits", "metered_limit_name": "codex", "rate_limits": map[string]any{"primary": map[string]any{"used_percent": float64(25)}}}
	fixture := newResponsesWSBillingTestWithHeaders(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		for _, lane := range []string{"a", "b"} {
			if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
				return
			}
			sendResponsesWSTestEvent(t, ws, nativeMetadata)
			sendResponsesWSTestEvent(t, ws, nativeLimits)
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "stream_id": lane, "response": map[string]any{"id": lane, "status": "completed", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
		}
		_, _, _ = ws.ReadMessage()
	}, nil, func(http.ResponseWriter, *http.Request) (http.Header, bool) { return upstream, true })
	for index, lane := range []string{"a", "b"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "hello", "stream_id": lane})
		if index == 0 {
			metadata := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "codex.response.metadata", metadata["type"])
			want := make(map[string]any)
			for name, values := range expected {
				want[strings.ToLower(name)] = values[0]
			}
			assert.Equal(t, want, metadata["headers"], "only scalar, allowed metadata reaches the client")
			for _, wantJSON := range []string{
				`{"type":"codex.rate_limits","metered_limit_name":"codex","rate_limits":{"primary":{"used_percent":0,"window_minutes":60,"reset_at":0},"secondary":{"used_percent":50.5,"window_minutes":10080,"reset_at":1700000000}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`,
				`{"type":"codex.rate_limits","metered_limit_name":"other_model","rate_limits":{"primary":{"used_percent":12.5},"secondary":{"used_percent":0,"window_minutes":0,"reset_at":0}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`,
			} {
				var wantEvent map[string]any
				require.NoError(t, common.UnmarshalJsonStr(wantJSON, &wantEvent))
				assert.Equal(t, wantEvent, readResponsesWSTestEvent(t, fixture.client))
			}
		}
		assert.Equal(t, nativeMetadata, readResponsesWSTestEvent(t, fixture.client))
		assert.Equal(t, nativeLimits, readResponsesWSTestEvent(t, fixture.client))
		if index == 0 {
			assert.Equal(t, map[string]any{"type": "response.metadata", "stream_id": lane, "headers": map[string]any{"x-codex-turn-state": "turn-state"}}, readResponsesWSTestEvent(t, fixture.client))
		}
		terminal := readResponsesWSTestEvent(t, fixture.client)
		assert.Equal(t, "response.completed", terminal["type"])
		assert.Equal(t, lane, terminal["stream_id"])
	}
	fixture.closeAndWait(t)
	assert.Equal(t, int32(1), fixture.connections.Load())
	assertResponsesWSAccounting(t, fixture, []int{1000, 1000})
}

func TestResponsesCodexMetadataHandshakeRetry(t *testing.T) {
	for _, finalFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("final failure %t", finalFailure), func(t *testing.T) {
			previousRetries := common.RetryTimes
			common.RetryTimes = 1
			t.Cleanup(func() { common.RetryTimes = previousRetries })
			var attempts atomic.Int32
			fixture := newResponsesWSBillingTestWithHeaders(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "response": map[string]any{"id": "retry", "status": "completed"}})
				_, _, _ = ws.ReadMessage()
			}, nil, func(w http.ResponseWriter, _ *http.Request) (http.Header, bool) {
				if attempts.Add(1) == 1 {
					w.Header().Set("X-Stale-Primary-Used-Percent", "90")
					w.Header().Set("Retry-After", "99")
					w.WriteHeader(http.StatusServiceUnavailable)
					return nil, false
				}
				if finalFailure {
					w.Header().Set("X-Models-Etag", "final")
					w.Header().Set("X-Request-Id", "https://reader:password@auth.example.org/token?key=handshake-secret")
					w.Header().Add("Retry-After", "2")
					w.Header().Add("Retry-After", "3")
					w.Header().Set("Authorization", "Bearer channel-secret")
					w.Header().Set("Set-Cookie", "secret")
					w.WriteHeader(http.StatusTooManyRequests)
					return nil, false
				}
				return http.Header{"X-Models-Etag": {"final"}}, true
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "hello"})
			event := readResponsesWSTestEvent(t, fixture.client)
			if finalFailure {
				assert.Equal(t, "error", event["type"])
				assert.Equal(t, float64(http.StatusTooManyRequests), event["status"])
				assert.Equal(t, map[string]any{
					"x-models-etag": "final", "retry-after": "2",
					"x-request-id": "https://***@auth.example.org/token?key=***",
				}, event["headers"])
			} else {
				assert.Equal(t, map[string]any{"type": "codex.response.metadata", "headers": map[string]any{"x-models-etag": "final"}}, event)
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(2), attempts.Load())
			if finalFailure {
				assertResponsesWSAccounting(t, fixture, nil)
			} else {
				assertResponsesWSAccounting(t, fixture, []int{1000})
			}
		})
	}
}

func TestResponsesCodexMetadataDoesNotAcceptRejectedRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		headers    http.Header
		rate       string
		noMetadata bool
	}{
		{name: "no allowed metadata", headers: http.Header{"X-Codex-Secret": {"secret"}}, noMetadata: true},
		{name: "turn state before rejection", headers: http.Header{"X-Codex-Turn-State": {"turn-state"}}},
		{name: "empty turn state before rejection", headers: http.Header{"X-Codex-Turn-State": {""}}},
		{name: "empty and invalid windows", headers: http.Header{
			"X-Codex-Primary-Used-Percent":   {"0"},
			"X-Codex-Primary-Window-Minutes": {"0"},
			"X-Codex-Secondary-Used-Percent": {"NaN"},
			"X-Other-Primary-Used-Percent":   {"Inf"},
			"X-Hex-Primary-Used-Percent":     {"0x1p2"},
			"X-Codex-Credits-Has-Credits":    {"T"},
			"X-Codex-Credits-Unlimited":      {"true"},
			"X-Codex-Credits-Balance":        {"5"},
		}},
		{name: "valid secondary without valid primary", headers: http.Header{
			"X-Other-Model-Primary-Used-Percent":     {"invalid"},
			"X-Other-Model-Secondary-Used-Percent":   {"20"},
			"X-Other-Model-Secondary-Window-Minutes": {"9223372036854775808"},
			"X-Other-Model-Secondary-Reset-At":       {"0"},
		}, rate: `{"type":"codex.rate_limits","metered_limit_name":"other_model","rate_limits":{"secondary":{"used_percent":20,"reset_at":0}}}`},
		{name: "credits without windows", headers: http.Header{
			"X-Codex-Credits-Has-Credits": {"TRUE"},
			"X-Codex-Credits-Unlimited":   {"1"},
		}, rate: `{"type":"codex.rate_limits","metered_limit_name":"codex","credits":{"has_credits":true,"unlimited":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTestWithHeaders(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "status": 400, "error": map[string]any{"type": "invalid_request_error", "message": "Rejected before generation"}})
				_, _, _ = ws.ReadMessage()
			}, nil, func(http.ResponseWriter, *http.Request) (http.Header, bool) { return tc.headers, true })
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "hello"})
			if !tc.noMetadata {
				metadata := readResponsesWSTestEvent(t, fixture.client)
				assert.Equal(t, "codex.response.metadata", metadata["type"])
				want := make(map[string]any)
				for name, values := range tc.headers {
					want[strings.ToLower(name)] = values[0]
				}
				assert.Equal(t, want, metadata["headers"], "raw header metadata survives invalid numeric fields")
			}
			if tc.rate != "" {
				var want map[string]any
				require.NoError(t, common.UnmarshalJsonStr(tc.rate, &want))
				assert.Equal(t, want, readResponsesWSTestEvent(t, fixture.client))
			}
			rejection := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", rejection["type"], "no synthetic response.* event may precede rejection")
			assert.Equal(t, float64(http.StatusBadRequest), rejection["status"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, nil)
		})
	}
}

func TestResponsesCodexMetadataUpstreamAcceptance(t *testing.T) {
	metadata := []string{
		`{"type":"response.metadata", "stream_id":"planner", "response_id":"metadata-only", "headers":{"x-codex-turn-state":"upstream-state"}}`,
		`{"type":"codex.response.metadata", "headers":{"x-models-etag":"native-update"}, "vendor":"preserved"}`,
		`{"type":"codex.rate_limits", "metered_limit_name":"codex", "rate_limits":{"primary":{"used_percent":25}}}`,
	}
	for _, tc := range []struct {
		name, terminal string
		created        bool
	}{
		{name: "rejected", terminal: `{"type":"error","stream_id":"planner","status":400,"error":{"type":"invalid_request_error","message":"Rejected before generation"}}`},
		{name: "completed", created: true, terminal: `{"type":"response.completed","stream_id":"planner","response":{"id":"generated","status":"completed"}}`},
		{name: "failed after acceptance", created: true, terminal: `{"type":"error","stream_id":"planner","response_id":"generated","status":400,"error":{"type":"invalid_request_error","message":"Failed after generation started"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				for _, event := range metadata {
					if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
						return
					}
				}
				if tc.created {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "generated", "status": "in_progress"}})
				}
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(tc.terminal))) {
					return
				}
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "hello"})
			for _, expected := range metadata {
				require.NoError(t, fixture.client.SetReadDeadline(time.Now().Add(3*time.Second)))
				kind, actual, err := fixture.client.ReadMessage()
				require.NoError(t, err)
				assert.Equal(t, websocket.TextMessage, kind)
				assert.Equal(t, expected, string(actual), "upstream metadata must pass through byte-for-byte")
			}
			if tc.created {
				assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			var terminal map[string]any
			require.NoError(t, common.UnmarshalJsonStr(tc.terminal, &terminal))
			actual := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, terminal["type"], actual["type"])
			assert.Equal(t, terminal["status"], actual["status"])
			fixture.closeAndWait(t)
			if tc.created {
				assertResponsesWSAccounting(t, fixture, []int{1000})
			} else {
				assertResponsesWSAccounting(t, fixture, nil)
			}
		})
	}
}

func TestResponsesCodexMetadataTurnStateAfterRejection(t *testing.T) {
	for _, tc := range []struct {
		name, streamID, turnState string
	}{
		{name: "default stream with empty turn state"},
		{name: "named stream", streamID: "planner", turnState: "turn-state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTestWithHeaders(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "stream_id": tc.streamID, "status": 400, "error": map[string]any{"type": "invalid_request_error", "message": "Rejected before generation"}})
				for index := range 2 {
					if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
						return
					}
					completeResponsesWSTestRequest(t, ws, tc.streamID, fmt.Sprintf("generated-%d", index), 1)
				}
				_, _, _ = ws.ReadMessage()
			}, nil, func(http.ResponseWriter, *http.Request) (http.Header, bool) {
				return http.Header{"X-Codex-Turn-State": {tc.turnState}}, true
			})
			create := map[string]any{"type": "response.create", "model": "ws-billing", "input": "rejected"}
			if tc.streamID != "" {
				create["stream_id"] = tc.streamID
			}
			sendResponsesWSTestEvent(t, fixture.client, create)
			assert.Equal(t, "codex.response.metadata", readResponsesWSTestEvent(t, fixture.client)["type"])
			require.Equal(t, "error", readResponsesWSTestEvent(t, fixture.client)["type"])
			assertResponsesWSAccounting(t, fixture, nil)
			create["input"] = "generated"
			for index := range 2 {
				sendResponsesWSTestEvent(t, fixture.client, create)
				if index == 0 {
					want := map[string]any{"type": "response.metadata", "headers": map[string]any{"x-codex-turn-state": tc.turnState}}
					if tc.streamID != "" {
						want["stream_id"] = tc.streamID
					}
					assert.Equal(t, want, readResponsesWSTestEvent(t, fixture.client))
				}
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"], "turn state must be sent only once per connection")
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, []int{1000, 1000})
		})
	}
}

func TestResponsesInterruptedStreamHealth(t *testing.T) {
	for _, scenario := range []string{"websocket timeout", "sse timeout", "sse client cancel"} {
		t.Run(scenario, func(t *testing.T) {
			events := []string{`{"type":"response.created","response":{"id":"partial","status":"in_progress"}}`, `{"type":"response.output_text.delta","delta":"hello"}`}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				for _, event := range events {
					if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
						return
					}
				}
				_, _, _ = ws.ReadMessage()
			})
			constant.StreamingTimeout = 1
			fixture.httpUpstream = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range events {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}
			if scenario == "websocket timeout" {
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hello"}`)))
				assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
				assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
				_, _, err := fixture.client.ReadMessage()
				require.Error(t, err, "idle timeout closes the incomplete stream")
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hello","stream":true}`))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
				require.NoError(t, err)
				if scenario == "sse client cancel" {
					reader := bufio.NewReader(response.Body)
					for {
						line, err := reader.ReadString('\n')
						require.NoError(t, err)
						if strings.Contains(line, "response.output_text.delta") {
							break
						}
					}
					cancel()
				} else {
					_, err = io.Copy(io.Discard, response.Body)
					require.NoError(t, err)
				}
				require.NoError(t, response.Body.Close())
				select {
				case <-fixture.httpDone:
				case <-time.After(3 * time.Second):
					t.Fatal("interrupted SSE request did not finish")
				}
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000})
			if scenario == "sse client cancel" {
				keys, err := common.RDB.Keys(context.Background(), "perf:ws-billing:*").Result()
				require.NoError(t, err)
				assert.Empty(t, keys, "client cancellation must not affect model health")
				return
			}
			requests, successes := waitPerfCounters(t, 1)
			assert.Equal(t, int64(1), requests)
			assert.Zero(t, successes)
		})
	}
}

func readResponsesWSTestEvent(t *testing.T, client *websocket.Conn) map[string]any {
	t.Helper()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, data, err := client.ReadMessage()
	require.NoError(t, err)
	var event map[string]any
	require.NoError(t, common.Unmarshal(data, &event))
	return event
}

func assertResponsesWSAccounting(t *testing.T, fixture *responsesWSBillingTest, expectedQuotas []int) {
	t.Helper()
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	require.Len(t, logs, len(expectedQuotas))
	var charged int
	for index, quota := range expectedQuotas {
		assert.Equal(t, quota, logs[index].Quota)
		charged += quota
	}
	require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
	require.NoError(t, model.DB.First(fixture.user, fixture.user.Id).Error)
	assert.Equal(t, 3000-charged, fixture.token.RemainQuota)
	assert.Equal(t, charged, fixture.token.UsedQuota)
	assert.Equal(t, 100000-charged, fixture.user.Quota)
	assert.Equal(t, charged, fixture.user.UsedQuota)
}

func TestResponsesWebSocketReusesConnectionAndSettlesEachRequest(t *testing.T) {
	type upstreamRequest struct {
		Authorization      string
		Type               string `json:"type"`
		Model              string `json:"model"`
		PreviousResponseID string `json:"previous_response_id"`
		ServiceTier        string `json:"service_tier"`
	}
	received := make(chan upstreamRequest, 3)
	fixture := newResponsesWSBillingTest(t, `param("service_tier") == "priority" ? tier("priority", p * 4) : tier("base", p * 2)`, func(ws *websocket.Conn, r *http.Request) {
		for index := 1; ; index++ {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var event upstreamRequest
			if !assert.NoError(t, common.Unmarshal(data, &event)) {
				return
			}
			event.Authorization = r.Header.Get("Authorization")
			received <- event
			if index == 2 {
				duplicate := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(duplicate))) {
					return
				}
			}
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","model":"ws-billing","output":[],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, index)
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
				return
			}
		}
	})
	client, user, token := fixture.client, fixture.user, fixture.token

	for index, payload := range []string{
		`{"type":"response.create","model":"ws-billing","input":"first","service_tier":"default"}`,
		`{"type":"response.create","model":"ws-billing","input":"second","previous_response_id":"resp_1","service_tier":"priority"}`,
	} {
		require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(payload)))
		require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
		_, data, err := client.ReadMessage()
		require.NoError(t, err)
		var terminal struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		require.NoError(t, common.Unmarshal(data, &terminal))
		require.Equal(t, "response.completed", terminal.Type, "unexpected response: %s", data)
		assert.Equal(t, fmt.Sprintf("resp_%d", index+1), terminal.Response.ID)
		observed := <-received
		assert.Equal(t, "Bearer upstream-first", observed.Authorization)
		assert.Equal(t, "gpt-4o", observed.Model)
		assert.Equal(t, "response.create", observed.Type)
		if index == 0 {
			assert.Empty(t, observed.PreviousResponseID)
			assert.Equal(t, "default", observed.ServiceTier)
		} else {
			assert.Equal(t, "resp_1", observed.PreviousResponseID)
			assert.Equal(t, "priority", observed.ServiceTier)
		}
	}

	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"exhausted"}`)))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, data, err := client.ReadMessage()
	require.NoError(t, err)
	var rejection struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	require.NoError(t, common.Unmarshal(data, &rejection))
	assert.Equal(t, "error", rejection.Type)
	assert.Equal(t, http.StatusUnauthorized, rejection.Status)
	assert.Equal(t, int32(1), fixture.connections.Load())
	assert.Empty(t, received, "exhausted token must be rejected before contacting upstream")
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	require.Len(t, logs, 2)
	for index, expectedQuota := range []int{1000, 2000} {
		assert.Equal(t, expectedQuota, logs[index].Quota)
		assert.Equal(t, fmt.Sprintf("responses-ws-billing-ws-%d", index), logs[index].RequestId)
		assert.Equal(t, 1000, logs[index].PromptTokens)
		assert.Equal(t, 10, logs[index].CompletionTokens)
	}
	// Routing decisions are persisted per request: the first create selects a
	// channel, later creates on the same connection reuse it as a pin.
	for index, wantDecision := range []string{"attempt:channel_selected", "select:pinned_channel"} {
		var other struct {
			AdminInfo struct {
				RequestPolicy []struct {
					ChannelID int `json:"channel_id"`
					Decision  struct {
						Action string `json:"action"`
						Reason string `json:"reason"`
					} `json:"decision"`
				} `json:"request_policy"`
			} `json:"admin_info"`
		}
		require.NoError(t, common.UnmarshalJsonStr(logs[index].Other, &other))
		var decisions []string
		for _, event := range other.AdminInfo.RequestPolicy {
			assert.Equal(t, fixture.channel.Id, event.ChannelID)
			decisions = append(decisions, event.Decision.Action+":"+event.Decision.Reason)
		}
		assert.Contains(t, decisions, wantDecision, "request %d policy events: %v", index, decisions)
	}
	require.NoError(t, model.DB.First(token, token.Id).Error)
	require.NoError(t, model.DB.First(user, user.Id).Error)
	assert.Zero(t, token.RemainQuota)
	assert.Equal(t, 3000, token.UsedQuota)
	assert.Equal(t, 97000, user.Quota)
	assert.Equal(t, 3000, user.UsedQuota)
}

func newResponsesWSModelSwitchTest(t *testing.T, mappedModel string, handle func(*websocket.Conn, *http.Request)) *responsesWSBillingTest {
	t.Helper()
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, handle)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"ws-billing":"tiered_expr","ws-switch":"tiered_expr"}`,
		"billing_setting.billing_expr": `{"ws-billing":"tier(\"request\", fixed(0.0002))","ws-switch":"tier(\"request\", fixed(0.0004))"}`,
	}))
	mapping, err := common.Marshal(map[string]string{"ws-billing": "gpt-4o", "ws-switch": mappedModel})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{
		"models": "ws-billing,ws-switch", "model_mapping": string(mapping),
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: fixture.channel.Id, Model: "ws-switch", Group: "default", Enabled: true}).Error)
	return fixture
}

func TestResponsesWebSocketSwitchesModels(t *testing.T) {
	for _, tc := range []struct {
		name         string
		streams      [3]string
		concurrent   bool
		continuation bool
	}{
		{name: "default stream"},
		{name: "same named stream", streams: [3]string{"planner", "planner", "planner"}},
		{name: "different streams", streams: [3]string{"planner", "critic", "planner"}},
		{name: "concurrent models", streams: [3]string{"planner", "critic", "planner"}, concurrent: true},
		{name: "previous response forwarded", continuation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []any{
				map[string]any{"role": "user", "content": "First question"},
				map[string]any{"role": "assistant", "content": "Previous answer"},
				map[string]any{"role": "user", "content": "Continue with the selected model"},
			}
			reserved := make(chan int, 3)
			tokenID := make(chan int, 1)
			fixture := newResponsesWSModelSwitchTest(t, "gpt-4.1", func(ws *websocket.Conn, r *http.Request) {
				assert.Equal(t, "Bearer upstream-first", r.Header.Get("Authorization"))
				id := <-tokenID
				for index, expectedModel := range []string{"gpt-4o", "gpt-4.1", "gpt-4o"} {
					create := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, expectedModel, create["model"])
					assert.Equal(t, input, create["input"])
					if tc.continuation && index > 0 {
						assert.Equal(t, fmt.Sprintf("switch-%d", index-1), create["previous_response_id"])
					} else {
						assert.NotContains(t, create, "previous_response_id")
					}
					var token model.Token
					require.NoError(t, model.DB.First(&token, id).Error)
					reserved <- token.RemainQuota
					if tc.concurrent && index == 0 {
						sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": tc.streams[0], "response": map[string]any{"id": "switch-0", "status": "in_progress"}})
						continue
					}
					completeResponsesWSTestRequest(t, ws, tc.streams[index], fmt.Sprintf("switch-%d", index), 100)
					if tc.concurrent && index == 1 {
						completeResponsesWSTestRequest(t, ws, tc.streams[0], "switch-0", 100)
					}
				}
				_, _, _ = ws.ReadMessage()
			})
			tokenID <- fixture.token.Id
			completed := make(map[string]string)
			for index, modelName := range []string{"ws-billing", "ws-switch", "ws-billing"} {
				create := map[string]any{"type": "response.create", "model": modelName, "input": input}
				if tc.streams[index] != "" {
					create["stream_id"] = tc.streams[index]
				}
				if tc.continuation && index > 0 {
					create["previous_response_id"] = fmt.Sprintf("switch-%d", index-1)
				}
				sendResponsesWSTestEvent(t, fixture.client, create)
				count := 1
				if tc.concurrent && index == 1 {
					count = 2
				}
				for range count {
					event := readResponsesWSTestEvent(t, fixture.client)
					if tc.concurrent && index == 0 {
						require.Equal(t, "response.created", event["type"], "%v", event)
						continue
					}
					require.Equal(t, "response.completed", event["type"], "%v", event)
					id := event["response"].(map[string]any)["id"].(string)
					assert.NotContains(t, completed, id)
					completed[id], _ = event["stream_id"].(string)
				}
				assert.Equal(t, []int{2900, 2700, 2600}[index], <-reserved)
			}
			assert.Equal(t, map[string]string{"switch-0": tc.streams[0], "switch-1": tc.streams[1], "switch-2": tc.streams[2]}, completed)
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("request_id").Find(&logs).Error)
			require.Len(t, logs, 3)
			for index, log := range logs {
				assert.Equal(t, fmt.Sprintf("responses-ws-billing-ws-%d", index), log.RequestId)
				assert.Equal(t, []string{"ws-billing", "ws-switch", "ws-billing"}[index], log.ModelName)
				assert.Equal(t, []int{100, 200, 100}[index], log.Quota)
			}
			require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
			require.NoError(t, model.DB.First(fixture.user, fixture.user.Id).Error)
			assert.Equal(t, 2600, fixture.token.RemainQuota)
			assert.Equal(t, 400, fixture.token.UsedQuota)
			assert.Equal(t, 99600, fixture.user.Quota)
			assert.Equal(t, 400, fixture.user.UsedQuota)
		})
	}
}

func TestResponsesWebSocketModelSwitchRevalidatesAccess(t *testing.T) {
	for _, scenario := range []string{"token model access revoked", "token revoked", "channel model disabled", "channel disabled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newResponsesWSModelSwitchTest(t, "gpt-4.1", func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				_, _, err := ws.ReadMessage()
				assert.Error(t, err, "unauthorized model switch must not reach upstream")
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "first"})
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
			status := http.StatusForbidden
			switch scenario {
			case "token model access revoked":
				fixture.token.ModelLimitsEnabled, fixture.token.ModelLimits = true, "ws-billing"
				require.NoError(t, fixture.token.Update())
			case "token revoked":
				fixture.token.Status = common.TokenStatusDisabled
				require.NoError(t, fixture.token.SelectUpdate())
				status = http.StatusUnauthorized
			case "channel model disabled":
				require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ? AND model = ?", fixture.channel.Id, "ws-switch").Update("enabled", false).Error)
			case "channel disabled":
				require.NoError(t, model.DB.Model(fixture.channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "event_id": "switch", "stream_id": "critic", "model": "ws-switch", "input": "second"})
			rejection := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", rejection["type"])
			assert.Equal(t, float64(status), rejection["status"])
			assert.Equal(t, "switch", rejection["event_id"])
			assert.Equal(t, "critic", rejection["stream_id"])
			_, _, err := fixture.client.ReadMessage()
			assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation), "%v", err)
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, []int{100})
		})
	}
}

func TestResponsesWebSocketAzureAPIVersionReuse(t *testing.T) {
	const defaultVersion = "2025-04-01-preview"
	previousVersion := constant.AzureDefaultAPIVersion
	constant.AzureDefaultAPIVersion = defaultVersion
	t.Cleanup(func() { constant.AzureDefaultAPIVersion = previousVersion })

	for _, tc := range []struct {
		name, apiVersion, responsesVersion, wantVersion string
		updatedVersion                                  *string
		reconnect                                       bool
	}{
		{name: "custom api version", apiVersion: "2025-03-01-preview", wantVersion: "2025-03-01-preview"},
		{name: "default api version", wantVersion: defaultVersion},
		{name: "responses version override", apiVersion: "2025-03-01-preview", responsesVersion: "2025-05-01-preview", updatedVersion: common.GetPointer("2025-06-01-preview"), wantVersion: "2025-05-01-preview"},
		{name: "api version changed", apiVersion: defaultVersion, updatedVersion: common.GetPointer("2025-03-01-preview"), wantVersion: defaultVersion, reconnect: true},
		{name: "api version cleared", apiVersion: "2025-03-01-preview", updatedVersion: common.GetPointer(""), wantVersion: "2025-03-01-preview", reconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectedQuotas := []int{100, 100}
			if tc.reconnect {
				expectedQuotas = expectedQuotas[:1]
			}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, r *http.Request) {
				assert.Equal(t, "/cognitiveservices.azure.com/openai/responses", r.URL.Path)
				assert.Equal(t, tc.wantVersion, r.URL.Query().Get("api-version"))
				for index := range expectedQuotas {
					create := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "gpt-4o", create["model"])
					completeResponsesWSTestRequest(t, ws, "", fmt.Sprintf("azure-%d", index), 100)
				}
				_, _, err := ws.ReadMessage()
				assert.Error(t, err, "a changed API version must not send another request on the old connection")
			})
			// The adaptor recognizes Cognitive Services by its base URL text.
			baseURL := fixture.channel.GetBaseURL() + "/cognitiveservices.azure.com"
			fixture.channel.SetOtherSettings(dto.ChannelOtherSettings{AzureResponsesVersion: tc.responsesVersion})
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{
				"type": constant.ChannelTypeAzure, "base_url": baseURL, "other": tc.apiVersion, "settings": fixture.channel.OtherSettings,
			}).Error)

			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"})
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			if tc.updatedVersion != nil {
				require.NoError(t, model.DB.Model(fixture.channel).Update("other", *tc.updatedVersion).Error)
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "second"})
			event := readResponsesWSTestEvent(t, fixture.client)
			if tc.reconnect {
				assert.Equal(t, "error", event["type"])
				assert.Equal(t, float64(http.StatusForbidden), event["status"])
				body, err := common.Marshal(event)
				require.NoError(t, err)
				assert.Contains(t, string(body), "reconnect required")
				_, _, err = fixture.client.ReadMessage()
				assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation), "%v", err)
			} else {
				require.Equal(t, "response.completed", event["type"], "%v", event)
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, expectedQuotas)
		})
	}
}

func TestResponsesWebSocketModelSwitchChecksResolvedURL(t *testing.T) {
	for _, tc := range []struct {
		name, mappedModel string
		allowed           bool
	}{
		{name: "same resolved address", mappedModel: "gpt-4o", allowed: true},
		{name: "different resolved address", mappedModel: "gpt-4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSModelSwitchTest(t, tc.mappedModel, func(ws *websocket.Conn, r *http.Request) {
				assert.Equal(t, "/models/gpt-4o/responses", r.URL.Path)
				assert.Equal(t, "upstream-first", r.URL.Query().Get("api_key"))
				readResponsesWSTestEvent(t, ws)
				completeResponsesWSTestRequest(t, ws, "", "first", 100)
				if tc.allowed {
					create := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "gpt-4o", create["model"])
					completeResponsesWSTestRequest(t, ws, "", "second", 100)
				}
				_, _, err := ws.ReadMessage()
				assert.Error(t, err, "a different upstream address must not reuse this connection")
			})
			fixture.channel.SetOtherSettings(dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
				IncomingPath: "/v1/responses", UpstreamPath: "/models/{model}/responses",
				Auth: &dto.AdvancedCustomRouteAuth{Type: dto.AdvancedCustomAuthTypeQuery, Name: "api_key", Value: "{api_key}"},
			}}}})
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"type": constant.ChannelTypeAdvancedCustom, "settings": fixture.channel.OtherSettings}).Error)
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"})
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-switch", "input": "second"})
			event := readResponsesWSTestEvent(t, fixture.client)
			expectedQuotas := []int{100}
			if tc.allowed {
				assert.Equal(t, "response.completed", event["type"])
				expectedQuotas = append(expectedQuotas, 200)
			} else {
				assert.Equal(t, "error", event["type"])
				assert.Equal(t, float64(http.StatusForbidden), event["status"])
				body, err := common.Marshal(event)
				require.NoError(t, err)
				assert.Contains(t, string(body), "reconnect required")
				assert.NotContains(t, string(body), "upstream-first")
				assert.NotContains(t, string(body), "api_key")
				_, _, err = fixture.client.ReadMessage()
				assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation), "%v", err)
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, expectedQuotas)
		})
	}
}

func TestResponsesWebSocketModelSwitchChecksResolvedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, header, initial, next, handshake string
		remove, allowed                        bool
	}{
		{name: "authorization changed", header: "Authorization", initial: "Bearer credential-a", next: "Bearer credential-b", handshake: "Bearer credential-a"},
		{name: "custom header changed", header: "X-Project", initial: "project-a", next: "project-b", handshake: "project-a"},
		{name: "header added", header: "X-Project", next: "project-b"},
		{name: "header removed", header: "X-Project", initial: "project-a", handshake: "project-a", remove: true},
		{name: "equivalent key placeholder", header: "aUtHoRiZaTiOn", initial: "Bearer {api_key}", next: "Bearer upstream-first", handshake: "Bearer upstream-first", allowed: true},
		{name: "equivalent header casing", header: "x-PrOjEcT", initial: "project-a", next: "project-a", handshake: "project-a", allowed: true},
		{name: "explicit default authorization", header: "Authorization", next: "Bearer {api_key}", handshake: "Bearer upstream-first", allowed: true},
		{name: "removed redundant authorization", header: "Authorization", initial: "Bearer {api_key}", handshake: "Bearer upstream-first", remove: true, allowed: true},
		{name: "content type forced after overrides", header: "Content-Type", initial: "application/a", next: "application/b", handshake: "application/json", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSModelSwitchTest(t, "gpt-4.1", func(ws *websocket.Conn, r *http.Request) {
				assert.Equal(t, tc.handshake, r.Header.Get(tc.header))
				initial := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "gpt-4o", initial["model"])
				completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				if tc.allowed {
					create := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "gpt-4.1", create["model"])
					completeResponsesWSTestRequest(t, ws, "critic", "second", 100)
				}
				_, unexpected, err := ws.ReadMessage()
				assert.Error(t, err, "a request with different handshake headers must not reach upstream: %s", unexpected)
			})
			var operations []map[string]any
			if tc.initial != "" {
				operations = append(operations, map[string]any{"mode": "set_header", "path": tc.header, "value": tc.initial})
			}
			override := map[string]any{
				"mode": "set_header", "path": strings.ToUpper(tc.header), "value": tc.next,
				"conditions": map[string]any{"upstream_model": "gpt-4.1"},
			}
			if tc.remove {
				override["mode"] = "delete_header"
				delete(override, "value")
			}
			operations = append(operations, override)
			paramOverride, err := common.Marshal(map[string]any{"operations": operations})
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(fixture.channel).Update("param_override", string(paramOverride)).Error)

			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "first"})
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "event_id": "switch-headers", "stream_id": "critic", "model": "ws-switch", "input": "second"})
			event := readResponsesWSTestEvent(t, fixture.client)
			expectedQuotas := []int{100}
			if tc.allowed {
				require.Equal(t, "response.completed", event["type"], "%v", event)
				expectedQuotas = append(expectedQuotas, 200)
			} else {
				assert.Equal(t, "error", event["type"])
				assert.Equal(t, float64(http.StatusForbidden), event["status"])
				assert.Equal(t, "switch-headers", event["event_id"])
				assert.Equal(t, "critic", event["stream_id"])
				body, err := common.Marshal(event)
				require.NoError(t, err)
				assert.Contains(t, string(body), "reconnect required")
				for _, secret := range []string{"credential-a", "credential-b", "upstream-first", "upstream-second", "project-a", "project-b"} {
					assert.NotContains(t, string(body), secret)
				}
				_, _, err = fixture.client.ReadMessage()
				assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation), "%v", err)
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, expectedQuotas)
		})
	}
}

// Both transports must reach the same upstream target with the same
// credential placement and settle identically for every supported channel type.
func TestResponsesWebSocketDialsNativeResponsesChannelTypes(t *testing.T) {
	type upstreamTarget struct {
		Path, Authorization, QueryKey string
	}
	queryAuthRoute := dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
		IncomingPath: "/v1/responses", UpstreamPath: "/upstream/responses",
		Auth: &dto.AdvancedCustomRouteAuth{Type: dto.AdvancedCustomAuthTypeQuery, Name: "api_key", Value: "{api_key}"},
	}}}}
	bearer := func(key string) upstreamTarget {
		return upstreamTarget{Path: "/v1/responses", Authorization: "Bearer " + key}
	}
	for _, tc := range []struct {
		name          string
		channelType   int
		otherSettings dto.ChannelOtherSettings
		want          func(key string) upstreamTarget
	}{
		{name: "new api", channelType: constant.ChannelTypeNewAPI, want: bearer},
		{name: "sub2api", channelType: constant.ChannelTypeSub2API, want: bearer},
		{name: "advanced custom query auth", channelType: constant.ChannelTypeAdvancedCustom, otherSettings: queryAuthRoute, want: func(key string) upstreamTarget {
			return upstreamTarget{Path: "/upstream/responses", QueryKey: key}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const terminal = `{"type":"response.completed","response":{"id":"first","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`
			targets := make(chan upstreamTarget, 2)
			observe := func(r *http.Request) {
				targets <- upstreamTarget{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), QueryKey: r.URL.Query().Get("api_key")}
			}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, r *http.Request) {
				observe(r)
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
					return
				}
				_, _, _ = ws.ReadMessage()
			})
			fixture.httpUpstream = func(w http.ResponseWriter, r *http.Request) {
				observe(r)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", terminal)
			}
			fixture.channel.SetOtherSettings(tc.otherSettings)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"type": tc.channelType, "settings": fixture.channel.OtherSettings}).Error)

			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			assert.Equal(t, tc.want("upstream-first"), <-targets)

			request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hi","stream":true}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Contains(t, string(body), terminal)
			select {
			case <-fixture.httpDone:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP request did not finish")
			}
			// The polling multi-key channel rotates to its second key for the HTTP request.
			assert.Equal(t, tc.want("upstream-second"), <-targets)
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000, 1000})
		})
	}
}

func TestResponsesWebSocketDisconnectSettlesDeliveredOutputOnce(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("output", c * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"partial","status":"in_progress"}}`,
			`{"type":"response.output_text.delta","delta":"hello"}`,
		} {
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
				return
			}
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi","max_output_tokens":1}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	delta := readResponsesWSTestEvent(t, fixture.client)
	require.Equal(t, "response.output_text.delta", delta["type"])
	assert.Equal(t, "hello", delta["delta"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1})
	keys, err := common.RDB.Keys(context.Background(), "perf:ws-billing:*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys, "client cancellation must not affect model health")
}

func TestResponsesWebSocketCancelErrorDoesNotFinishActiveRequest(t *testing.T) {
	complete := make(chan struct{})
	defer close(complete)
	allowCreated := make(chan struct{}, 1)
	defer close(allowCreated)
	receivedCreate := make(chan struct{}, 1)
	fixture := newResponsesWSBillingTest(t, `tier("base", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		_, create, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.Contains(t, string(create), `"type":"response.create"`) {
			return
		}
		receivedCreate <- struct{}{}
		<-allowCreated
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.JSONEq(t, `{"type":"response.cancel","response_id":"wrong"}`, string(cancel)) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"response_not_found","message":"No such response"}}`))) {
			return
		}
		<-complete
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"active","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	select {
	case <-receivedCreate:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive the initial request")
	}
	// Queue cancellation before upstream acceptance. An unsupported event
	// acknowledges that the read loop processed the preceding control.
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"wrong"}`)))
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"unsupported"}`)))
	assert.Equal(t, float64(http.StatusBadRequest), readResponsesWSTestEvent(t, fixture.client)["status"])
	allowCreated <- struct{}{}
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	cancelError := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", cancelError["type"])
	assert.Equal(t, float64(http.StatusBadRequest), cancelError["status"])
	complete <- struct{}{}
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000})
}

func TestResponsesWebSocketInitialUpstreamRejectionRefundsReservation(t *testing.T) {
	for _, tc := range []struct {
		name, upstream, wantType, wantMessage string
		wantHeaders, wantErrorHeaders         string
		status                                int
	}{
		{name: "structured error", upstream: `{"type":"error","response_id":"rejected","status":400,"error":{"type":"invalid_request_error","code":"invalid_input","message":"Invalid input"}}`, status: http.StatusBadRequest, wantType: "invalid_request_error", wantMessage: "Invalid input"},
		// A frame without an error object is still reported as a request error.
		{name: "bare error", upstream: `{"type":"error","status":500,"message":"upstream rejected","headers":{"retry-after":"4"}}`, status: http.StatusInternalServerError, wantType: "invalid_request_error", wantMessage: "upstream rejected", wantHeaders: `{"retry-after":"4"}`},
		{name: "alias rate limit", upstream: `{"type":"error","status_code":429,"headers":{"retry-after":"3","x-count":2},"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 429, wantType: "invalid_request_error", wantMessage: "Rejected", wantHeaders: `{"retry-after":"3"}`, wantErrorHeaders: `{"retry-after":2}`},
		{name: "alias server error", upstream: `{"type":"error","status_code":500,"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 500, wantType: "invalid_request_error", wantMessage: "Rejected", wantErrorHeaders: `{"retry-after":2}`},
		{name: "nested headers", upstream: `{"type":"error","status":429,"headers":{"retry-after":"9"},"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 429, wantType: "invalid_request_error", wantMessage: "Rejected", wantHeaders: `{"retry-after":"9"}`, wantErrorHeaders: `{"retry-after":2}`},
		{name: "status precedence", upstream: `{"type":"error","status":503,"status_code":429,"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 503, wantType: "invalid_request_error", wantMessage: "Rejected", wantErrorHeaders: `{"retry-after":2}`},
		{name: "zero status alias", upstream: `{"type":"error","status":0,"status_code":500,"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 500, wantType: "invalid_request_error", wantMessage: "Rejected", wantErrorHeaders: `{"retry-after":2}`},
		{name: "missing status", upstream: `{"type":"error","extra":true,"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 400, wantType: "invalid_request_error", wantMessage: "Rejected", wantErrorHeaders: `{"retry-after":2}`},
		{name: "invalid status", upstream: `{"type":"error","status":200,"status_code":500,"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 400, wantType: "invalid_request_error", wantMessage: "Rejected", wantErrorHeaders: `{"retry-after":2}`},
		{name: "invalid alias", upstream: `{"type":"error","status_code":600,"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":2,"x-values":["a","b"]}},"unknown":"drop"}`, status: 400, wantType: "invalid_request_error", wantMessage: "Rejected", wantErrorHeaders: `{"retry-after":2}`},
		{
			name:     "sensitive headers and scalar metadata",
			upstream: `{"type":"error","status":429,"headers":{"AuThOrIzAtIoN":"Bearer channel-secret","Proxy-Authorization":"Basic proxy-secret","api-key":"channel-secret","X-API-Key":"channel-secret","Cookie":"session=secret","Set-Cookie":"session=secret","Location":"https://reader:password@auth.example.org/token?key=secret","X-Secret":"secret","ReTrY-AfTeR":"3","X-Codex-Primary-Used-Percent":0,"X-Reasoning-Included":false,"x-models-etag":""},"error":{"type":"invalid_request_error","message":"Rejected","headers":{"authorization":"Bearer channel-secret","proxy-authorization":"Basic proxy-secret","API-KEY":"channel-secret","x-api-key":"channel-secret","COOKIE":"session=secret","set-cookie":"session=secret","location":"https://reader:password@auth.example.org/token?key=secret","x-secret":"secret","Retry-After":2,"X-Request-Id":["req-1",7,true]}}}`,
			status:   429, wantType: "invalid_request_error", wantMessage: "Rejected",
			wantHeaders:      `{"ReTrY-AfTeR":"3","X-Codex-Primary-Used-Percent":0,"X-Reasoning-Included":false,"x-models-etag":""}`,
			wantErrorHeaders: `{"Retry-After":2,"X-Request-Id":["req-1",7,true]}`,
		},
		{
			name:     "credentials in allowed values",
			upstream: `{"type":"error","status":429,"headers":{"x-request-id":"https://reader:password@auth.example.org/token?key=secret","x-models-etag":["$UPSTREAM/private/route?key=secret",false,0]},"error":{"type":"invalid_request_error","message":"Rejected","headers":{"x-request-id":["api_key:channel-secret","https://auth.example.org/token?client_secret=secret"],"x-models-etag":"$UPSTREAM/private/route?key=secret"}}}`,
			status:   429, wantType: "invalid_request_error", wantMessage: "Rejected",
			wantHeaders:      `{"x-request-id":"https://***@auth.example.org/token?key=***","x-models-etag":["$UPSTREAM/***/***?key=***",false,0]}`,
			wantErrorHeaders: `{"x-request-id":["api_key:***","https://auth.example.org/token?client_secret=***"],"x-models-etag":"$UPSTREAM/***/***?key=***"}`,
		},
		{
			name:     "hop by hop metadata",
			upstream: `{"type":"error","status":429,"headers":{"cOnNeCtIoN":"X-Models-Etag, X-Codex-Primary-Used-Percent","x-models-etag":"secret","x-codex-primary-used-percent":"secret","retry-after":"3"},"error":{"type":"invalid_request_error","message":"Rejected","headers":{"Connection":["X-Request-Id","Retry-After"],"x-request-id":"secret","retry-after":2,"X-Models-Etag":"safe"}}}`,
			status:   429, wantType: "invalid_request_error", wantMessage: "Rejected",
			wantHeaders: `{"retry-after":"3"}`, wantErrorHeaders: `{"X-Models-Etag":"safe"}`,
		},
		{
			name:     "complex header values",
			upstream: `{"type":"error","status":429,"headers":{"retry-after":{"Authorization":"secret"},"x-models-etag":["safe",{"Authorization":"secret"}],"x-request-id":[["secret"]],"x-codex-turn-state":null,"x-codex-primary-used-percent":[],"x-reasoning-included":[true,null]},"error":{"type":"invalid_request_error","message":"Rejected","headers":{"retry-after":[[2]],"x-request-id":{"Authorization":"secret"}}}}`,
			status:   429, wantType: "invalid_request_error", wantMessage: "Rejected",
		},
		{
			name:     "non object header containers",
			upstream: `{"type":"error","status":429,"headers":[{"Authorization":"secret"}],"error":{"type":"invalid_request_error","message":"Rejected","headers":"https://reader:password@auth.example.org/token?key=secret"}}`,
			status:   429, wantType: "invalid_request_error", wantMessage: "Rejected",
		},
		{
			name:     "empty header containers",
			upstream: `{"type":"error","status":429,"headers":{},"error":{"type":"invalid_request_error","message":"Rejected","headers":null}}`,
			status:   429, wantType: "invalid_request_error", wantMessage: "Rejected",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preConsumed := make(chan int, 1)
			tokenID := make(chan int, 1)
			// Pre-consume no longer estimates completion tokens, so an output-priced expression reserves nothing to refund.
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, request *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				var token model.Token
				if !assert.NoError(t, model.DB.First(&token, <-tokenID).Error) {
					return
				}
				preConsumed <- token.RemainQuota
				upstream := strings.ReplaceAll(tc.upstream, "$UPSTREAM", "http://"+request.Host)
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(upstream))) {
					return
				}
				_, _, _ = ws.ReadMessage()
			})
			tokenID <- fixture.token.Id
			// Refund is asynchronous. Observe committed token writes instead of waiting
			// a fixed delay or returning while its worker still uses the test database.
			updates := make(chan struct{}, 4)
			require.NoError(t, model.DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("responses-ws-refund", func(tx *gorm.DB) {
				if tx.Error == nil && tx.Statement.Table == "tokens" {
					select {
					case updates <- struct{}{}:
					default:
					}
				}
			}))
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"create-rejected","stream_id":"planner","model":"ws-billing","input":"hi","max_output_tokens":10}`)))
			rejection := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", rejection["type"])
			assert.Equal(t, float64(tc.status), rejection["status"])
			assert.Equal(t, "create-rejected", rejection["event_id"])
			assert.Equal(t, "planner", rejection["stream_id"])
			rejectionError, ok := rejection["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.wantType, rejectionError["type"])
			assert.Equal(t, tc.wantMessage, rejectionError["message"])
			for _, expected := range []struct {
				object  map[string]any
				headers string
			}{{rejection, tc.wantHeaders}, {rejectionError, tc.wantErrorHeaders}} {
				if expected.headers == "" {
					assert.NotContains(t, expected.object, "headers")
					continue
				}
				actual, err := common.Marshal(expected.object["headers"])
				require.NoError(t, err)
				// The httptest upstream uses loopback; its path must also be
				// masked using the channel address, not just the IP pattern.
				maskedUpstream := strings.ReplaceAll(*fixture.channel.BaseURL, "127.0.0.1", "***.***.***.***")
				assert.JSONEq(t, strings.ReplaceAll(expected.headers, "$UPSTREAM", maskedUpstream), string(actual))
			}
			assert.NotContains(t, rejection, "unknown")
			assert.Equal(t, 2000, <-preConsumed, "the rejected request reserved quota before contacting upstream")
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			for {
				require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
				if fixture.token.RemainQuota == 3000 {
					break
				}
				select {
				case <-updates:
				case <-deadline.C:
					t.Fatal("initial rejection did not refund the token reservation")
				}
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, nil)
		})
	}
}

// TEST_RESPONSES_SQL_DSN / TEST_RESPONSES_LOG_SQL_DSN optionally run these
// entry-point regressions against isolated real MySQL/PostgreSQL databases.
func TestResponsesStreamOutcomesPreserveAccounting(t *testing.T) {
	for _, transport := range []string{"websocket", "http-sse"} {
		for _, tc := range []struct {
			name, expression, terminal string
			// sseTerminal is the flat error envelope used by the Responses SSE
			// protocol; the WebSocket protocol nests the error instead.
			sseTerminal string
			delta       bool
			failed      bool
			ignored     bool
		}{
			{name: "failed-null-fixed", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed","usage":null,"error":{"code":"server_error","message":"sensitive upstream detail"}}}`, failed: true},
			{name: "failed-missing-fixed", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed"}}`, failed: true},
			{name: "failed-actual-usage", expression: `tier("input", p * 2)`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, failed: true},
			{name: "failed-estimated-output", expression: `tier("output", c * 2000)`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed","usage":null}}`, delta: true, failed: true},
			{name: "error-after-created", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"error","status":500,"error":{"type":"server_error","code":"server_error","message":"Internal server error"}}`, sseTerminal: `{"type":"error","code":"server_error","message":"Internal server error","param":null,"sequence_number":2}`, failed: true},
			{name: "business-error-after-created", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Input too long"}}`, sseTerminal: `{"type":"error","code":"context_length_exceeded","message":"Input too long","param":null,"sequence_number":2}`, failed: true, ignored: true},
			{name: "completed-at-output-limit", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.incomplete","response":{"id":"first","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1000,"output_tokens":1,"total_tokens":1001}}}`},
			{name: "completed-zero-fixed", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.completed","response":{"id":"first","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				events := []string{`{"type":"response.created","response":{"id":"first","status":"in_progress"}}`}
				if tc.delta {
					events = append(events, `{"type":"response.output_text.delta","delta":"hello"}`)
				}
				terminal := tc.terminal
				if transport == "http-sse" && tc.sseTerminal != "" {
					terminal = tc.sseTerminal
				}
				events = append(events, terminal)
				fixture := newResponsesWSBillingTest(t, tc.expression, func(ws *websocket.Conn, _ *http.Request) {
					for turn := 0; ; turn++ {
						_, request, err := ws.ReadMessage()
						if err != nil {
							return
						}
						var body map[string]any
						if !assert.NoError(t, common.Unmarshal(request, &body)) {
							return
						}
						assert.Equal(t, "planner", body["stream_id"])
						assert.Equal(t, "gpt-4o", body["model"])
						output := events
						if transport == "http-sse" || turn > 0 {
							output = []string{`{"type":"response.completed","stream_id":"planner","response":{"id":"second","status":"completed","usage":{"input_tokens":1000,"output_tokens":1,"total_tokens":1001}}}`}
						}
						for _, event := range output {
							if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
								return
							}
						}
					}
				}, events...)
				setting.ModelRequestRateLimitEnabled = true
				setting.ModelRequestRateLimitDurationMinutes = 1
				setting.ModelRequestRateLimitSuccessCount = 1
				setting.ModelRequestRateLimitCount = 0
				if transport == "http-sse" {
					request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hi","stream":true,"max_output_tokens":1}`))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
					request.Header.Set("Content-Type", "application/json")
					response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
					require.NoError(t, err)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					assert.Equal(t, http.StatusOK, response.StatusCode)
					assert.Contains(t, string(body), terminal)
				} else {
					require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"planner","model":"ws-billing","input":"hi","max_output_tokens":1}`)))
					for _, expected := range events {
						actual, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
						require.NoError(t, err)
						assert.JSONEq(t, expected, string(actual), "upstream frames must be delivered once and unchanged")
					}
				}
				quotas := []int{1000}
				if tc.failed {
					// The first terminal must complete settlement and middleware before
					// admitting this immediately following request.
					require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"planner","model":"ws-billing","input":"next","max_output_tokens":1}`)))
					assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
					quotas = append(quotas, 1000)
				}
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"limited","stream_id":"planner","model":"ws-billing","input":"limited"}`)))
				limited := readResponsesWSTestEvent(t, fixture.client)
				assert.Equal(t, float64(http.StatusTooManyRequests), limited["status"])
				assert.Equal(t, "planner", limited["stream_id"])
				assert.Equal(t, "limited", limited["event_id"])
				fixture.closeAndWait(t)
				assertResponsesWSAccounting(t, fixture, quotas)
				var logs []model.Log
				require.NoError(t, model.LOG_DB.Where("token_id = ? AND type = ?", fixture.token.Id, model.LogTypeConsume).Order("id").Find(&logs).Error)
				require.Len(t, logs, len(quotas))
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
				stream := other["stream_status"].(map[string]any)
				if tc.failed {
					assert.Equal(t, "error", stream["status"])
					assert.Equal(t, "failed", stream["response_status"])
				} else {
					assert.Equal(t, "ok", stream["status"])
					if tc.name == "completed-at-output-limit" {
						assert.Equal(t, "incomplete", stream["response_status"])
					} else {
						assert.Equal(t, "completed", stream["response_status"])
					}
				}
				assert.NotContains(t, logs[0].Other, "sensitive upstream detail")
				expectedRequests := int64(len(quotas))
				if tc.ignored {
					expectedRequests--
				}
				requests, successes := waitPerfCounters(t, expectedRequests)
				assert.Equal(t, expectedRequests, requests)
				assert.Equal(t, int64(1), successes)
			})
		}
	}
}

func TestResponsesHTTPHealthCountsFinalResult(t *testing.T) {
	for _, tc := range []struct {
		name, code            string
		firstStatus, attempts int
		success               bool
		ignored               bool
	}{
		{"business rejection", "context_length_exceeded", 400, 1, false, true},
		{"credentials rejected as 400", "invalid_api_key", 400, 1, false, false},
		{"retry succeeds", "server_error", 500, 2, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {})
			oldRetries := common.RetryTimes
			common.RetryTimes = 1
			t.Cleanup(func() { common.RetryTimes = oldRetries })
			var attempts atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if attempts.Add(1) == 1 {
					w.WriteHeader(tc.firstStatus)
					_, _ = fmt.Fprintf(w, `{"error":{"type":"%s","code":"%s","message":"test rejection"}}`, tc.code, tc.code)
					return
				}
				_, _ = fmt.Fprint(w, `{"id":"completed","status":"completed","usage":{"input_tokens":1000,"output_tokens":1,"total_tokens":1001}}`)
			}))
			t.Cleanup(upstream.Close)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("name = ?", "responses-ws-upstream").Update("base_url", upstream.URL).Error)
			request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hello"}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			select {
			case <-fixture.httpDone:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP request did not finish")
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int64(tc.attempts), attempts.Load())
			if !tc.success {
				assert.Equal(t, tc.firstStatus, response.StatusCode)
			} else {
				assert.Equal(t, http.StatusOK, response.StatusCode)
			}
			if tc.ignored {
				keys, err := common.RDB.Keys(context.Background(), "perf:ws-billing:*").Result()
				require.NoError(t, err)
				assert.Empty(t, keys)
				return
			}
			requests, successes := waitPerfCounters(t, 1)
			assert.Equal(t, int64(1), requests)
			if tc.success {
				assert.Equal(t, int64(1), successes)
			} else {
				assert.Zero(t, successes)
			}
		})
	}
}

func TestResponsesWebSocketLocalErrorsKeepStreamIdentity(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {
		t.Error("local rejection reached upstream")
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"invalid","stream_id":"planner","model":"ws-billing","max_output_tokens":18446744073709551615}`)))
	invalid := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, float64(http.StatusBadRequest), invalid["status"])
	assert.Equal(t, "planner", invalid["stream_id"])
	assert.Equal(t, "invalid", invalid["event_id"])
	fixture.token.Status = common.TokenStatusDisabled
	require.NoError(t, fixture.token.SelectUpdate())
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"revoked","stream_id":"planner","model":"ws-billing","input":"hi"}`)))
	revoked := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, float64(http.StatusUnauthorized), revoked["status"])
	assert.Equal(t, "planner", revoked["stream_id"])
	assert.Equal(t, "revoked", revoked["event_id"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, nil)
}

func TestResponsesWebSocketAmbiguousControlErrorClosesAndSettlesOnce(t *testing.T) {
	const terminal = `{"type":"error","status":500,"error":{"type":"server_error","code":"server_error","message":"Internal server error"}}`
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Contains(t, string(cancel), `"type":"response.cancel"`)
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"active"}`)))
	body, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
	require.NoError(t, err)
	assert.JSONEq(t, terminal, string(body))
	_, _, err = fixture.client.ReadMessage()
	// A dropped socket would read as a synthetic 1006; 1011 is the gateway's frame.
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr, "an unattributed error must close the connection instead of remaining busy")
	assert.Equal(t, websocket.CloseInternalServerErr, closeErr.Code)
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000})
}

// Every way a session ends sends the client a close frame whose code says
// why. A dropped socket would read as a synthetic 1006 close error, so a
// received 1000, 1001, 1008 or 1011 proves the gateway sent the frame.
func TestResponsesWebSocketCloseCodes(t *testing.T) {
	require.NoError(t, i18n.Init())
	const (
		created   = `{"type":"response.created","response":{"id":"active","status":"in_progress"}}`
		delta     = `{"type":"response.output_text.delta","delta":"hello"}`
		completed = `{"type":"response.completed","response":{"id":"active","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`
	)
	for _, tc := range []struct {
		name   string
		events []string // upstream events after the create, all relayed
		// After the events, and after the client read them with waitClient,
		// upstream sends upstreamClose or drops the socket. Otherwise it reads
		// until the gateway closes the connection.
		upstreamClose    int
		drop, waitClient bool
		timeout          bool
		end              func(*testing.T, *responsesWSBillingTest)
		code             int
		reason           string
		upstreamCode     int // the close code upstream receives, when checked
	}{
		{name: "upstream closes after completion", events: []string{completed}, upstreamClose: websocket.CloseNormalClosure, code: websocket.CloseNormalClosure, reason: "upstream connection closed"},
		{name: "idle upstream goes away", events: []string{completed}, waitClient: true, upstreamClose: websocket.CloseGoingAway, code: websocket.CloseGoingAway, reason: "upstream connection closed"},
		{name: "idle upstream drops", events: []string{completed}, waitClient: true, drop: true, code: websocket.CloseInternalServerErr, reason: "upstream connection closed"},
		// A normal upstream close still truncates the active response.
		{name: "upstream closes mid-response", events: []string{created, delta}, upstreamClose: websocket.CloseNormalClosure, code: websocket.CloseInternalServerErr, reason: "upstream connection closed"},
		{name: "upstream drops mid-response", events: []string{created, delta}, drop: true, code: websocket.CloseInternalServerErr, reason: "upstream connection closed"},
		{name: "upstream response times out", events: []string{created}, timeout: true, code: websocket.CloseInternalServerErr, reason: "upstream response timed out", upstreamCode: websocket.CloseInternalServerErr},
		{name: "client closes", events: []string{completed}, end: func(t *testing.T, fixture *responsesWSBillingTest) {
			require.NoError(t, fixture.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
		}, code: websocket.CloseNormalClosure, upstreamCode: websocket.CloseNormalClosure},
		{name: "channel disabled", events: []string{completed}, end: func(t *testing.T, fixture *responsesWSBillingTest) {
			require.Equal(t, 1, service.CloseActiveWebSocketsForChannel(fixture.channel.Id, service.ChannelDisabledCloseReason))
		}, code: websocket.ClosePolicyViolation, reason: service.ChannelDisabledCloseReason, upstreamCode: websocket.ClosePolicyViolation},
		{name: "token revoked", events: []string{completed}, end: func(t *testing.T, fixture *responsesWSBillingTest) {
			fixture.token.Status = common.TokenStatusDisabled
			require.NoError(t, fixture.token.SelectUpdate())
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"again"}`)))
			assert.Equal(t, float64(http.StatusUnauthorized), readResponsesWSTestEvent(t, fixture.client)["status"])
		}, code: websocket.ClosePolicyViolation, upstreamCode: websocket.ClosePolicyViolation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientRead := make(chan struct{})
			upstreamCodes := make(chan int, 2)
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				defaultClose := ws.CloseHandler()
				ws.SetCloseHandler(func(code int, text string) error {
					upstreamCodes <- code
					return defaultClose(code, text)
				})
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				for _, event := range tc.events {
					if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
						return
					}
				}
				if tc.waitClient {
					<-clientRead
				}
				if tc.drop {
					return
				}
				if tc.upstreamClose != 0 && !assert.NoError(t, ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(tc.upstreamClose, "upstream shutdown"), time.Now().Add(time.Second))) {
					return
				}
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			})
			if tc.timeout {
				constant.StreamingTimeout = 1
			}
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			for _, event := range tc.events {
				actual, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
				require.NoError(t, err)
				assert.JSONEq(t, event, string(actual))
			}
			close(clientRead)
			if tc.end != nil {
				tc.end(t, fixture)
			}
			require.NoError(t, fixture.client.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, _, err := fixture.client.ReadMessage()
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			assert.Equal(t, tc.code, closeErr.Code)
			assert.Equal(t, tc.reason, closeErr.Text)
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000})
			if tc.upstreamCode != 0 {
				require.Len(t, upstreamCodes, 1)
				assert.Equal(t, tc.upstreamCode, <-upstreamCodes)
			}
		})
	}
}

// A control event queued while another is still outstanding is rejected
// locally instead of reaching upstream twice.
func TestResponsesWebSocketRejectsSecondControlWhileOnePending(t *testing.T) {
	receivedCancel := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.Contains(t, string(cancel), `"event_id":"first-cancel"`) {
			return
		}
		receivedCancel <- struct{}{}
		<-release
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"active","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"first-cancel","response_id":"active"}`)))
	select {
	case <-receivedCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive the first cancel")
	}
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"second-cancel","response_id":"active"}`)))
	rejected := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", rejected["type"])
	assert.Equal(t, float64(http.StatusBadRequest), rejected["status"])
	assert.Equal(t, "second-cancel", rejected["event_id"])
	rejectedError, _ := rejected["error"].(map[string]any)
	assert.Contains(t, rejectedError["message"], "already pending")
	release <- struct{}{}
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000})
}

// A control error resolves the outstanding control, so a corrected cancel is
// forwarded instead of being rejected as still pending.
func TestResponsesWebSocketAcceptsCancelAfterControlError(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.JSONEq(t, `{"type":"response.cancel","response_id":"wrong"}`, string(cancel)) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"response_not_found","message":"No such response"}}`))) {
			return
		}
		_, cancel, err = ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.JSONEq(t, `{"type":"response.cancel","response_id":"active"}`, string(cancel)) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancelled","response":{"id":"active","status":"cancelled","usage":{"input_tokens":1000,"output_tokens":0,"total_tokens":1000}}}`))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"wrong"}`)))
	cancelError := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", cancelError["type"])
	assert.Equal(t, float64(http.StatusBadRequest), cancelError["status"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"active"}`)))
	assert.Equal(t, "response.cancelled", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000})
}

// A late error that names the previous response must not end the request
// that follows it on the same connection, whether that response completed or
// failed.
func TestResponsesWebSocketIgnoresLateErrorForPreviousResponse(t *testing.T) {
	for _, tc := range []struct {
		name, firstTerminal, firstTerminalType string
	}{
		{name: "after completed", firstTerminalType: "response.completed", firstTerminal: `{"type":"response.completed","response":{"id":"first","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`},
		{name: "after error", firstTerminalType: "error", firstTerminal: `{"type":"error","response_id":"first","status":500,"error":{"type":"server_error","code":"server_error","message":"Internal server error"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				for _, turn := range [][]string{
					{`{"type":"response.created","response":{"id":"first","status":"in_progress"}}`, tc.firstTerminal},
					{
						`{"type":"error","response_id":"first","status":500,"error":{"type":"server_error","code":"server_error","message":"late failure"}}`,
						`{"type":"response.created","response":{"id":"second","status":"in_progress"}}`,
						`{"type":"response.completed","response":{"id":"second","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`,
					},
				} {
					if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
						return
					}
					for _, event := range turn {
						if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
							return
						}
					}
				}
				_, _, _ = ws.ReadMessage()
			})
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"first"}`)))
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			assert.Equal(t, tc.firstTerminalType, readResponsesWSTestEvent(t, fixture.client)["type"])
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"second"}`)))
			created := readResponsesWSTestEvent(t, fixture.client)
			require.Equal(t, "response.created", created["type"], "the late error must not reach the client or end the request: %v", created)
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000, 1000})
		})
	}
}

// An error that names a different response than the active one is relayed to
// the client and leaves the active generation running.
func TestResponsesWebSocketForwardsErrorForOtherResponseWithoutEndingRequest(t *testing.T) {
	const strayError = `{"type":"error","response_id":"other","status":500,"error":{"type":"server_error","code":"server_error","message":"unrelated failure"}}`
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`,
			strayError,
			`{"type":"response.completed","response":{"id":"active","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`,
		} {
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
				return
			}
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	stray, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
	require.NoError(t, err)
	assert.JSONEq(t, strayError, string(stray))
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000})
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
	require.Len(t, logs, 1)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
	stream, _ := other["stream_status"].(map[string]any)
	assert.Equal(t, "ok", stream["status"])
	assert.Equal(t, "completed", stream["response_status"])
}

// Requests rejected before a RelayInfo exists reach the model health sampler
// with the HTTP classification: routing failures count against the model,
// business rejections such as a token model limit do not.
func TestResponsesWebSocketPreRoutingRejectionsFollowHealthClassification(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, tc := range []struct {
		name    string
		reject  func(*testing.T, *responsesWSBillingTest)
		status  int
		sampled bool
	}{
		{name: "no eligible channel", status: http.StatusServiceUnavailable, sampled: true, reject: func(t *testing.T, fixture *responsesWSBillingTest) {
			require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", fixture.channel.Id).Update("enabled", false).Error)
		}},
		{name: "token model limit", status: http.StatusForbidden, reject: func(t *testing.T, fixture *responsesWSBillingTest) {
			// Update through the model so the cached token is invalidated.
			fixture.token.ModelLimitsEnabled = true
			fixture.token.ModelLimits = "other-model"
			require.NoError(t, fixture.token.Update())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {
				t.Error("rejected request reached upstream")
			})
			tc.reject(t, fixture)
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			rejected := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", rejected["type"])
			assert.Equal(t, float64(tc.status), rejected["status"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, nil)
			if !tc.sampled {
				keys, err := common.RDB.Keys(context.Background(), "perf:ws-billing:*").Result()
				require.NoError(t, err)
				assert.Empty(t, keys, "a business rejection must not affect model health")
				return
			}
			requests, successes := waitPerfCounters(t, 1)
			assert.Equal(t, int64(1), requests)
			assert.Zero(t, successes)
		})
	}
}

// These helpers exercise actual gateway/upstream sockets; all writes use the
// same JSON wrapper as production and each fixture owns one upstream writer.
func sendResponsesWSTestEvent(t *testing.T, ws *websocket.Conn, event map[string]any) {
	t.Helper()
	body, err := common.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, body))
}

func responsesWSTestControl(eventType, responseID string) map[string]any {
	event := map[string]any{"type": eventType, "response_id": responseID}
	if eventType == "response.inject" {
		event["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "c1", "output": "done"}}
	} else if eventType == "response.interrupt" {
		event["mode"] = "discard_partial_items"
	}
	return event
}

func TestResponsesWebSocketSteeringSettlesEachResponse(t *testing.T) {
	for _, tc := range []struct {
		name, expression, terminal, stream string
		input                              any
		quotas                             []int
		runtimeHeaders                     bool
	}{
		{name: "tokens", expression: `tier("input", p * 2)`, terminal: "response.incomplete", quotas: []int{100, 200}, input: "Keep it short"},
		{name: "tiered-completed", expression: `len <= 150 ? tier("small", p * 2) : tier("large", p * 4)`, terminal: "response.completed", stream: "planner", quotas: []int{100, 400}, input: "Keep it short"},
		{name: "fixed-multimodal", expression: `tier("request", fixed(0.0002))`, terminal: "response.incomplete", stream: "planner", quotas: []int{100, 100}, input: []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "Use these"},
			map[string]any{"type": "input_image", "file_id": "file-image"},
			map[string]any{"type": "input_file", "file_id": "file-document"},
		}}}},
		{name: "runtime-headers", expression: `tier("request", fixed(0.0002))`, terminal: "response.completed", stream: "planner", quotas: []int{100, 100}, input: "Keep it short", runtimeHeaders: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, tc.expression, func(ws *websocket.Conn, r *http.Request) {
				if tc.runtimeHeaders {
					assert.Equal(t, "Bearer steering-credential", r.Header.Get("Authorization"))
				}
				initial := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response.create", initial["type"])
				created := map[string]any{"type": "response.created", "response": map[string]any{"id": "parent", "status": "in_progress"}}
				if tc.stream != "" {
					created["stream_id"] = tc.stream
				}
				sendResponsesWSTestEvent(t, ws, created)
				steer := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response.steer", steer["type"])
				assert.Equal(t, "parent", steer["previous_response_id"])
				assert.NotContains(t, steer, "stream_id")
				assert.Len(t, steer, 3)
				wantInput, err := common.Marshal(tc.input)
				require.NoError(t, err)
				gotInput, err := common.Marshal(steer["input"])
				require.NoError(t, err)
				assert.JSONEq(t, string(wantInput), string(gotInput))
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.accepted", "steer": map[string]any{"id": "steer-1", "previous_response_id": "parent"}})
				parent := map[string]any{"type": tc.terminal, "response": map[string]any{"id": "parent", "status": strings.TrimPrefix(tc.terminal, "response."), "incomplete_details": map[string]any{"reason": "steered"}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 0, "total_tokens": 100}}}
				if tc.stream != "" {
					parent["stream_id"] = tc.stream
				}
				sendResponsesWSTestEvent(t, ws, parent)
				child := map[string]any{"type": "response.created", "response": map[string]any{"id": "child", "previous_response_id": "parent", "status": "in_progress"}}
				if tc.stream != "" {
					child["stream_id"] = tc.stream
				}
				sendResponsesWSTestEvent(t, ws, child)
				// A duplicate parent terminal cannot settle the successor.
				sendResponsesWSTestEvent(t, ws, parent)
				completeResponsesWSTestRequest(t, ws, tc.stream, "child", 200)
				_, extra, err := ws.ReadMessage()
				assert.Error(t, err, "automatic continuation must not send an extra create: %s", extra)
			})
			if tc.runtimeHeaders {
				require.NoError(t, model.DB.Model(fixture.channel).Update("param_override", `{"operations":[{"mode":"set_header","path":"Authorization","value":"Bearer steering-credential","conditions":{"upstream_model":"gpt-4o"}}]}`).Error)
			}
			initial := map[string]any{"type": "response.create", "model": "ws-billing", "input": "Plan"}
			if tc.stream != "" {
				initial["stream_id"] = tc.stream
			}
			sendResponsesWSTestEvent(t, fixture.client, initial)
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.steer", "previous_response_id": "parent", "input": tc.input})
			for _, expected := range []string{"response.steer.accepted", tc.terminal, "response.created", "response.completed"} {
				assert.Equal(t, expected, readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, tc.quotas)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
			require.Len(t, logs, 2)
			assert.NotEqual(t, logs[0].RequestId, logs[1].RequestId)
			if tc.terminal == "response.incomplete" {
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
				assert.Equal(t, "steered", other["stream_status"].(map[string]any)["incomplete_reason"])
			}
			wantSamples := int64(2)
			if tc.terminal == "response.incomplete" {
				wantSamples = 1
			}
			requests, successes := waitPerfCounters(t, wantSamples)
			assert.Equal(t, wantSamples, requests)
			assert.Equal(t, wantSamples, successes)
		})
	}
}

func TestResponsesWebSocketSteeringQueuesAndContinuesAgain(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, _ *http.Request) {
		readResponsesWSTestEvent(t, ws)
		for index, id := range []string{"parent", "child", "grandchild"} {
			response := map[string]any{"id": id, "status": "in_progress"}
			if index > 0 {
				response["previous_response_id"] = []string{"parent", "child"}[index-1]
			}
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": response})
			if index < 2 {
				for n := range 2 {
					steer := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, id, steer["previous_response_id"])
					assert.NotContains(t, steer, "stream_id")
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.accepted", "steer": map[string]any{"id": fmt.Sprintf("%s-%d", id, n), "previous_response_id": id}})
				}
			}
			completeResponsesWSTestRequest(t, ws, "planner", id, 100)
		}
		_, _, _ = ws.ReadMessage()
	})
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "input": "Plan"})
	for index, id := range []string{"parent", "child", "grandchild"} {
		created := readResponsesWSTestEvent(t, fixture.client)
		require.Equal(t, "response.created", created["type"])
		assert.Equal(t, id, created["response"].(map[string]any)["id"])
		if index < 2 {
			for n := range 2 {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.steer", "previous_response_id": id, "input": fmt.Sprintf("Update %d", n)})
				assert.Equal(t, "response.steer.accepted", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
		}
		assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{100, 100, 100})
}

func TestResponsesWebSocketSteeringPendingReleasesReservation(t *testing.T) {
	for _, notification := range []bool{true, false} {
		t.Run(fmt.Sprintf("notification=%t", notification), func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `param("service_tier") == "priority" ? tier("priority", fixed(0.004)) : tier("base", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "parent", "status": "in_progress"}})
				steer := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response.steer", steer["type"])
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.accepted", "steer": map[string]any{"id": "steer-1", "previous_response_id": "parent"}})
				completeResponsesWSTestRequest(t, ws, "", "parent", 100)
				if notification {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.pending", "reason": "waiting_for_required_input", "steer": map[string]any{"id": "steer-1", "previous_response_id": "parent"}, "required_input": []any{map[string]any{"type": "function_call_output", "call_id": "call-1"}}})
				}
				next := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response.create", next["type"])
				assert.Equal(t, "priority", next["service_tier"])
				items := next["input"].([]any)
				require.Len(t, items, 1, "steering input is already held by upstream")
				assert.Equal(t, "function_call_output", items[0].(map[string]any)["type"])
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "child", "previous_response_id": "parent", "status": "in_progress"}})
				completeResponsesWSTestRequest(t, ws, "", "child", 100)
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "Plan"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.steer", "previous_response_id": "parent", "input": "Keep it short"})
			for _, expected := range []string{"response.steer.accepted", "response.completed"} {
				assert.Equal(t, expected, readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			if notification {
				assert.Equal(t, "response.steer.pending", readResponsesWSTestEvent(t, fixture.client)["type"])
				require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
				assert.Equal(t, 2000, fixture.token.RemainQuota, "unused automatic reservation must be available before pending is delivered")
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "unrelated request"})
				assert.Equal(t, float64(http.StatusBadRequest), readResponsesWSTestEvent(t, fixture.client)["status"])
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "previous_response_id": "parent", "service_tier": "priority", "input": []any{map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "done"}}})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000, 2000})
		})
	}
}

func TestResponsesWebSocketSteeringFailureAndDisconnect(t *testing.T) {
	for _, mode := range []string{"rejected", "successor-failed", "disconnect-pending", "disconnect-child", "cancel-child", "timeout-pending", "client-disconnect-pending"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "parent", "status": "in_progress"}})
				readResponsesWSTestEvent(t, ws)
				steer := map[string]any{"previous_response_id": "parent", "input": "Update"}
				if mode == "rejected" {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.failed", "steer": steer, "error": map[string]any{"code": "steering_not_supported", "message": "Unsupported"}})
					cancel := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "response.cancel", cancel["type"])
					completeResponsesWSTestRequest(t, ws, "", "parent", 100)
					_, _, _ = ws.ReadMessage()
					return
				}
				steer["id"] = "steer-1"
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.accepted", "steer": steer})
				if mode == "client-disconnect-pending" {
					_, _, _ = ws.ReadMessage()
					return
				}
				completeResponsesWSTestRequest(t, ws, "", "parent", 100)
				if mode == "disconnect-pending" {
					return
				}
				if mode == "timeout-pending" {
					_, _, _ = ws.ReadMessage()
					return
				}
				if mode == "successor-failed" {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.failed", "steer": steer, "error": map[string]any{"code": "successor_creation_failed", "message": "Could not continue"}})
					_, _, _ = ws.ReadMessage()
					return
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "child", "previous_response_id": "parent", "status": "in_progress"}})
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "child", "delta": "hello"})
				if mode == "cancel-child" {
					cancel := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "response.cancel", cancel["type"])
					assert.Equal(t, "child", cancel["response_id"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.cancelled", "response": map[string]any{"id": "child", "status": "cancelled", "usage": map[string]any{"input_tokens": 100, "output_tokens": 1, "total_tokens": 101}}})
					_, _, _ = ws.ReadMessage()
				}
			})
			if mode == "timeout-pending" {
				constant.StreamingTimeout = 1
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "Plan"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.steer", "previous_response_id": "parent", "input": "Update"})
			quotas := []int{1000}
			if mode == "rejected" {
				assert.Equal(t, "response.steer.failed", readResponsesWSTestEvent(t, fixture.client)["type"])
				require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
				assert.Equal(t, 2000, fixture.token.RemainQuota)
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "response_id": "parent"})
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			} else {
				assert.Equal(t, "response.steer.accepted", readResponsesWSTestEvent(t, fixture.client)["type"])
				if mode == "client-disconnect-pending" {
					fixture.closeAndWait(t)
					assertResponsesWSAccounting(t, fixture, quotas)
					return
				}
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				if mode == "successor-failed" {
					assert.Equal(t, "response.steer.failed", readResponsesWSTestEvent(t, fixture.client)["type"])
				} else if mode != "disconnect-pending" && mode != "timeout-pending" {
					assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
					assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
					quotas = append(quotas, 1000)
					if mode == "cancel-child" {
						sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "response_id": "child"})
						assert.Equal(t, "response.cancelled", readResponsesWSTestEvent(t, fixture.client)["type"])
					}
				}
			}
			if strings.HasPrefix(mode, "disconnect") || mode == "timeout-pending" {
				_, _, err := fixture.client.ReadMessage()
				require.Error(t, err)
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, quotas)
		})
	}
}

func TestResponsesWebSocketSteeringAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"invalid-input", 400}, {"wrong-parent", 400}, {"extra-fields", 400},
		{"quota", 403}, {"revoked", 401}, {"expired", 401}, {"model-access", 403}, {"rate-limit", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finish := make(chan struct{})
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.004))`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "parent", "status": "in_progress"}})
				<-finish
				completeResponsesWSTestRequest(t, ws, "", "parent", 100)
				_, unexpected, err := ws.ReadMessage()
				assert.Error(t, err, "rejected steer reached upstream: %s", unexpected)
			})
			if tc.name != "quota" {
				require.NoError(t, model.InvalidateUserTokensCache(fixture.user.Id))
				require.NoError(t, model.DB.Model(fixture.token).Update("remain_quota", 10000).Error)
			}
			if tc.name == "rate-limit" {
				setting.ModelRequestRateLimitEnabled = true
				setting.ModelRequestRateLimitDurationMinutes = 1
				setting.ModelRequestRateLimitCount = 1
				setting.ModelRequestRateLimitSuccessCount = 0
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "Plan"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			steer := map[string]any{"type": "response.steer", "previous_response_id": "parent", "input": "Update"}
			require.NoError(t, model.InvalidateUserTokensCache(fixture.user.Id))
			switch tc.name {
			case "invalid-input":
				steer["input"] = []any{map[string]any{"role": "system", "content": "Update"}}
			case "wrong-parent":
				steer["previous_response_id"] = "another-connection"
			case "extra-fields":
				steer["max_output_tokens"] = uint64(18446744073709551615)
			case "revoked":
				require.NoError(t, model.DB.Model(fixture.token).Update("status", common.TokenStatusDisabled).Error)
			case "expired":
				require.NoError(t, model.DB.Model(fixture.token).Update("expired_time", time.Now().Unix()-1).Error)
			case "model-access":
				require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "other"}).Error)
			}
			sendResponsesWSTestEvent(t, fixture.client, steer)
			event := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", event["type"])
			assert.Equal(t, float64(tc.status), event["status"])
			close(finish)
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, 2000, logs[0].Quota)
			require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
			assert.Equal(t, 2000, fixture.token.UsedQuota)
		})
	}
}

func TestResponsesWebSocketSteeringAdjustsFrozenReservation(t *testing.T) {
	for _, rejectSecond := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject-second=%t", rejectSecond), func(t *testing.T) {
			settle := make(chan struct{})
			fixture := newResponsesWSBillingTest(t, `param("input.#") == 2 ? tier("two", fixed(0.004)) : tier("one", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "parent", "status": "in_progress"}})
				for n := range 2 {
					steer := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "response.steer", steer["type"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.accepted", "steer": map[string]any{"id": fmt.Sprintf("steer-%d", n), "previous_response_id": "parent"}})
				}
				<-settle
				if rejectSecond {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.failed", "steer": map[string]any{"id": "steer-1", "previous_response_id": "parent", "input": "Update 1"}, "error": map[string]any{"code": "invalid_input", "message": "Rejected"}})
				}
				completeResponsesWSTestRequest(t, ws, "", "parent", 100)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "child", "previous_response_id": "parent", "status": "in_progress"}})
				completeResponsesWSTestRequest(t, ws, "", "child", 100)
				_, _, _ = ws.ReadMessage()
			})
			t.Cleanup(func() {
				select {
				case <-settle:
				default:
					close(settle)
				}
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "Plan"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			for n := range 2 {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.steer", "previous_response_id": "parent", "input": fmt.Sprintf("Update %d", n)})
				require.Equal(t, "response.steer.accepted", readResponsesWSTestEvent(t, fixture.client)["type"])
				if n == 0 {
					require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_expr": `{"ws-billing":"tier(\"changed\", fixed(0.01))"}`}))
				}
			}
			require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
			assert.Zero(t, fixture.token.RemainQuota, "the second input must increase the existing reservation")
			close(settle)
			quotas := []int{1000, 2000}
			if rejectSecond {
				require.Equal(t, "response.steer.failed", readResponsesWSTestEvent(t, fixture.client)["type"])
				quotas[1] = 1000
			}
			for _, eventType := range []string{"response.completed", "response.created", "response.completed"} {
				require.Equal(t, eventType, readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, quotas)
		})
	}
}

func completeResponsesWSTestRequest(t *testing.T, ws *websocket.Conn, streamID, responseID string, inputTokens int) {
	t.Helper()
	event := map[string]any{"type": "response.completed", "response": map[string]any{
		"id": responseID, "status": "completed", "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0, "total_tokens": inputTokens},
	}}
	if streamID != "" {
		event["stream_id"] = streamID
	}
	sendResponsesWSTestEvent(t, ws, event)
}

func TestResponsesWebSocketMultiplexesAndQueues(t *testing.T) {
	for _, lane := range []string{"planner", ""} {
		t.Run("lane="+lane, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				first := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "first", first["input"])
				// The busy lane's second request must not block another lane.
				other := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "research", other["stream_id"])
				assert.Equal(t, "other", other["input"])
				for _, stream := range []string{lane, "research"} {
					event := map[string]any{"type": "response.created", "response": map[string]any{"id": "first-" + stream, "status": "in_progress"}}
					if stream != "" {
						event["stream_id"] = stream
					}
					sendResponsesWSTestEvent(t, ws, event)
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "stream_id": "research", "delta": "research output"})
				completeResponsesWSTestRequest(t, ws, "research", "first-research", 2000)
				completeResponsesWSTestRequest(t, ws, lane, "first-"+lane, 500)
				second := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "second", second["input"])
				assert.Equal(t, "first-"+lane, second["previous_response_id"])
				// Neither another lane's terminal nor this lane's previous terminal
				// may complete or bill the next request a second time.
				completeResponsesWSTestRequest(t, ws, "research", "first-research", 2000)
				completeResponsesWSTestRequest(t, ws, lane, "first-"+lane, 500)
				completeResponsesWSTestRequest(t, ws, lane, "second", 250)
				_, _, _ = ws.ReadMessage()
			})
			for _, input := range []string{"first", "second", "other"} {
				event := map[string]any{"type": "response.create", "model": "ws-billing", "input": input}
				if lane != "" {
					event["stream_id"] = lane
				}
				if input == "second" {
					event["previous_response_id"] = "first-" + lane
				}
				if input == "other" {
					event["stream_id"] = "research"
				}
				sendResponsesWSTestEvent(t, fixture.client, event)
			}
			completed := make(map[string]bool)
			for len(completed) < 3 {
				event := readResponsesWSTestEvent(t, fixture.client)
				require.NotEqual(t, "error", event["type"], "%v", event)
				if event["type"] != "response.completed" {
					continue
				}
				id := event["response"].(map[string]any)["id"].(string)
				assert.False(t, completed[id], "duplicate terminal %s", id)
				completed[id] = true
				if id == "first-research" {
					assert.Equal(t, "research", event["stream_id"])
				} else if lane == "" {
					assert.NotContains(t, event, "stream_id")
				} else {
					assert.Equal(t, lane, event["stream_id"])
				}
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			quotas := make(map[string]int)
			for _, log := range logs {
				quotas[log.RequestId] = log.Quota
			}
			assert.Equal(t, map[string]int{"responses-ws-billing-ws-0": 500, "responses-ws-billing-ws-1": 250, "responses-ws-billing-ws-2": 2000}, quotas)
			require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
			require.NoError(t, model.DB.First(fixture.user, fixture.user.Id).Error)
			assert.Equal(t, 250, fixture.token.RemainQuota)
			assert.Equal(t, 2750, fixture.token.UsedQuota)
			assert.Equal(t, 97250, fixture.user.Quota)
		})
	}
}

func TestResponsesWebSocketForkSharesUpstreamConnection(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		parent := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "planner", parent["stream_id"])
		completeResponsesWSTestRequest(t, ws, "planner", "parent", 100)
		fork := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "critic", fork["stream_id"])
		assert.Equal(t, "parent", fork["previous_response_id"])
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.in_progress", "stream_id": "critic", "response": map[string]any{"id": "fork", "status": "in_progress"}})
		continued := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "planner", continued["stream_id"])
		assert.Equal(t, "parent", continued["previous_response_id"])
		completeResponsesWSTestRequest(t, ws, "planner", "continued", 100)
		completeResponsesWSTestRequest(t, ws, "critic", "fork", 100)
		_, _, _ = ws.ReadMessage()
	})
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "input": "parent", "store": false})
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "critic", "previous_response_id": "parent", "input": "fork", "store": false})
	assert.Equal(t, "response.in_progress", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "previous_response_id": "parent", "input": "continue", "store": false})
	for range 2 {
		assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	fixture.closeAndWait(t)
	assert.Equal(t, int32(1), fixture.connections.Load())
	assertResponsesWSAccounting(t, fixture, []int{100, 100, 100})
}

func TestResponsesWebSocketConnectionLimits(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		active := make(map[string]bool)
		for range 16 {
			event := readResponsesWSTestEvent(t, ws)
			stream := event["stream_id"].(string)
			active[stream] = true
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": stream, "response": map[string]any{"id": "resp-" + stream, "status": "in_progress"}})
		}
		// A control sent after all 17 creates must reach upstream before the
		// seventeenth create, proving admission stops at 16 active responses.
		control := readResponsesWSTestEvent(t, ws)
		require.Equal(t, "response.cancel", control["type"])
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "stream_id": "lane-0", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "response_not_active", "message": "Control rejected"}})
		completeResponsesWSTestRequest(t, ws, "lane-0", "resp-lane-0", 1)
		delete(active, "lane-0")
		seventeenth := readResponsesWSTestEvent(t, ws)
		require.Equal(t, "lane-16", seventeenth["stream_id"])
		active["lane-16"] = true
		for stream := range active {
			completeResponsesWSTestRequest(t, ws, stream, "resp-"+stream, 1)
		}
		for {
			_, body, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var event map[string]any
			require.NoError(t, common.Unmarshal(body, &event))
			stream, _ := event["stream_id"].(string)
			completeResponsesWSTestRequest(t, ws, stream, "resp-"+event["input"].(string), 1)
		}
	})
	for index := range 17 {
		stream := fmt.Sprintf("lane-%d", index)
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": stream, "input": stream})
	}
	for range 16 {
		assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "event_id": "probe", "response_id": "resp-lane-0"})
	completed := 0
	for completed < 17 {
		event := readResponsesWSTestEvent(t, fixture.client)
		if event["type"] == "error" {
			assert.Equal(t, "lane-0", event["stream_id"])
		} else {
			require.Equal(t, "response.completed", event["type"])
			completed++
		}
	}
	for index := 17; index < 32; index++ {
		stream := fmt.Sprintf("lane-%d", index)
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": stream, "input": stream})
		assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "lane-32", "input": "too-many", "event_id": "limit"})
	limited := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, float64(400), limited["status"])
	assert.Equal(t, "lane-32", limited["stream_id"])
	assert.Equal(t, "limit", limited["event_id"])
	assert.Equal(t, "websocket_stream_limit_reached", limited["error"].(map[string]any)["code"])
	assert.Equal(t, "stream_id", limited["error"].(map[string]any)["param"])
	for _, stream := range []string{"", "lane-0"} {
		event := map[string]any{"type": "response.create", "model": "ws-billing", "input": "reuse-" + stream}
		if stream != "" {
			event["stream_id"] = stream
		}
		sendResponsesWSTestEvent(t, fixture.client, event)
		assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	for _, invalid := range []any{"", nil, 1, "bad stream", strings.Repeat("a", 257)} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": invalid})
		event := readResponsesWSTestEvent(t, fixture.client)
		assert.Equal(t, float64(400), event["status"])
		assert.NotContains(t, event, "stream_id")
		assert.Equal(t, "invalid_stream_id", event["error"].(map[string]any)["code"])
		assert.Equal(t, "stream_id", event["error"].(map[string]any)["param"])
	}
	fixture.closeAndWait(t)
	assert.Equal(t, int32(1), fixture.connections.Load())
	quotas := make([]int, 34)
	for index := range quotas {
		quotas[index] = 1
	}
	assertResponsesWSAccounting(t, fixture, quotas)
}

func TestResponsesWebSocketRevalidatesQueuedCredentials(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				<-release
				completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				_, _, err := ws.ReadMessage()
				assert.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation), "queued request must never reach upstream: %v", err)
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "input": "first"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "input": "queued"})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "unsupported", "event_id": "queued-ack"})
			assert.Equal(t, "queued-ack", readResponsesWSTestEvent(t, fixture.client)["event_id"])
			if expired {
				require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
				fixture.token.ExpiredTime = time.Now().Unix() - 1
				require.NoError(t, fixture.token.Update())
			} else {
				fixture.token.Status = common.TokenStatusDisabled
				require.NoError(t, fixture.token.SelectUpdate())
			}
			release <- struct{}{}
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			rejection := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, float64(http.StatusUnauthorized), rejection["status"])
			assert.Equal(t, "planner", rejection["stream_id"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{100})
		})
	}
}

func TestResponsesWebSocketBoundsQueuedRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		queued  int
		padding int
		close   bool
	}{
		{name: "count", queued: 128},
		{name: "bytes", queued: 1, padding: 600 << 10},
		{name: "close-while-full", queued: 128, close: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousMax := constant.MaxRequestBodyMB
			constant.MaxRequestBodyMB = 1
			t.Cleanup(func() { constant.MaxRequestBodyMB = previousMax })
			release := make(chan struct{})
			defer close(release)
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "first", "status": "in_progress"}})
				<-release
				if tc.close {
					_, _, _ = ws.ReadMessage()
					return
				}
				completeResponsesWSTestRequest(t, ws, "", "first", 1)
				for index := range tc.queued + 1 {
					event := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, fmt.Sprintf("queued-%d", index), event["input"])
					completeResponsesWSTestRequest(t, ws, "", fmt.Sprintf("response-%d", index), 1)
				}
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			for index := range tc.queued {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": fmt.Sprintf("queued-%d", index), "metadata": map[string]any{"padding": strings.Repeat("x", tc.padding)}})
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "unsupported", "event_id": "queue-full"})
			assert.Equal(t, "queue-full", readResponsesWSTestEvent(t, fixture.client)["event_id"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": fmt.Sprintf("queued-%d", tc.queued), "metadata": map[string]any{"padding": strings.Repeat("x", tc.padding)}})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "unsupported", "event_id": "after-backpressure"})
			if tc.close {
				require.Equal(t, 1, service.CloseActiveWebSocketsForChannel(fixture.channel.Id, service.ChannelDisabledCloseReason))
				release <- struct{}{}
				fixture.closeAndWait(t)
				assertResponsesWSAccounting(t, fixture, []int{0})
				return
			}
			release <- struct{}{}
			// Capacity is released only after the active terminal is written.
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			completed, resumed := 1, false
			for completed < tc.queued+2 || !resumed {
				event := readResponsesWSTestEvent(t, fixture.client)
				if event["type"] == "response.completed" {
					completed++
				} else {
					require.Equal(t, "after-backpressure", event["event_id"])
					resumed = true
				}
			}
			fixture.closeAndWait(t)
			quotas := make([]int, tc.queued+2)
			for index := range quotas {
				quotas[index] = 1
			}
			assertResponsesWSAccounting(t, fixture, quotas)
		})
	}
}

func TestResponsesWebSocketIsolatesLaneFailures(t *testing.T) {
	fixture := newResponsesWSBillingTestWithHeaders(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		for range 2 {
			readResponsesWSTestEvent(t, ws)
		}
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "b", "response": map[string]any{"id": "response-b", "status": "in_progress"}})
		control := readResponsesWSTestEvent(t, ws)
		require.Equal(t, "response.cancel", control["type"])
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "stream_id": "b", "response_id": "missing", "event_id": "cancel-b", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "response_not_found", "message": "Missing response"}})
		// A request-scoped upstream 403 must not revoke the gateway session.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "stream_id": "a", "status": 403, "error": map[string]any{"type": "invalid_request_error", "code": "invalid_input", "message": "Rejected input"}})
		next := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "a", next["stream_id"])
		assert.Equal(t, "next", next["input"])
		completeResponsesWSTestRequest(t, ws, "a", "next-a", 100)
		completeResponsesWSTestRequest(t, ws, "b", "response-b", 200)
		_, _, _ = ws.ReadMessage()
	}, nil, func(http.ResponseWriter, *http.Request) (http.Header, bool) {
		return http.Header{"X-Codex-Turn-State": {"turn-state"}}, true
	})
	updates := make(chan struct{}, 16)
	require.NoError(t, model.DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("responses-ws-multiplex-refund", func(tx *gorm.DB) {
		if tx.Error == nil && tx.Statement.Table == "tokens" {
			select {
			case updates <- struct{}{}:
			default:
			}
		}
	}))
	for _, stream := range []string{"a", "b"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": stream, "input": "first", "event_id": "create-" + stream})
	}
	assert.Equal(t, "codex.response.metadata", readResponsesWSTestEvent(t, fixture.client)["type"])
	assert.Equal(t, map[string]any{"type": "response.metadata", "stream_id": "b", "headers": map[string]any{"x-codex-turn-state": "turn-state"}}, readResponsesWSTestEvent(t, fixture.client), "older gateways must attribute turn state to the accepted lane")
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "a", "input": "next"})
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "stream_id": "b", "response_id": "missing", "event_id": "cancel-b"})
	errorsByStream := make(map[string]map[string]any)
	completed := make(map[string]bool)
	for len(errorsByStream) < 2 || len(completed) < 2 {
		event := readResponsesWSTestEvent(t, fixture.client)
		stream := event["stream_id"].(string)
		if event["type"] == "error" {
			errorsByStream[stream] = event
		} else {
			require.Equal(t, "response.completed", event["type"])
			completed[stream] = true
		}
	}
	assert.Equal(t, float64(403), errorsByStream["a"]["status"])
	assert.Equal(t, "create-a", errorsByStream["a"]["event_id"])
	assert.Equal(t, "cancel-b", errorsByStream["b"]["event_id"])
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
		if fixture.token.RemainQuota == 1000 {
			break
		}
		select {
		case <-updates:
		case <-deadline.C:
			t.Fatal("rejected lane reservation was not refunded")
		}
	}
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000, 1000})
}

func TestResponsesWebSocketConnectionFailureSettlesAllLanes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		ambiguous, defaultLane bool
	}{
		{name: "upstream-disconnect"},
		{name: "ambiguous-named", ambiguous: true},
		{name: "ambiguous-with-default", ambiguous: true, defaultLane: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streams := []string{"a", "b"}
			if tc.defaultLane {
				streams[0] = ""
			}
			release := make(chan struct{})
			defer close(release)
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				for range 2 {
					readResponsesWSTestEvent(t, ws)
				}
				for _, stream := range streams {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": stream, "response": map[string]any{"id": "response-" + stream, "status": "in_progress"}})
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "stream_id": stream, "delta": "delivered output"})
				}
				<-release
				if tc.ambiguous {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "status": 500, "error": map[string]any{"type": "server_error", "message": "Unattributed failure"}})
					_, _, _ = ws.ReadMessage()
				}
			})
			for _, stream := range streams {
				event := map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"}
				if stream != "" {
					event["stream_id"] = stream
				}
				sendResponsesWSTestEvent(t, fixture.client, event)
			}
			for range 4 {
				require.NotEqual(t, "error", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			for _, stream := range streams {
				event := map[string]any{"type": "response.create", "model": "ws-billing", "input": "queued"}
				if stream != "" {
					event["stream_id"] = stream
				}
				sendResponsesWSTestEvent(t, fixture.client, event)
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "unsupported", "event_id": "queued"})
			assert.Equal(t, "queued", readResponsesWSTestEvent(t, fixture.client)["event_id"])
			release <- struct{}{}
			if tc.ambiguous {
				assert.Equal(t, "error", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			_, _, err := fixture.client.ReadMessage()
			assert.True(t, websocket.IsCloseError(err, websocket.CloseInternalServerErr), "unexpected connection result: %v", err)
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{1000, 1000})
		})
	}
}

func TestResponsesWebSocketQueuedRequestWaitsForRefund(t *testing.T) {
	received := make(chan struct{}, 1)
	reject := make(chan struct{})
	defer close(reject)
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.006))`, func(ws *websocket.Conn, _ *http.Request) {
		readResponsesWSTestEvent(t, ws)
		received <- struct{}{}
		<-reject
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "stream_id": "planner", "status": 400, "error": map[string]any{"type": "invalid_request_error", "message": "Rejected input"}})
		next := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "next", next["input"])
		completeResponsesWSTestRequest(t, ws, "planner", "next", 1)
		_, _, _ = ws.ReadMessage()
	})
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "input": "rejected"})
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive the reserved request")
	}
	require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
	require.Zero(t, fixture.token.RemainQuota, "the first request reserves the entire token quota")
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": "planner", "input": "next"})
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "unsupported", "event_id": "queued"})
	assert.Equal(t, "queued", readResponsesWSTestEvent(t, fixture.client)["event_id"])
	reject <- struct{}{}
	errorEvent := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, float64(400), errorEvent["status"])
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{3000})
}

func TestResponsesWebSocketInterruptLifecycle(t *testing.T) {
	const interrupt = `{"type":"response.interrupt","response_id":"first","mode":"discard_partial_items","vendor":{"integer":9007199254740993,"zero":0.0,"enabled":false}}`
	for _, tc := range []struct {
		name, lane, expression, errorEventID       string
		terminalFirst, late, nextBeforeAck, reject bool
		quotas                                     []int
	}{
		{name: "acknowledgement first", expression: `tier("request", fixed(0.0002))`, quotas: []int{100, 100}},
		{name: "terminal first", lane: "planner", terminalFirst: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "next response before acknowledgement", lane: "planner", terminalFirst: true, nextBeforeAck: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "interrupt after terminal", late: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "late interrupt and acknowledgement", lane: "planner", late: true, nextBeforeAck: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "generic rejection after completion", late: true, reject: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "generic rejection after interrupted terminal", terminalFirst: true, reject: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "generic rejection during next response", lane: "planner", late: true, nextBeforeAck: true, reject: true, expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
		{name: "generic rejection with upstream event id", late: true, reject: true, errorEventID: "upstream-error", expression: `tier("input", p * 2)`, quotas: []int{100, 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nextSeen := make(chan struct{})
			releaseNext := sync.OnceFunc(func() { close(nextSeen) })
			defer releaseNext()
			created := map[string]any{"type": "response.created", "response": map[string]any{"id": "first", "status": "in_progress"}}
			delta := map[string]any{"type": "response.output_text.delta", "response_id": "first", "delta": "partial output"}
			status := "incomplete"
			if tc.late {
				status = "completed"
			}
			response := map[string]any{
				"id": "first", "status": status,
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110},
			}
			if !tc.late {
				response["incomplete_details"] = map[string]any{"reason": "interrupted"}
			}
			terminal := map[string]any{"type": "response." + status, "response": response}
			ack := map[string]any{"type": "response.interrupt.accepted", "response_id": "first", "vendor": "preserved"}
			if tc.reject {
				ack = map[string]any{"type": "error", "response_id": "first", "status": float64(http.StatusBadRequest), "error": map[string]any{"type": "invalid_request_error", "code": "invalid_mode", "message": "Unsupported interruption mode"}, "vendor": "preserved"}
				if tc.errorEventID != "" {
					ack["event_id"] = tc.errorEventID
				}
			}
			nextCreated := map[string]any{"type": "response.created", "response": map[string]any{"id": "second", "status": "in_progress"}}
			initial := map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"}
			next := map[string]any{"type": "response.create", "model": "ws-billing", "input": "next", "previous_response_id": "first"}
			if tc.lane != "" {
				for _, event := range []map[string]any{created, delta, terminal, nextCreated, initial, next} {
					event["stream_id"] = tc.lane
				}
			}
			fixture := newResponsesWSBillingTest(t, tc.expression, func(ws *websocket.Conn, _ *http.Request) {
				require.Equal(t, "response.create", readResponsesWSTestEvent(t, ws)["type"])
				sendResponsesWSTestEvent(t, ws, created)
				sendResponsesWSTestEvent(t, ws, delta)
				if tc.late {
					sendResponsesWSTestEvent(t, ws, terminal)
				}
				_, body, err := ws.ReadMessage()
				require.NoError(t, err)
				assert.Equal(t, interrupt, string(body), "interrupt payload must be forwarded byte for byte")
				if !tc.late && !tc.terminalFirst {
					sendResponsesWSTestEvent(t, ws, ack)
				}
				if !tc.late {
					sendResponsesWSTestEvent(t, ws, terminal)
				}
				if !tc.nextBeforeAck && (tc.late || tc.terminalFirst) {
					sendResponsesWSTestEvent(t, ws, ack)
				}
				continuation := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response.create", continuation["type"])
				assert.Equal(t, "first", continuation["previous_response_id"])
				sendResponsesWSTestEvent(t, ws, nextCreated)
				if tc.nextBeforeAck {
					<-nextSeen
					sendResponsesWSTestEvent(t, ws, ack)
				}
				if tc.reject {
					_, retry, err := ws.ReadMessage()
					require.NoError(t, err)
					assert.Equal(t, interrupt, string(retry), "a rejected interrupt must be retryable")
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.interrupt.accepted", "response_id": "first"})
				}
				// A repeated terminal must still be suppressed after the control reply.
				sendResponsesWSTestEvent(t, ws, terminal)
				completeResponsesWSTestRequest(t, ws, tc.lane, "second", 200)
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, initial)
			assert.Equal(t, created, readResponsesWSTestEvent(t, fixture.client))
			assert.Equal(t, delta, readResponsesWSTestEvent(t, fixture.client))
			if tc.late {
				assert.Equal(t, terminal["type"], readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(interrupt)))
			if !tc.late && !tc.terminalFirst {
				assert.Equal(t, ack, readResponsesWSTestEvent(t, fixture.client))
			}
			if !tc.late {
				assert.Equal(t, terminal["type"], readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			if !tc.nextBeforeAck && (tc.late || tc.terminalFirst) {
				assert.Equal(t, ack, readResponsesWSTestEvent(t, fixture.client))
			}
			sendResponsesWSTestEvent(t, fixture.client, next)
			assert.Equal(t, nextCreated, readResponsesWSTestEvent(t, fixture.client))
			releaseNext()
			if tc.nextBeforeAck {
				assert.Equal(t, ack, readResponsesWSTestEvent(t, fixture.client))
			}
			if tc.reject {
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(interrupt)))
				assert.Equal(t, "response.interrupt.accepted", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, tc.quotas)
			var log model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").First(&log).Error)
			assert.Equal(t, 100, log.PromptTokens)
			assert.Equal(t, 10, log.CompletionTokens)
			if !tc.late {
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
				assert.Equal(t, "interrupted", other["stream_status"].(map[string]any)["incomplete_reason"])
			}
			wantSamples := int64(1)
			if tc.late {
				wantSamples = 2
			}
			requests, successes := waitPerfCounters(t, wantSamples)
			assert.Equal(t, wantSamples, requests)
			assert.Equal(t, wantSamples, successes)
		})
	}
}

func TestResponsesWebSocketControlDuringSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, replyType     string
		cancelFirst, inject bool
	}{
		{name: "injected", inject: true, replyType: "response.inject.created"},
		{name: "injection failed", inject: true, replyType: "response.inject.failed"},
		{name: "injection after cancel", inject: true, cancelFirst: true, replyType: "response.inject.created"},
		{name: "accepted", replyType: "response.interrupt.accepted"},
		{name: "accepted after cancel", replyType: "response.interrupt.accepted", cancelFirst: true},
		{name: "failed", replyType: "response.interrupt.failed"},
		{name: "failed after cancel", replyType: "response.interrupt.failed", cancelFirst: true},
		{name: "generic rejection", replyType: "error"},
		{name: "generic rejection after cancel", replyType: "error", cancelFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlType := "response.interrupt"
			if tc.inject {
				controlType = "response.inject"
			}
			finish := make(chan struct{})
			releaseFinish := sync.OnceFunc(func() { close(finish) })
			defer releaseFinish()
			settling := make(chan struct{})
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			reply := `{"type":"` + tc.replyType + `","response_id":"first","vendor":{"integer":9007199254740993,"zero":0.0,"enabled":false}}`
			if tc.replyType == "response.inject.failed" {
				reply = strings.TrimSuffix(reply, "}") + `,"input":[{"type":"function_call_output","call_id":"c1","output":"done"}],"error":{"code":"response_already_completed","message":"Already completed"}}`
			} else if tc.replyType != "response.interrupt.accepted" && tc.replyType != "response.inject.created" {
				reply = strings.TrimSuffix(reply, "}") + `,"status":400,"error":{"type":"invalid_request_error","code":"invalid_mode","message":"Unsupported interruption mode"}}`
			}
			delta := map[string]any{"type": "response.output_text.delta", "stream_id": "other", "response_id": "second", "delta": "other stream continues"}
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "first", "status": "in_progress"}})
				assert.Equal(t, "other", readResponsesWSTestEvent(t, ws)["stream_id"])
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "other", "response": map[string]any{"id": "second", "status": "in_progress"}})
				if tc.cancelFirst {
					assert.Equal(t, "response.cancel", readResponsesWSTestEvent(t, ws)["type"])
				}
				<-finish
				completeResponsesWSTestRequest(t, ws, "", "first", 100)
				control := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, controlType, control["type"])
				assert.Equal(t, "first", control["response_id"])
				require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(reply)))
				sendResponsesWSTestEvent(t, ws, delta)
				<-release
				completeResponsesWSTestRequest(t, ws, "other", "second", 100)
				_, _, _ = ws.ReadMessage()
			})
			var block atomic.Bool
			require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("responses-ws-interrupt-settlement", func(tx *gorm.DB) {
				if tx.Statement.Table == "tokens" && block.CompareAndSwap(true, false) {
					close(settling)
					<-release
				}
			}))
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "other", "model": "ws-billing", "input": "second"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			if tc.cancelFirst {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "response_id": "first"})
			}
			block.Store(true)
			releaseFinish()
			select {
			case <-settling:
			case <-time.After(3 * time.Second):
				t.Fatal("response did not enter settlement")
			}
			sendResponsesWSTestEvent(t, fixture.client, responsesWSTestControl(controlType, "first"))
			assert.Equal(t, delta, readResponsesWSTestEvent(t, fixture.client), "another stream must advance before settlement is released")
			unblock()
			completed := make(map[string]bool)
			replies := 0
			for range 3 {
				_, body, err := fixture.client.ReadMessage()
				require.NoError(t, err)
				if string(body) == reply {
					assert.True(t, completed["first"], "the terminal must precede the unchanged interrupt reply")
					replies++
					continue
				}
				var event map[string]any
				require.NoError(t, common.Unmarshal(body, &event))
				require.Equal(t, "response.completed", event["type"])
				id := event["response"].(map[string]any)["id"].(string)
				assert.False(t, completed[id], "each terminal must be delivered once")
				completed[id] = true
			}
			assert.Equal(t, map[string]bool{"first": true, "second": true}, completed)
			assert.Equal(t, 1, replies)
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{100, 100})
		})
	}
}

func TestResponsesWebSocketControlBufferedReplies(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		replyCount, replyBytes     int
		overflow                   bool
		clientClose, upstreamClose bool
		inject                     bool
	}{
		{name: "mixed replies", replyCount: 4, inject: true},
		{name: "mixed count overflow", replyCount: 128, overflow: true, inject: true},
		{name: "mixed byte overflow", replyCount: 2, replyBytes: 1 << 19, overflow: true, inject: true},
		{name: "injection client disconnect", replyCount: 4, clientClose: true, inject: true},
		{name: "injection upstream close", replyCount: 4, upstreamClose: true, inject: true},
		{name: "count limit across lanes", replyCount: 128},
		{name: "count overflow across lanes", replyCount: 128, overflow: true},
		{name: "byte limit across lanes", replyCount: 2, replyBytes: 1 << 19},
		{name: "byte overflow across lanes", replyCount: 2, replyBytes: 1 << 19, overflow: true},
		{name: "client disconnect", replyCount: 4, clientClose: true},
		{name: "upstream close after replies", replyCount: 4, upstreamClose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousLimit := constant.MaxRequestBodyMB
			constant.MaxRequestBodyMB = 1
			t.Cleanup(func() { constant.MaxRequestBodyMB = previousLimit })
			finish, release, proceed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseFinish := sync.OnceFunc(func() { close(finish) })
			unblock := sync.OnceFunc(func() { close(release) })
			advance := sync.OnceFunc(func() { close(proceed) })
			defer releaseFinish()
			defer unblock()
			defer advance()
			settling := make(chan struct{})
			replies := make([]string, tc.replyCount)
			expected := make(map[string][]string)
			for i := range replies {
				id := "first"
				if i%2 != 0 {
					id = "second"
				}
				replyType := "response.interrupt.accepted"
				if tc.inject && id == "first" {
					replyType = "response.inject.created"
				}
				reply := fmt.Sprintf(`{"type":%q,"response_id":%q,"sequence":%d,"padding":""}`, replyType, id, i)
				if tc.replyBytes > 0 {
					reply = strings.TrimSuffix(reply, `"}`) + strings.Repeat("x", tc.replyBytes-len(reply)) + `"}`
				}
				replies[i] = reply
				expected[id] = append(expected[id], reply)
			}
			probeReply := map[string]any{"type": "response.interrupt.accepted", "response_id": "probe"}
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				completeResponsesWSTestRequest(t, ws, "", "probe", 100)
				for _, id := range []string{"first", "second"} {
					create := readResponsesWSTestEvent(t, ws)
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": create["stream_id"], "response": map[string]any{"id": id, "status": "in_progress"}})
				}
				<-finish
				completeResponsesWSTestRequest(t, ws, "first", "first", 100)
				select {
				case <-settling:
				case <-release:
					return
				}
				// Both terminals are routed while the first settlement holds the
				// fixture's only database connection, so both queues remain full.
				completeResponsesWSTestRequest(t, ws, "second", "second", 100)
				for _, id := range []string{"first", "second", "probe"} {
					control := readResponsesWSTestEvent(t, ws)
					controlType := "response.interrupt"
					if tc.inject && id == "first" {
						controlType = "response.inject"
					}
					assert.Equal(t, controlType, control["type"])
					assert.Equal(t, id, control["response_id"])
				}
				for _, reply := range replies {
					require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(reply)))
				}
				// A completed response has no worker to wait for. Its reply proves
				// all preceding replies were queued without blocking the reader.
				sendResponsesWSTestEvent(t, ws, probeReply)
				<-proceed
				if tc.overflow {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.interrupt.accepted", "response_id": "first"})
				}
				if tc.upstreamClose {
					require.NoError(t, ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
				}
				_, _, _ = ws.ReadMessage()
			})
			var block atomic.Bool
			require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("responses-ws-buffered-interrupt-settlement", func(tx *gorm.DB) {
				if tx.Statement.Table == "tokens" && block.CompareAndSwap(true, false) {
					close(settling)
					<-release
				}
			}))
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "probe"})
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			for _, id := range []string{"first", "second"} {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": id, "model": "ws-billing", "input": id})
				assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			block.Store(true)
			releaseFinish()
			select {
			case <-settling:
			case <-time.After(3 * time.Second):
				t.Fatal("response did not enter settlement")
			}
			for _, id := range []string{"first", "second", "probe"} {
				controlType := "response.interrupt"
				if tc.inject && id == "first" {
					controlType = "response.inject"
				}
				sendResponsesWSTestEvent(t, fixture.client, responsesWSTestControl(controlType, id))
			}
			require.Equal(t, probeReply, readResponsesWSTestEvent(t, fixture.client))
			advance()
			if tc.clientClose {
				require.NoError(t, fixture.client.Close())
			}
			if tc.clientClose || tc.upstreamClose {
				select {
				case <-fixture.upstreamDone:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream connection did not close while settlement was pending")
				}
			}
			if tc.overflow {
				_, _, err := fixture.client.ReadMessage()
				require.True(t, websocket.IsCloseError(err, websocket.CloseInternalServerErr), "overflow must close without waiting for settlement: %v", err)
			}
			unblock()
			if !tc.clientClose && !tc.overflow {
				completed := make(map[string]bool)
				for range tc.replyCount + 2 {
					require.NoError(t, fixture.client.SetReadDeadline(time.Now().Add(3*time.Second)))
					_, body, err := fixture.client.ReadMessage()
					require.NoError(t, err)
					var event map[string]any
					require.NoError(t, common.Unmarshal(body, &event))
					if event["type"] == "response.completed" {
						id := event["response"].(map[string]any)["id"].(string)
						assert.False(t, completed[id], "each terminal must be delivered once")
						completed[id] = true
						continue
					}
					id, _ := event["response_id"].(string)
					assert.True(t, completed[id], "a reply must follow its own terminal")
					require.NotEmpty(t, expected[id])
					assert.Equal(t, expected[id][0], string(body), "replies must retain their bytes and lane order")
					expected[id] = expected[id][1:]
				}
				assert.Equal(t, map[string]bool{"first": true, "second": true}, completed)
				assert.Empty(t, expected["first"])
				assert.Empty(t, expected["second"])
				if tc.upstreamClose {
					_, _, err := fixture.client.ReadMessage()
					assert.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "upstream close must follow both lanes' replies: %v", err)
				}
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{100, 100, 100})
		})
	}
}

func TestResponsesWebSocketInterruptAdmissionAndIsolation(t *testing.T) {
	const lateError = `{"type":"error","response_id":"a","status":400,"error":{"type":"invalid_request_error","code":"invalid_mode","message":"Unsupported interruption mode"},"vendor":{"integer":9007199254740993,"zero":0.0,"enabled":false}}`
	proceed := make(chan struct{})
	defer close(proceed)
	fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		for range 2 {
			create := readResponsesWSTestEvent(t, ws)
			lane := create["stream_id"].(string)
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": lane, "response": map[string]any{"id": lane, "status": "in_progress"}})
		}
		first := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "response.interrupt", first["type"])
		assert.Equal(t, "a", first["response_id"])
		// Neither the wrong response nor the wrong lane acknowledges a's control.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.interrupt.accepted", "response_id": "b"})
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.interrupt.accepted", "response_id": "a", "stream_id": "b"})
		<-proceed
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.interrupt.failed", "response_id": "a", "error": map[string]any{"type": "invalid_request_error", "code": "invalid_mode", "message": "Unsupported interruption mode"}})
		second := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "retry-a", second["event_id"])
		// An explicitly correlated generic rejection is also a control failure.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "event_id": "retry-a", "response_id": "a", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "unsupported_event", "message": "Unsupported event"}})
		cancel := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "response.cancel", cancel["type"])
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "stream_id": "b", "response_id": "b", "delta": "still running"})
		<-proceed
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "event_id": "cancel-a", "response_id": "a", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "response_not_active", "message": "Cancel rejected"}})
		completeResponsesWSTestRequest(t, ws, "a", "a", 100)
		// A late interrupt still belongs to a, although b is now the only active lane.
		late := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "response.interrupt", late["type"])
		assert.Equal(t, "a", late["response_id"])
		assert.Equal(t, "late-a", late["event_id"])
		for _, unrelated := range []map[string]any{
			{"type": "error", "response_id": "missing", "event_id": "late-a"},
			{"type": "error", "response_id": "a", "stream_id": "b", "event_id": "late-a"},
			{"type": "error", "response_id": "a", "event_id": "another-control"},
		} {
			unrelated["status"] = http.StatusBadRequest
			unrelated["error"] = map[string]any{"type": "invalid_request_error", "code": "invalid_mode", "message": "Unrelated control error"}
			sendResponsesWSTestEvent(t, ws, unrelated)
		}
		// This delta proves all preceding errors have passed through the reader.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "stream_id": "b", "response_id": "b", "delta": "still running after unrelated errors"})
		<-proceed
		require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(lateError)))
		retry := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "response.interrupt", retry["type"])
		assert.Equal(t, "a", retry["response_id"])
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "response_id": "a", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "response_already_completed", "message": "Already completed"}})
		completeResponsesWSTestRequest(t, ws, "b", "b", 200)
		_, _, _ = ws.ReadMessage()
	})
	for _, lane := range []string{"a", "b"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "stream_id": lane, "input": "first"})
		require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	for _, control := range []map[string]any{
		{"type": "response.interrupt"},
		{"type": "response.interrupt", "response_id": nil},
		{"type": "response.interrupt", "response_id": 123},
		{"type": "response.interrupt", "response_id": "  "},
		{"type": "response.interrupt", "response_id": "missing"},
		{"type": "response.interrupt", "response_id": "a", "stream_id": "b"},
	} {
		control["event_id"] = "invalid"
		sendResponsesWSTestEvent(t, fixture.client, control)
		rejected := readResponsesWSTestEvent(t, fixture.client)
		assert.Equal(t, "error", rejected["type"])
		assert.Equal(t, float64(400), rejected["status"])
		assert.Equal(t, "invalid", rejected["event_id"])
		if id, ok := control["response_id"].(string); ok {
			assert.Equal(t, id, rejected["response_id"])
		}
	}
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "a", "mode": "discard_partial_items"})
	for range 2 {
		assert.Equal(t, "response.interrupt.accepted", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	for _, kind := range []string{"response.interrupt", "response.cancel"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": kind, "response_id": "a", "event_id": "pending"})
		rejected := readResponsesWSTestEvent(t, fixture.client)
		assert.Equal(t, "pending", rejected["event_id"])
		assert.Equal(t, float64(400), rejected["status"])
	}
	proceed <- struct{}{}
	assert.Equal(t, "response.interrupt.failed", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "a", "event_id": "retry-a"})
	assert.Equal(t, "retry-a", readResponsesWSTestEvent(t, fixture.client)["event_id"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "response_id": "a", "event_id": "cancel-a"})
	assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "a", "event_id": "cancel-pending"})
	assert.Equal(t, "cancel-pending", readResponsesWSTestEvent(t, fixture.client)["event_id"])
	proceed <- struct{}{}
	assert.Equal(t, "cancel-a", readResponsesWSTestEvent(t, fixture.client)["event_id"])
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "a", "event_id": "late-a", "mode": "discard_partial_items"})
	assert.Equal(t, "missing", readResponsesWSTestEvent(t, fixture.client)["response_id"])
	assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "a", "event_id": "late-pending"})
	stillPending := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "late-pending", stillPending["event_id"])
	assert.Equal(t, float64(http.StatusBadRequest), stillPending["status"])
	proceed <- struct{}{}
	_, body, err := fixture.client.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, lateError, string(body), "the rejection must be forwarded byte for byte")
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "a", "mode": "discard_partial_items"})
	retried := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "a", retried["response_id"])
	assert.Equal(t, "response_already_completed", retried["error"].(map[string]any)["code"])
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{100, 200})
}

func TestResponsesWebSocketInterruptPendingLimit(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%t", reject), func(t *testing.T) {
			reply := map[string]any{"type": "response.interrupt.accepted", "response_id": "response-0"}
			if reject {
				reply = map[string]any{"type": "error", "response_id": "response-0", "status": float64(http.StatusBadRequest), "error": map[string]any{"type": "invalid_request_error", "code": "invalid_mode", "message": "Unsupported interruption mode"}}
			}
			acknowledge := make(chan struct{})
			defer close(acknowledge)
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				for i := range 129 {
					readResponsesWSTestEvent(t, ws)
					id := fmt.Sprintf("response-%d", i)
					completeResponsesWSTestRequest(t, ws, "", id, 1)
					if i < 128 {
						control := readResponsesWSTestEvent(t, ws)
						assert.Equal(t, "response.interrupt", control["type"])
						assert.Equal(t, id, control["response_id"])
					}
				}
				<-acknowledge
				// The first response has left the completion history, but its
				// control reply still releases capacity and reaches the client.
				sendResponsesWSTestEvent(t, ws, reply)
				control := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response-128", control["response_id"])
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.interrupt.accepted", "response_id": "response-128"})
				_, _, _ = ws.ReadMessage()
			})
			for i := range 129 {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "next"})
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": fmt.Sprintf("response-%d", i)})
			}
			limited := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, float64(429), limited["status"])
			assert.Equal(t, "response-128", limited["response_id"])
			acknowledge <- struct{}{}
			assert.Equal(t, reply, readResponsesWSTestEvent(t, fixture.client))
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "response-128"})
			assert.Equal(t, "response.interrupt.accepted", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			quotas := make([]int, 129)
			for i := range quotas {
				quotas[i] = 1
			}
			assertResponsesWSAccounting(t, fixture, quotas)
		})
	}
}

func TestResponsesWebSocketControlDoesNotConsumeGenerationError(t *testing.T) {
	for _, controlType := range []string{"response.interrupt", "response.inject"} {
		t.Run(controlType, func(t *testing.T) {
			replyType := "response.interrupt.accepted"
			if controlType == "response.inject" {
				replyType = "response.inject.created"
			}
			const failure = `{"type":"error","response_id":"first","status":500,"error":{"type":"server_error","code":"server_error","message":"Generation failed"}}`
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "first", "status": "in_progress"}})
				control := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, controlType, control["type"])
				assert.Equal(t, "first", control["response_id"])
				require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(failure)))
				// A generation failure must finish the worker so the queued request runs.
				next := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "response.create", next["type"])
				assert.Equal(t, "next", next["input"])
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": replyType, "response_id": "first"})
				completeResponsesWSTestRequest(t, ws, "", "second", 100)
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"})
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, responsesWSTestControl(controlType, "first"))
			_, body, err := fixture.client.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, failure, string(body))
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "next"})
			assert.Equal(t, replyType, readResponsesWSTestEvent(t, fixture.client)["type"])
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{100, 100})
		})
	}
}

func TestResponsesWebSocketInterruptLateErrorAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, firstLane, nextLane, errorFields, interruptEvent string
		cancel, generationFailure, closeConnection             bool
	}{
		{name: "stream only", firstLane: "planner", nextLane: "planner", errorFields: `,"stream_id":"planner"`, closeConnection: true},
		{name: "server event only", errorFields: `,"event_id":"upstream-error"`, closeConnection: true},
		{name: "stream and server event", firstLane: "planner", nextLane: "planner", errorFields: `,"stream_id":"planner","event_id":"upstream-error"`, closeConnection: true},
		{name: "current response failure", firstLane: "planner", nextLane: "planner", errorFields: `,"response_id":"second","event_id":"upstream-error"`, generationFailure: true},
		{name: "current cancel event", firstLane: "planner", nextLane: "planner", errorFields: `,"event_id":"cancel-second"`, cancel: true},
		{name: "other lane failure", firstLane: "planner", nextLane: "research", errorFields: `,"stream_id":"research","event_id":"upstream-error"`, generationFailure: true},
		{name: "interrupt event", firstLane: "planner", nextLane: "planner", errorFields: `,"event_id":"interrupt-first"`, interruptEvent: "interrupt-first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nextSeen := make(chan struct{})
			releaseNext := sync.OnceFunc(func() { close(nextSeen) })
			defer releaseNext()
			payload := `{"type":"error"` + tc.errorFields + `,"status":400,"error":{"type":"invalid_request_error","code":"invalid_mode","message":"Unsupported interruption mode"},"vendor":{"integer":9007199254740993,"zero":0.0,"enabled":false}}`
			if tc.generationFailure {
				payload = `{"type":"error"` + tc.errorFields + `,"status":500,"error":{"type":"server_error","code":"server_error","message":"Generation failed"}}`
			}
			initial := map[string]any{"type": "response.create", "model": "ws-billing", "input": "first"}
			next := map[string]any{"type": "response.create", "model": "ws-billing", "input": "second"}
			created := map[string]any{"type": "response.created", "response": map[string]any{"id": "second", "status": "in_progress"}}
			delta := map[string]any{"type": "response.output_text.delta", "response_id": "second", "delta": "still running"}
			if tc.firstLane != "" {
				initial["stream_id"] = tc.firstLane
			}
			if tc.nextLane != "" {
				for _, event := range []map[string]any{next, created, delta} {
					event["stream_id"] = tc.nextLane
				}
			}
			interrupt := map[string]any{"type": "response.interrupt", "response_id": "first", "mode": "discard_partial_items"}
			if tc.interruptEvent != "" {
				interrupt["event_id"] = tc.interruptEvent
			}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, _ *http.Request) {
				require.Equal(t, "response.create", readResponsesWSTestEvent(t, ws)["type"])
				completeResponsesWSTestRequest(t, ws, tc.firstLane, "first", 100)
				assert.Equal(t, interrupt, readResponsesWSTestEvent(t, ws))
				require.Equal(t, "second", readResponsesWSTestEvent(t, ws)["input"])
				sendResponsesWSTestEvent(t, ws, created)
				<-nextSeen
				if tc.cancel {
					control := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "response.cancel", control["type"])
					assert.Equal(t, "cancel-second", control["event_id"])
				}
				require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(payload)))
				if tc.closeConnection {
					_, _, err := ws.ReadMessage()
					assert.True(t, websocket.IsCloseError(err, websocket.CloseInternalServerErr), "ambiguous error must close upstream: %v", err)
					return
				}
				if tc.generationFailure {
					// A real failure must release B's worker and keep the socket reusable.
					require.Equal(t, "third", readResponsesWSTestEvent(t, ws)["input"])
					completeResponsesWSTestRequest(t, ws, tc.nextLane, "third", 100)
				} else {
					sendResponsesWSTestEvent(t, ws, delta)
					completeResponsesWSTestRequest(t, ws, tc.nextLane, "second", 200)
				}
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, initial)
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, interrupt)
			sendResponsesWSTestEvent(t, fixture.client, next)
			assert.Equal(t, created, readResponsesWSTestEvent(t, fixture.client))
			releaseNext()
			if tc.cancel {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.cancel", "response_id": "second", "event_id": "cancel-second"})
			}
			_, body, err := fixture.client.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, payload, string(body), "the error must be forwarded unchanged")
			quotas := []int{100, 100}
			if tc.closeConnection {
				_, _, err = fixture.client.ReadMessage()
				var closeErr *websocket.CloseError
				require.ErrorAs(t, err, &closeErr)
				assert.Equal(t, websocket.CloseInternalServerErr, closeErr.Code)
			} else if tc.generationFailure {
				next["input"] = "third"
				sendResponsesWSTestEvent(t, fixture.client, next)
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				quotas = append(quotas, 100)
			} else {
				assert.Equal(t, delta, readResponsesWSTestEvent(t, fixture.client))
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, quotas)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
			require.Len(t, logs, len(quotas))
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(logs[1].Other, &other))
			stream, ok := other["stream_status"].(map[string]any)
			require.True(t, ok)
			if tc.closeConnection {
				assert.NotEqual(t, "done", stream["end_reason"])
				assert.NotContains(t, stream, "response_status", "the old interrupt is not B's terminal")
				assert.NotContains(t, logs[1].Other, "invalid_mode")
			} else if tc.generationFailure {
				assert.Equal(t, "failed", stream["response_status"])
			} else {
				assert.Equal(t, "completed", stream["response_status"])
				assert.Equal(t, 200, logs[1].PromptTokens, "B must retain its real terminal usage")
			}
		})
	}
}

func TestResponsesWebSocketAmbiguousResponseControlError(t *testing.T) {
	for _, tc := range []struct {
		name, secondControl, errorCode string
		late, inject                   bool
	}{
		{name: "inject and interrupt", inject: true, secondControl: "response.interrupt", errorCode: "response_not_active"},
		{name: "inject and cancel", inject: true, secondControl: "response.cancel", errorCode: "response_not_active"},
		{name: "two interrupts", secondControl: "response.interrupt", errorCode: "response_not_active"},
		{name: "interrupt and cancel", secondControl: "response.cancel", errorCode: "response_not_active"},
		{name: "unscoped rejection after completion", late: true, errorCode: "unsupported_event"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlType := "response.interrupt"
			if tc.inject {
				controlType = "response.inject"
			}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, _ *http.Request) {
				for range 2 {
					create := readResponsesWSTestEvent(t, ws)
					lane := create["stream_id"].(string)
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": lane, "response": map[string]any{"id": lane, "status": "in_progress"}})
					if tc.late && lane == "a" {
						completeResponsesWSTestRequest(t, ws, "a", "a", 100)
					}
				}
				assert.Equal(t, controlType, readResponsesWSTestEvent(t, ws)["type"])
				if tc.secondControl != "" {
					assert.Equal(t, tc.secondControl, readResponsesWSTestEvent(t, ws)["type"])
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": tc.errorCode, "message": "Control rejected"}})
				_, _, _ = ws.ReadMessage()
			})
			for _, lane := range []string{"a", "b"} {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": lane, "model": "ws-billing", "input": "first"})
				assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
				if tc.late && lane == "a" {
					assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				}
			}
			sendResponsesWSTestEvent(t, fixture.client, responsesWSTestControl(controlType, "a"))
			if tc.secondControl != "" {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": tc.secondControl, "response_id": "b"})
			}
			assert.Equal(t, "error", readResponsesWSTestEvent(t, fixture.client)["type"])
			_, _, err := fixture.client.ReadMessage()
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			assert.Equal(t, websocket.CloseInternalServerErr, closeErr.Code)
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{100, 100})
		})
	}
}

func TestResponsesMultiAgentForwarding(t *testing.T) {
	const enabled = `{"enabled":true,"max_concurrent_subagents":3,"future":{"integer":9007199254740993}}`
	const disabled = `{"enabled":false,"max_concurrent_subagents":0,"future":{"zero":0.0}}`
	const output = `[{"type":"multi_agent_call","id":"mac_1","call_id":"spawn_1","action":"spawn_agent","arguments":"{}","agent":{"agent_name":"/root"}},{"type":"multi_agent_call_output","call_id":"spawn_1","output":[],"agent":{"agent_name":"/root"}},{"type":"agent_message","author":"/root/research","recipient":"/root","agent":{"agent_name":"/root"},"content":[{"type":"encrypted_content","encrypted_content":"enc_test"}]},{"type":"message","role":"assistant","agent":{"agent_name":"/root"},"content":[{"type":"output_text","text":"done"}]}]`
	const response = `{"id":"first","object":"response","status":"completed","output":` + output + `,"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}`
	const itemEvent = `{"type":"response.output_item.done","response_id":"first","agent":{"agent_name":"/root/research"},"output_index":2,"item":{"type":"agent_message","author":"/root/research","recipient":"/root","agent":{"agent_name":"/root"},"content":[{"type":"encrypted_content","encrypted_content":"enc_test"}]}}`
	const terminal = `{"type":"response.completed","response":` + response + `}`
	for _, tc := range []struct {
		name, transport, multiAgent string
		override, omitBeta          bool
	}{
		{name: "HTTP JSON", transport: "json", multiAgent: enabled},
		{name: "HTTP SSE explicit zero", transport: "sse", multiAgent: disabled},
		{name: "WS flat", transport: "flat", multiAgent: enabled},
		{name: "WS wrapped explicit zero", transport: "wrapped", multiAgent: disabled},
		{name: "HTTP header override", transport: "json", multiAgent: enabled, override: true},
		{name: "WS header override", transport: "flat", multiAgent: enabled, override: true},
		{name: "HTTP omitted configuration", transport: "json", omitBeta: true},
		{name: "WS does not synthesize beta", transport: "flat", multiAgent: enabled, omitBeta: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			if !tc.omitBeta {
				headers["Openai-Beta"] = []string{"responses_multi_agent=v1", "future=v1, another=v2"}
			}
			wantBeta := headers.Values("OpenAI-Beta")
			if tc.override {
				wantBeta = []string{"channel=v1"}
			}
			type observedRequest struct {
				body    []byte
				headers http.Header
			}
			received := make(chan observedRequest, 2)
			fixture := newResponsesWSBillingTestWithHeaders(t, `tier("input", p * 2)`, func(ws *websocket.Conn, r *http.Request) {
				for _, id := range []string{"first", "second"} {
					_, body, err := ws.ReadMessage()
					require.NoError(t, err)
					received <- observedRequest{body: body, headers: r.Header.Clone()}
					require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(strings.ReplaceAll(itemEvent, `"first"`, `"`+id+`"`))))
					require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(strings.ReplaceAll(terminal, `"first"`, `"`+id+`"`))))
				}
				_, _, _ = ws.ReadMessage()
			}, headers, nil)
			if tc.override {
				require.NoError(t, model.DB.Model(fixture.channel).Update("header_override", `{"OpenAI-Beta":"channel=v1"}`).Error)
			}
			fixture.httpUpstream = func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				received <- observedRequest{body: body, headers: r.Header.Clone()}
				if tc.transport == "json" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", itemEvent, terminal)
			}
			body := `{"model":"ws-billing","input":` + output
			if tc.multiAgent != "" {
				body += `,"multi_agent":` + tc.multiAgent
			}
			body += `}`
			quotas := []int{100}
			if tc.transport == "json" || tc.transport == "sse" {
				if tc.transport == "sse" {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(body))
				require.NoError(t, err)
				request.Header = headers.Clone()
				request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
				require.NoError(t, err)
				wire, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, http.StatusOK, resp.StatusCode, "%s", wire)
				if tc.transport == "json" {
					assert.Equal(t, response, string(wire))
				} else {
					assert.Contains(t, string(wire), "event: response.output_item.done\ndata: "+itemEvent+"\n\n")
					assert.Contains(t, string(wire), "event: response.completed\ndata: "+terminal+"\n\n")
				}
				select {
				case <-fixture.httpDone:
				case <-time.After(3 * time.Second):
					t.Fatal("HTTP response did not finish settlement")
				}
			} else {
				quotas = append(quotas, 100)
				for _, id := range []string{"first", "second"} {
					request := body
					if id == "second" {
						request = strings.TrimSuffix(request, "}") + `,"previous_response_id":"first"}`
					}
					if tc.transport == "wrapped" {
						request = `{"type":"response.create","response":` + request + `}`
					} else {
						request = `{"type":"response.create",` + strings.TrimPrefix(request, "{")
					}
					require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(request)))
					for _, event := range []string{itemEvent, terminal} {
						require.NoError(t, fixture.client.SetReadDeadline(time.Now().Add(3*time.Second)))
						_, wire, err := fixture.client.ReadMessage()
						require.NoError(t, err)
						assert.Equal(t, strings.ReplaceAll(event, `"first"`, `"`+id+`"`), string(wire))
					}
				}
				assert.Equal(t, int32(1), fixture.connections.Load())
			}
			for i := range len(quotas) {
				observed := <-received
				assert.Equal(t, wantBeta, observed.headers.Values("OpenAI-Beta"))
				var request map[string]common.RawMessage
				require.NoError(t, common.Unmarshal(observed.body, &request))
				assert.Equal(t, tc.multiAgent, string(request["multi_agent"]))
				assert.JSONEq(t, output, string(request["input"]))
				assert.Equal(t, `"gpt-4o"`, string(request["model"]))
				if i == 1 {
					assert.Equal(t, `"first"`, string(request["previous_response_id"]))
				}
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, quotas)
		})
	}
}

func setupResponsesWSSubscriptionTest(t *testing.T, fixture *responsesWSBillingTest, quota int) *model.UserSubscription {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}))
	plan := &model.SubscriptionPlan{Title: "WebSocket injection", TotalAmount: int64(quota), QuotaResetPeriod: model.SubscriptionResetNever}
	require.NoError(t, model.DB.Create(plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	subscription := &model.UserSubscription{
		UserId: fixture.user.Id, PlanId: plan.Id, AmountTotal: int64(quota), Status: "active",
		StartTime: time.Now().Unix(), EndTime: time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, model.DB.Create(subscription).Error)
	userSetting := fixture.user.GetSetting()
	userSetting.BillingPreference = "subscription_only"
	require.NoError(t, model.UpdateUserSetting(fixture.user.Id, userSetting))
	t.Cleanup(func() {
		fixture.closeAndWait(t)
		require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Delete(&model.SubscriptionPreConsumeRecord{}).Error)
		require.NoError(t, model.DB.Delete(subscription).Error)
		require.NoError(t, model.DB.Delete(plan).Error)
		model.InvalidateSubscriptionPlanCache(plan.Id)
	})
	return subscription
}

func assertResponsesWSReservedQuota(t *testing.T, fixture *responsesWSBillingTest, subscription *model.UserSubscription, userQuota, tokenQuota, reserved int) {
	t.Helper()
	var user model.User
	var token model.Token
	require.NoError(t, model.DB.First(&user, fixture.user.Id).Error)
	require.NoError(t, model.DB.First(&token, fixture.token.Id).Error)
	assert.Equal(t, tokenQuota-reserved, token.RemainQuota)
	assert.Equal(t, reserved, token.UsedQuota)
	if subscription == nil {
		assert.Equal(t, userQuota-reserved, user.Quota)
		return
	}
	var sub model.UserSubscription
	require.NoError(t, model.DB.First(&sub, subscription.Id).Error)
	assert.Equal(t, int64(reserved), sub.AmountUsed)
	assert.Equal(t, userQuota, user.Quota, "an injection must retain its subscription funding source")
}

func TestResponsesWebSocketInjectReservesBeforeForwarding(t *testing.T) {
	const tiered = `param("service_tier") == "priority" ? (len <= 5 ? tier("short", p * 2) : tier("long", p * 4)) : tier("wrong", p * 100)`
	for _, tc := range []struct {
		name, expression, legacy string
		subscription, trusted    bool
		freeGroup                bool
		initial, first, total    int
		settled                  int
	}{
		{name: "wallet tier crossing", expression: tiered, initial: 6, first: 10, total: 28, settled: 14},
		{name: "subscription tier crossing", expression: tiered, subscription: true, initial: 6, first: 10, total: 28, settled: 14},
		{name: "trusted wallet requires full reservation", expression: tiered, trusted: true, first: 10, total: 28, settled: 14},
		{name: "legacy token ratio", expression: tiered, legacy: "ratio", initial: 18, first: 30, total: 42, settled: 21},
		{name: "fixed expression charged once", expression: `tier("request", fixed(0.002))`, initial: 1000, first: 1000, total: 1000, settled: 1000},
		{name: "legacy request price charged once", expression: tiered, legacy: "price", initial: 1000, first: 1000, total: 1000, settled: 1000},
		{name: "free group", expression: tiered, freeGroup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousQuota := *operation_setting.GetQuotaSetting()
			t.Cleanup(func() { *operation_setting.GetQuotaSetting() = previousQuota })
			operation_setting.GetQuotaSetting().PreConsumeMultiplier = 2
			operation_setting.GetQuotaSetting().TrustQuotaUSD = 0
			operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = !tc.freeGroup
			if tc.trusted {
				operation_setting.GetQuotaSetting().TrustQuotaUSD = 0.000001
			}
			if tc.legacy != "" {
				ratios, err := common.Marshal(ratio_setting.GetModelRatioCopy())
				require.NoError(t, err)
				prices, err := common.Marshal(ratio_setting.GetModelPriceCopy())
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(ratios)))
					require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(prices)))
				})
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"ws-billing":3}`))
				if tc.legacy == "price" {
					require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"ws-billing":0.002}`))
				}
			}
			ready := make(chan struct{})
			var fixture *responsesWSBillingTest
			var subscription *model.UserSubscription
			fixture = newResponsesWSBillingTest(t, tc.expression, func(ws *websocket.Conn, _ *http.Request) {
				<-ready
				readResponsesWSTestEvent(t, ws)
				assertResponsesWSReservedQuota(t, fixture, subscription, 100000, 3000, tc.initial)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				for i, reserved := range []int{tc.first, tc.total} {
					injection := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, fmt.Sprintf("inject-%d", i), injection["event_id"])
					assertResponsesWSReservedQuota(t, fixture, subscription, 100000, 3000, reserved)
					// A barrier without an acknowledgement keeps both inputs pending.
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "first", "delta": ""})
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.failed", "response_id": "first", "error": map[string]any{"code": "invalid_input", "message": "Rejected"}})
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
				injection := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "inject-2", injection["event_id"])
				assertResponsesWSReservedQuota(t, fixture, subscription, 100000, 3000, tc.total)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "response": map[string]any{"id": "first", "status": "completed"}})
				_, _, _ = ws.ReadMessage()
			})
			constant.CountToken = true
			if tc.legacy != "" {
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": `{}`}))
			}
			if tc.freeGroup {
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"group_ratio_setting.group_ratio": `{"default":0}`}))
			}
			if tc.subscription {
				subscription = setupResponsesWSSubscriptionTest(t, fixture, 100000)
			}
			close(ready)
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "hello", "instructions": "world", "service_tier": "priority"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			for i := range 3 {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{
					"type": "response.inject", "response_id": "first", "event_id": fmt.Sprintf("inject-%d", i),
					"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": " hey"}},
				})
				if i == 0 {
					require.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
					require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
						"billing_setting.billing_expr":         `{"ws-billing":"tier(\"changed\", p * 1000)"}`,
						"group_ratio_setting.group_ratio":      `{"default":3}`,
						"quota_setting.pre_consume_multiplier": "9",
					}))
				} else if i == 1 {
					for _, event := range []string{"response.output_text.delta", "response.inject.failed", "response.inject.created"} {
						require.Equal(t, event, readResponsesWSTestEvent(t, fixture.client)["type"])
					}
					assertResponsesWSReservedQuota(t, fixture, subscription, 100000, 3000, tc.total)
				}
			}
			require.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assertResponsesWSReservedQuota(t, fixture, subscription, 100000, 3000, tc.settled)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, tc.settled, logs[0].Quota)
			assert.Equal(t, 7, logs[0].PromptTokens)
		})
	}
}

func TestResponsesWebSocketInjectReservationFailureKeepsGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, expression      string
		userQuota, tokenQuota int
		subscriptionQuota     int
		code                  types.ErrorCode
	}{
		{name: "wallet exhausted", userQuota: 6, tokenQuota: 3000, code: types.ErrorCodeInsufficientUserQuota},
		{name: "token failure rolls back wallet", userQuota: 100000, tokenQuota: 6, code: types.ErrorCodePreConsumeTokenQuotaFailed},
		{name: "subscription exhausted", userQuota: 100000, tokenQuota: 3000, subscriptionQuota: 6, code: types.ErrorCodeInsufficientUserQuota},
		{name: "token failure rolls back subscription", userQuota: 100000, tokenQuota: 6, subscriptionQuota: 100000, code: types.ErrorCodePreConsumeTokenQuotaFailed},
		{name: "quota overflow", expression: `len > 5 ? tier("overflow", p * 1e100) : tier("input", p * 2)`, userQuota: 100000, tokenQuota: 3000, code: types.ErrorCodeModelPriceError},
		{name: "negative quota", expression: `len > 5 ? tier("negative", -2) : tier("input", p * 2)`, userQuota: 100000, tokenQuota: 3000, code: types.ErrorCodeModelPriceError},
		{name: "expression evaluation fails", expression: `len > 5 ? tier("invalid", float(param("missing"))) : tier("input", p * 2)`, userQuota: 100000, tokenQuota: 3000, code: types.ErrorCodeModelPriceError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousQuota := *operation_setting.GetQuotaSetting()
			t.Cleanup(func() { *operation_setting.GetQuotaSetting() = previousQuota })
			operation_setting.GetQuotaSetting().TrustQuotaUSD = 0
			operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1
			expression := tc.expression
			if expression == "" {
				expression = `tier("input", p * 2)`
			}
			ready := make(chan struct{})
			var fixture *responsesWSBillingTest
			var subscription *model.UserSubscription
			fixture = newResponsesWSBillingTest(t, expression, func(ws *websocket.Conn, _ *http.Request) {
				<-ready
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				injection := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "valid", injection["event_id"], "failed reservations must never reach upstream")
				assertResponsesWSReservedQuota(t, fixture, subscription, tc.userQuota, tc.tokenQuota, 5)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
				completeResponsesWSTestRequest(t, ws, "planner", "first", 5)
				_, _, _ = ws.ReadMessage()
			})
			constant.CountToken = true
			require.NoError(t, model.DecreaseUserQuota(fixture.user.Id, 100000-tc.userQuota, false))
			fixture.token.RemainQuota = tc.tokenQuota
			require.NoError(t, fixture.token.Update())
			if tc.subscriptionQuota > 0 {
				subscription = setupResponsesWSSubscriptionTest(t, fixture, tc.subscriptionQuota)
			}
			close(ready)
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "hello", "instructions": "world"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{
				"type": "response.inject", "response_id": "first", "event_id": "invalid",
				"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": strings.Repeat(" hey", 10)}},
			})
			rejected := readResponsesWSTestEvent(t, fixture.client)
			require.Equal(t, "error", rejected["type"])
			assert.Equal(t, "invalid", rejected["event_id"])
			assert.Equal(t, "first", rejected["response_id"])
			assert.Equal(t, "planner", rejected["stream_id"])
			status := http.StatusForbidden
			if tc.code == types.ErrorCodeModelPriceError {
				status = http.StatusBadRequest
			}
			assert.Equal(t, float64(status), rejected["status"])
			require.IsType(t, map[string]any{}, rejected["error"])
			assert.Equal(t, string(tc.code), rejected["error"].(map[string]any)["code"])
			assertResponsesWSReservedQuota(t, fixture, subscription, tc.userQuota, tc.tokenQuota, 3)
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{
				"type": "response.inject", "response_id": "first", "event_id": "valid",
				"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": " hey"}},
			})
			require.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assertResponsesWSReservedQuota(t, fixture, subscription, tc.userQuota, tc.tokenQuota, 5)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, 5, logs[0].Quota)
			assert.Equal(t, 5, logs[0].PromptTokens)
		})
	}
}

func TestResponsesWebSocketInjectEstimatedUsage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		replies       []string
		disableCount  bool
		upstreamClose bool
		terminalUsage int
		extraAck      bool
		wantPrompt    int
	}{
		{name: "accepted before client disconnect", replies: []string{"response.inject.created"}, wantPrompt: 5},
		{name: "accepted before upstream EOF", replies: []string{"response.inject.created"}, upstreamClose: true, wantPrompt: 5},
		{name: "mixed accepted rejected and pending", replies: []string{"response.inject.created", "response.inject.failed", "error", "response.inject.created", ""}, wantPrompt: 11},
		{name: "pending is not accepted", replies: []string{""}, wantPrompt: 3},
		{name: "wrong lane is not accepted", replies: []string{"other-lane"}, wantPrompt: 3},
		{name: "unmatched acceptance", extraAck: true, wantPrompt: 3},
		{name: "duplicate acceptance without pending input", replies: []string{"response.inject.created"}, extraAck: true, wantPrompt: 5},
		{name: "terminal usage replaces estimate", replies: []string{"response.inject.created"}, terminalUsage: 29, wantPrompt: 29},
		{name: "token estimation disabled", replies: []string{"response.inject.created"}, disableCount: true, wantPrompt: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				for i, replyType := range tc.replies {
					injection := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "response.inject", injection["type"])
					assert.Equal(t, fmt.Sprintf("inject-%d", i), injection["event_id"])
					if replyType == "" {
						continue
					}
					reply := map[string]any{"type": replyType, "response_id": "first"}
					if replyType == "other-lane" {
						reply["type"], reply["stream_id"] = "response.inject.created", "other"
					}
					if replyType == "response.inject.failed" || replyType == "error" {
						reply["event_id"] = injection["event_id"]
						reply["error"] = map[string]any{"type": "invalid_request_error", "code": "invalid_tool_output", "message": "Invalid tool output"}
					}
					sendResponsesWSTestEvent(t, ws, reply)
				}
				if tc.extraAck {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "first", "delta": "generated"})
				if tc.terminalUsage > 0 {
					completeResponsesWSTestRequest(t, ws, "planner", "first", tc.terminalUsage)
				}
				if !tc.upstreamClose {
					_, _, _ = ws.ReadMessage()
				}
			})
			constant.CountToken = !tc.disableCount
			// These two words and their separator estimate to three tokens. Each
			// injection contributes only its output, without repeating instructions.
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "hello", "instructions": "world"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			for i, replyType := range tc.replies {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{
					"type": "response.inject", "response_id": "first", "event_id": fmt.Sprintf("inject-%d", i),
					"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": strings.Repeat(" hey", i+1)}},
				})
				if replyType != "" {
					reply := readResponsesWSTestEvent(t, fixture.client)
					if replyType == "other-lane" {
						assert.Equal(t, "other", reply["stream_id"])
						replyType = "response.inject.created"
					}
					assert.Equal(t, replyType, reply["type"])
				}
			}
			if tc.extraAck {
				assert.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			require.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
			if tc.terminalUsage > 0 {
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			if tc.upstreamClose {
				_, _, err := fixture.client.ReadMessage()
				require.Error(t, err)
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{tc.wantPrompt})
			var log model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).First(&log).Error)
			assert.Equal(t, tc.wantPrompt, log.PromptTokens)
		})
	}
}

func TestResponsesWebSocketInjectEstimatesStayWithGeneration(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		for _, id := range []string{"first", "other"} {
			readResponsesWSTestEvent(t, ws)
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": id, "response": map[string]any{"id": id, "status": "in_progress"}})
		}
		for _, id := range []string{"first", "other"} {
			injection := readResponsesWSTestEvent(t, ws)
			assert.Equal(t, id, injection["response_id"])
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": id})
		}
		assert.Equal(t, "first", readResponsesWSTestEvent(t, ws)["response_id"])
		// Completion without usage settles the first generation's accepted input.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "response": map[string]any{"id": "first", "status": "completed"}})
		assert.Equal(t, "first", readResponsesWSTestEvent(t, ws)["stream_id"])
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "first", "response": map[string]any{"id": "next", "status": "in_progress"}})
		// The old pending injection must not increase the newer generation.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "response": map[string]any{"id": "other", "status": "completed"}})
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "next", "delta": "generated"})
		_, _, _ = ws.ReadMessage()
	})
	constant.CountToken = true
	for _, id := range []string{"first", "other"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": id, "model": "ws-billing", "input": "hey"})
		require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	for i, id := range []string{"first", "other", "first"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{
			"type": "response.inject", "response_id": id,
			"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": strings.Repeat(" hey", i+1)}},
		})
		if i < 2 {
			ack := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "response.inject.created", ack["type"])
			assert.Equal(t, id, ack["response_id"])
		}
	}
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "first", "model": "ws-billing", "input": " hey hey hey hey", "previous_response_id": "first"})
	// Late replies bypass the new worker; independent lanes can publish in
	// either order too. Wait for every event before interrupting the new request.
	var events []any
	for range 4 {
		events = append(events, readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	assert.ElementsMatch(t, []any{"response.created", "response.inject.created", "response.completed", "response.output_text.delta"}, events)
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{4, 5, 6})
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	require.Len(t, logs, 3)
	assert.Equal(t, []int{4, 5, 6}, []int{logs[0].PromptTokens, logs[1].PromptTokens, logs[2].PromptTokens})
}

func TestResponsesWebSocketInjectPluginFilter(t *testing.T) {
	const toolInput = `[{"type":"function_call_output","call_id":"call_1","output":"forbidden"}]`
	for _, tc := range []struct {
		name, input                                                     string
		disableCount, completed, disableSensitive, disablePrompt, allow bool
	}{
		{name: "message text", input: `[{"role":"user","content":"forbidden"}]`},
		{name: "message parts without counting", input: `[{"role":"user","content":[{"type":"input_text","text":"FORBIDDEN"}]}]`, disableCount: true},
		{name: "tool output", input: toolInput},
		{name: "tool parts without counting", input: `[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"forbidden"}]}]`, disableCount: true},
		{name: "completed response", input: toolInput, completed: true},
		{name: "completed response without counting", input: toolInput, completed: true, disableCount: true},
		{name: "global check disabled", input: toolInput, disableSensitive: true, allow: true},
		{name: "prompt check disabled", input: toolInput, disablePrompt: true, disableCount: true, allow: true},
		{name: "unmatched input", input: `[{"role":"user","content":"allowed"}]`, allow: true},
		{name: "empty input", input: `[]`, allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousQuota := *operation_setting.GetQuotaSetting()
			t.Cleanup(func() {
				*operation_setting.GetQuotaSetting() = previousQuota
			})
			configureResponsesWSInputFilter(t, !tc.disableSensitive && !tc.disablePrompt)
			operation_setting.GetQuotaSetting().TrustQuotaUSD = 0
			operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1

			ack := map[string]any{"type": "response.inject.created", "response_id": "first"}
			if tc.completed {
				ack["type"] = "response.inject.failed"
				ack["error"] = map[string]any{"code": "response_already_completed", "message": "Already completed"}
			}
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				if tc.completed {
					completeResponsesWSTestRequest(t, ws, "planner", "first", 3)
				}
				if tc.allow {
					injection := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "candidate", injection["event_id"])
					sendResponsesWSTestEvent(t, ws, ack)
				}
				injection := readResponsesWSTestEvent(t, ws)
				assert.Equal(t, "valid", injection["event_id"], "rejected sensitive input must never reach upstream")
				sendResponsesWSTestEvent(t, ws, ack)
				if !tc.completed {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "first", "delta": "still generating"})
					completeResponsesWSTestRequest(t, ws, "planner", "first", 5)
				}
				_, _, _ = ws.ReadMessage()
			})
			constant.CountToken = !tc.disableCount
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "hello", "instructions": "world"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			reserved := 3
			if tc.disableCount {
				reserved = 0
			}
			if tc.completed {
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				reserved = 3
			}
			assertResponsesWSReservedQuota(t, fixture, nil, 100000, 3000, reserved)
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{
				"type": "response.inject", "response_id": "first", "event_id": "candidate", "input": common.RawMessage(tc.input),
			})
			reply := readResponsesWSTestEvent(t, fixture.client)
			if tc.allow {
				require.Equal(t, ack["type"], reply["type"])
			} else {
				require.Equal(t, "error", reply["type"])
				assert.Equal(t, "candidate", reply["event_id"])
				assert.Equal(t, "first", reply["response_id"])
				assert.Equal(t, "planner", reply["stream_id"])
				require.IsType(t, map[string]any{}, reply["error"])
				apiErr := reply["error"].(map[string]any)
				assert.Equal(t, string(types.ErrorCodeRequestRejected), apiErr["code"])
				assert.Contains(t, apiErr["message"], "Blocked")
				assertResponsesWSReservedQuota(t, fixture, nil, 100000, 3000, reserved)
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{
				"type": "response.inject", "response_id": "first", "event_id": "valid",
				"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": " hey"}},
			})
			require.Equal(t, ack["type"], readResponsesWSTestEvent(t, fixture.client)["type"])
			settled := 3
			if !tc.completed {
				require.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				settled = 5
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{settled})
		})
	}
}

func TestResponsesWebSocketInjectInputUniqueness(t *testing.T) {
	const input = `[{"type":"function_call_output","call_id":"call_1","output":" hey"}]`
	const sensitiveInput = `[{"type":"function_call_output","call_id":"call_1","output":"forbidden"}]`
	const valid = `{"type":"response.inject","response_id":"first","stream_id":"planner","event_id":"valid","input":` + input + `}`
	for _, tc := range []struct {
		name, members                                            string
		reject, disableCount, completed, sensitive, limitedQuota bool
		inputTokens                                              int
	}{
		{name: "empty then input", members: `"input":[],"input":` + input, reject: true},
		{name: "input then empty", members: `"input":` + input + `,"input":[]`, reject: true},
		{name: "identical inputs", members: `"input":` + input + `,"input":` + input, reject: true},
		{name: "identical empty arrays", members: `"input":[],"input":[]`, reject: true},
		{name: "escaped duplicate", members: `"input":[],"\u0069nput":` + input, reject: true},
		{name: "escaped first input", members: `"in\u0070ut":[],"input":` + input, reject: true},
		{name: "insufficient quota", members: `"input":[],"input":[{"role":"user","content":" hey hey hey hey hey hey hey hey hey hey"}]`, reject: true, limitedQuota: true},
		{name: "sensitive input", members: `"input":[],"input":` + sensitiveInput, reject: true, sensitive: true},
		{name: "sensitive input without counting", members: `"input":[],"input":` + sensitiveInput, reject: true, sensitive: true, disableCount: true},
		{name: "checks disabled", members: `"input":[],"input":` + sensitiveInput, reject: true, disableCount: true},
		{name: "completed response", members: `"input":[],"input":` + input, reject: true, completed: true},
		{name: "single empty array", members: `"input":[]`},
		{name: "single escaped input", members: `"\u0069nput":` + input, inputTokens: 2},
		{name: "single input with nested field", members: `"input": ` + input + `, "vendor":{"input":[],"number":9007199254740993}`, inputTokens: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousQuota := *operation_setting.GetQuotaSetting()
			t.Cleanup(func() {
				*operation_setting.GetQuotaSetting() = previousQuota
			})
			configureResponsesWSInputFilter(t, tc.sensitive)
			operation_setting.GetQuotaSetting().TrustQuotaUSD = 0
			operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1
			candidate := `{"type":"response.inject", "response_id":"first","stream_id":"planner","event_id":"candidate",` + tc.members + `}`
			ack := map[string]any{"type": "response.inject.created", "response_id": "first"}
			if tc.completed {
				ack["type"] = "response.inject.failed"
				ack["error"] = map[string]any{"code": "response_already_completed", "message": "Already completed"}
			}
			terminal := map[string]any{"type": "response.completed", "response": map[string]any{"id": "first", "status": "completed"}}
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				if tc.completed {
					sendResponsesWSTestEvent(t, ws, terminal)
				}
				if !tc.reject {
					_, body, err := ws.ReadMessage()
					require.NoError(t, err)
					assert.Equal(t, candidate, string(body), "valid input must retain its original bytes")
					sendResponsesWSTestEvent(t, ws, ack)
				}
				_, body, err := ws.ReadMessage()
				require.NoError(t, err)
				assert.Equal(t, valid, string(body), "duplicate input must never reach upstream")
				sendResponsesWSTestEvent(t, ws, ack)
				if !tc.completed {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "first", "delta": "still generating"})
					sendResponsesWSTestEvent(t, ws, terminal)
				}
				_, _, _ = ws.ReadMessage()
			})
			constant.CountToken = !tc.disableCount
			tokenQuota := 3000
			if tc.limitedQuota {
				tokenQuota = 6
				fixture.token.RemainQuota = tokenQuota
				require.NoError(t, fixture.token.Update())
			}
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "hello", "instructions": "world"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			if tc.completed {
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			reserved := 3
			if tc.disableCount {
				reserved = 0
			}
			assertResponsesWSReservedQuota(t, fixture, nil, 100000, tokenQuota, reserved)
			// Send raw JSON so duplicate and escaped member names survive the test client.
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(candidate)))
			reply := readResponsesWSTestEvent(t, fixture.client)
			if tc.reject {
				require.Equal(t, "error", reply["type"])
				assert.Equal(t, float64(http.StatusBadRequest), reply["status"])
				assert.Equal(t, "candidate", reply["event_id"])
				assert.Equal(t, "first", reply["response_id"])
				assert.Equal(t, "planner", reply["stream_id"])
				require.IsType(t, map[string]any{}, reply["error"])
				assert.Equal(t, string(types.ErrorCodeInvalidRequest), reply["error"].(map[string]any)["code"])
			} else {
				require.Equal(t, ack["type"], reply["type"])
				reserved += tc.inputTokens
			}
			assertResponsesWSReservedQuota(t, fixture, nil, 100000, tokenQuota, reserved)
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(valid)))
			require.Equal(t, ack["type"], readResponsesWSTestEvent(t, fixture.client)["type"])
			if !tc.completed {
				require.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				if !tc.disableCount {
					reserved += 2
				}
			}
			fixture.closeAndWait(t)
			assertResponsesWSReservedQuota(t, fixture, nil, 100000, tokenQuota, reserved)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, reserved, logs[0].Quota)
			assert.Equal(t, reserved, logs[0].PromptTokens, "missing upstream usage must include only accepted input")
		})
	}
}

func TestResponsesWebSocketInjectMediaDoesNotBlockResponses(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		completeDuringDownload bool
		failDownload           bool
		wantPrompt             int
	}{
		{name: "active response", wantPrompt: 516},
		{name: "completed during download", completeDuringDownload: true, wantPrompt: 2},
		{name: "download failure", failDownload: true, wantPrompt: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousMedia, previousNonStream, previousLimit := constant.GetMediaToken, constant.GetMediaTokenNotStream, constant.MaxFileDownloadMB
			previousFetch, previousWorker := *system_setting.GetFetchSetting(), system_setting.WorkerUrl
			previousQuota := *operation_setting.GetQuotaSetting()
			previousCache := common.GetDiskCacheConfig()
			t.Cleanup(func() {
				constant.GetMediaToken, constant.GetMediaTokenNotStream, constant.MaxFileDownloadMB = previousMedia, previousNonStream, previousLimit
				*system_setting.GetFetchSetting(), system_setting.WorkerUrl = previousFetch, previousWorker
				*operation_setting.GetQuotaSetting() = previousQuota
				common.SetDiskCacheConfig(previousCache)
			})
			constant.GetMediaToken, constant.GetMediaTokenNotStream, constant.MaxFileDownloadMB = true, false, 1
			system_setting.GetFetchSetting().AllowPrivateIp = true
			system_setting.GetFetchSetting().AllowedPorts = []string{"1-65535"}
			system_setting.WorkerUrl = ""
			operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1
			operation_setting.GetQuotaSetting().TrustQuotaUSD = 0
			common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: true, ThresholdMB: 0, MaxSizeMB: 16, Path: t.TempDir()})
			service.InitHttpClient()

			mediaRequested, releaseMedia := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseMedia) })
			defer release()
			media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/blocked" {
					close(mediaRequested)
					<-releaseMedia
					if tc.failDownload {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
				}
				w.Header().Set("Content-Type", "audio/mpeg")
				_, _ = io.WriteString(w, "media content")
			}))
			t.Cleanup(media.Close)

			ready := make(chan struct{})
			var fixture *responsesWSBillingTest
			fixture = newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				<-ready
				for _, lane := range []string{"a", "b"} {
					create := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, lane, create["stream_id"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": lane, "response": map[string]any{"id": lane, "status": "in_progress"}})
				}
				select {
				case <-mediaRequested:
				case <-time.After(3 * time.Second):
					t.Error("injected media download did not start")
					return
				}
				// Two A events exceed its single-slot inbox if its worker is downloading.
				for _, delta := range []string{"first", "second"} {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "stream_id": "a", "response_id": "a", "delta": delta})
				}
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "stream_id": "b", "response_id": "b", "delta": "other"})
				completeResponsesWSTestRequest(t, ws, "b", "b", 3)
				if tc.completeDuringDownload {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "stream_id": "a", "response": map[string]any{"id": "a", "status": "completed"}})
				}
				if !tc.failDownload {
					injection := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "media", injection["event_id"])
					assert.Equal(t, "a", injection["response_id"])
					assertResponsesWSReservedQuota(t, fixture, nil, 100000, 3000, tc.wantPrompt+3)
					if !tc.completeDuringDownload {
						sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "a"})
					}
				}
				followup := readResponsesWSTestEvent(t, ws)
				// The next client frame proves submitResponseControl returned and
				// cleaned its own media, even while the original worker remains active.
				cachedFiles, err := filepath.Glob(filepath.Join(common.GetDiskCacheDir(), "file-*.tmp"))
				require.NoError(t, err)
				assert.Empty(t, cachedFiles)
				if tc.completeDuringDownload {
					assert.Equal(t, "response.create", followup["type"])
					assert.Equal(t, "a", followup["stream_id"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "a", "response": map[string]any{"id": "next", "status": "in_progress"}})
					// A late acknowledgement must not add the old media to the new response.
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "a"})
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "stream_id": "a", "response": map[string]any{"id": "next", "status": "completed"}})
				} else {
					assert.Equal(t, "response.inject", followup["type"])
					assert.Equal(t, "followup", followup["event_id"], "failed media must never reach upstream")
					assertResponsesWSReservedQuota(t, fixture, nil, 100000, 3000, tc.wantPrompt+3)
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "a"})
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.completed", "stream_id": "a", "response": map[string]any{"id": "a", "status": "completed"}})
				}
				_, _, _ = ws.ReadMessage()
			})
			constant.CountToken = true
			close(ready)
			for _, lane := range []string{"a", "b"} {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": lane, "model": "ws-billing", "input": "hey"})
				require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			// Each audio file contributes 256 tokens; the original ws-billing
			// model estimates "hey" as two, unlike the mapped upstream gpt-4o.
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{
				"type": "response.inject", "response_id": "a", "event_id": "media",
				"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
					map[string]any{"type": "input_text", "text": "hey"},
					map[string]any{"type": "input_file", "file_url": media.URL + "/cached"},
					map[string]any{"type": "input_file", "file_url": media.URL + "/blocked"},
				}}},
			})
			wantEvents := []string{"a:response.output_text.delta", "a:response.output_text.delta", "b:response.output_text.delta", "b:response.completed"}
			if tc.completeDuringDownload {
				wantEvents = append(wantEvents, "a:response.completed")
			}
			var events []string
			for range len(wantEvents) {
				event := readResponsesWSTestEvent(t, fixture.client)
				events = append(events, fmt.Sprintf("%v:%v", event["stream_id"], event["type"]))
			}
			assert.ElementsMatch(t, wantEvents, events, "both lanes must drain before releasing the download")
			cachedFiles, err := filepath.Glob(filepath.Join(common.GetDiskCacheDir(), "file-*.tmp"))
			require.NoError(t, err)
			require.Len(t, cachedFiles, 1, "the first file is cached while the second download is blocked")
			release()
			if tc.completeDuringDownload {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "a", "model": "ws-billing", "input": "hey", "previous_response_id": "a"})
				var nextEvents []any
				for range 3 {
					nextEvents = append(nextEvents, readResponsesWSTestEvent(t, fixture.client)["type"])
				}
				assert.ElementsMatch(t, []any{"response.created", "response.inject.created", "response.completed"}, nextEvents)
			} else {
				reply := readResponsesWSTestEvent(t, fixture.client)
				input := []any{}
				if tc.failDownload {
					require.Equal(t, "error", reply["type"])
					assert.Equal(t, "media", reply["event_id"])
					assert.Equal(t, "a", reply["response_id"])
					require.IsType(t, map[string]any{}, reply["error"])
					assert.Equal(t, string(types.ErrorCodeCountTokenFailed), reply["error"].(map[string]any)["code"])
					input = []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "yes"}}
				} else {
					require.Equal(t, "response.inject.created", reply["type"])
				}
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.inject", "response_id": "a", "event_id": "followup", "input": input})
				require.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			fixture.closeAndWait(t)
			wantPrompts := map[string]int{"responses-ws-billing-ws-0": tc.wantPrompt, "responses-ws-billing-ws-1": 3}
			wantQuota := tc.wantPrompt + 3
			if tc.completeDuringDownload {
				wantPrompts["responses-ws-billing-ws-2"] = 2
				wantQuota += 2
			}
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, len(wantPrompts))
			prompts := make(map[string]int)
			for _, log := range logs {
				prompts[log.RequestId] = log.PromptTokens
				assert.Equal(t, log.PromptTokens, log.Quota)
			}
			assert.Equal(t, wantPrompts, prompts)
			assertResponsesWSReservedQuota(t, fixture, nil, 100000, 3000, wantQuota)
		})
	}
}

func TestResponsesWebSocketInjectEstimationFailureKeepsGeneration(t *testing.T) {
	previousMedia := constant.GetMediaToken
	constant.GetMediaToken = true
	t.Cleanup(func() { constant.GetMediaToken = previousMedia })
	fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		readResponsesWSTestEvent(t, ws)
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": "first", "status": "in_progress"}})
		injection := readResponsesWSTestEvent(t, ws)
		assert.Equal(t, "valid", injection["event_id"], "the input that failed estimation must never reach upstream")
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "first", "delta": "generated"})
		_, _, _ = ws.ReadMessage()
	})
	constant.CountToken = true
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "hey"})
	require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{
		"type": "response.inject", "response_id": "first", "event_id": "invalid",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{map[string]any{"type": "input_file", "file_url": "https://[invalid"}}}},
	})
	rejected := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", rejected["type"])
	assert.Equal(t, "invalid", rejected["event_id"])
	assert.Equal(t, "first", rejected["response_id"])
	require.IsType(t, map[string]any{}, rejected["error"])
	assert.Equal(t, string(types.ErrorCodeCountTokenFailed), rejected["error"].(map[string]any)["code"])
	sendResponsesWSTestEvent(t, fixture.client, map[string]any{
		"type": "response.inject", "response_id": "first", "event_id": "valid",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "yes"}},
	})
	assert.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{4})
}

func TestResponsesWebSocketInjectLifecycle(t *testing.T) {
	const input = `[{"type":"function_call_output","call_id":"call_1","output":"result","agent":{"agent_name":"/root/research"},"extension":{"integer":9007199254740993,"zero":0.0,"enabled":false}}]`
	const inject = `{"type":"response.inject","response_id":"first","input":` + input + `}`
	const accepted = `{"type":"response.inject.created","sequence_number":42,"response_id":"first"}`
	const rejected = `{"type":"response.inject.failed","sequence_number":43,"response_id":"first","input":` + input + `,"error":{"code":"response_already_completed","message":"Already completed"}}`
	for _, tc := range []struct {
		name                               string
		terminalFirst, late, nextBeforeAck bool
		interruptBeforeAck                 bool
	}{
		{name: "multiple injections while active"},
		{name: "acknowledgements after terminal", terminalFirst: true},
		{name: "injection after completion", late: true},
		{name: "exact interrupt error outranks unnamed late injections", terminalFirst: true, interruptBeforeAck: true},
		{name: "next generation before acknowledgements", terminalFirst: true, nextBeforeAck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nextSeen := make(chan struct{})
			releaseNext := sync.OnceFunc(func() { close(nextSeen) })
			defer releaseNext()
			firstReply := accepted
			if tc.late {
				firstReply = strings.Replace(rejected, `"sequence_number":43`, `"sequence_number":42`, 1)
			}
			secondReply := rejected
			if !tc.terminalFirst && !tc.late {
				secondReply = strings.ReplaceAll(secondReply, "response_already_completed", "invalid_tool_output")
				secondReply = strings.ReplaceAll(secondReply, "Already completed", "Invalid tool output")
			}
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				if tc.late {
					completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				}
				for range 2 {
					_, body, err := ws.ReadMessage()
					require.NoError(t, err)
					assert.Equal(t, inject, string(body), "inject must preserve raw tool outputs")
				}
				if tc.terminalFirst {
					completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				}
				if tc.nextBeforeAck {
					next := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "first", next["previous_response_id"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "second", "status": "in_progress"}})
					<-nextSeen
				}
				if tc.interruptBeforeAck {
					assert.Equal(t, "response.interrupt", readResponsesWSTestEvent(t, ws)["type"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "response_id": "first", "event_id": "interrupt-first", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "invalid_mode", "message": "Unsupported interruption mode"}})
				}
				require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(firstReply)))
				require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(secondReply)))
				if !tc.terminalFirst && !tc.late {
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.output_text.delta", "response_id": "first", "stream_id": "planner", "delta": "still generating"})
					completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				}
				if tc.nextBeforeAck {
					completeResponsesWSTestRequest(t, ws, "planner", "second", 200)
				}
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "first", "multi_agent": map[string]any{"enabled": true}})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			if tc.late {
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			for range 2 {
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(inject)))
			}
			if tc.terminalFirst {
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			quotas := []int{100}
			if tc.nextBeforeAck {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": input, "previous_response_id": "first"})
				require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
				releaseNext()
				quotas = append(quotas, 200)
			}
			if tc.interruptBeforeAck {
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.interrupt", "response_id": "first", "event_id": "interrupt-first"})
				assert.Equal(t, "interrupt-first", readResponsesWSTestEvent(t, fixture.client)["event_id"])
			}
			for _, reply := range []string{firstReply, secondReply} {
				_, body, err := fixture.client.ReadMessage()
				require.NoError(t, err)
				assert.Equal(t, reply, string(body), "even late acknowledgements retain raw input and sequence numbers")
			}
			if !tc.terminalFirst && !tc.late {
				assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			if tc.nextBeforeAck {
				assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, quotas)
		})
	}
}

func TestResponsesWebSocketInjectAdmissionAndControlIsolation(t *testing.T) {
	const inject = `{"type":"response.inject","response_id":"a","event_id":"inject-a","input":[{"type":"function_call_output","call_id":"c1","output":"done"}]}`
	fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		for _, id := range []string{"a", "b"} {
			create := readResponsesWSTestEvent(t, ws)
			assert.Equal(t, id, create["stream_id"])
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": id, "response": map[string]any{"id": id, "status": "in_progress"}})
		}
		_, body, err := ws.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, inject, string(body))
		for _, control := range []string{"cancel", "interrupt"} {
			event := readResponsesWSTestEvent(t, ws)
			assert.Equal(t, "response."+control, event["type"])
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "event_id": control + "-a", "response_id": "a", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "response_not_active", "message": "Control rejected"}})
		}
		// The injection still owns its own generic error after both controls.
		sendResponsesWSTestEvent(t, ws, map[string]any{"type": "error", "event_id": "inject-a", "response_id": "a", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "invalid_tool_output", "message": "Invalid tool output"}})
		for _, id := range []string{"b", "a"} {
			event := readResponsesWSTestEvent(t, ws)
			assert.Equal(t, "response.inject", event["type"])
			assert.Equal(t, id, event["response_id"])
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": id})
		}
		completeResponsesWSTestRequest(t, ws, "a", "a", 100)
		completeResponsesWSTestRequest(t, ws, "b", "b", 100)
		_, _, _ = ws.ReadMessage()
	})
	for _, id := range []string{"a", "b"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": id, "model": "ws-billing", "input": id})
		require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	for _, invalid := range []string{
		`{"type":"response.inject","input":[]}`,
		`{"type":"response.inject","response_id":null,"input":[]}`,
		`{"type":"response.inject","response_id":123,"input":[]}`,
		`{"type":"response.inject","response_id":"  ","input":[]}`,
		`{"type":"response.inject","response_id":"a"}`,
		`{"type":"response.inject","response_id":"a","input":null}`,
		`{"type":"response.inject","response_id":"a","input":{}}`,
		`{"type":"response.inject","response_id":"a","input":"text"}`,
		`{"type":"response.inject","response_id":"missing","input":[],"stream_id":"a"}`,
		`{"type":"response.inject","response_id":"a","input":[],"stream_id":"b"}`,
		`{"type":"response.inject","response_id":"a","input":[],"stream_id":false}`,
	} {
		require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(invalid)))
		errorEvent := readResponsesWSTestEvent(t, fixture.client)
		assert.Equal(t, "error", errorEvent["type"])
		assert.Equal(t, float64(400), errorEvent["status"])
	}
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(inject)))
	for _, control := range []string{"cancel", "interrupt"} {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response." + control, "response_id": "a", "event_id": control + "-a"})
		assert.Equal(t, control+"-a", readResponsesWSTestEvent(t, fixture.client)["event_id"])
	}
	assert.Equal(t, "inject-a", readResponsesWSTestEvent(t, fixture.client)["event_id"])
	for _, id := range []string{"b", "a"} {
		retry := strings.ReplaceAll(inject, `"response_id":"a"`, `"response_id":"`+id+`"`)
		require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(retry)))
		ack := readResponsesWSTestEvent(t, fixture.client)
		assert.Equal(t, "response.inject.created", ack["type"])
		assert.Equal(t, id, ack["response_id"])
	}
	for range 2 {
		assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{100, 100})
}

func TestResponsesWebSocketInjectPendingLimits(t *testing.T) {
	for _, bytesLimit := range []bool{false, true} {
		t.Run(fmt.Sprintf("bytes=%t", bytesLimit), func(t *testing.T) {
			previousLimit := constant.MaxRequestBodyMB
			constant.MaxRequestBodyMB = 1
			t.Cleanup(func() { constant.MaxRequestBodyMB = previousLimit })
			count, eventID := 128, "inject"
			if bytesLimit {
				count, eventID = 1, strings.Repeat("x", 1<<19)
			}
			advance := make(chan struct{})
			release := sync.OnceFunc(func() { close(advance) })
			defer release()
			fixture := newResponsesWSBillingTest(t, `tier("input", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
				readResponsesWSTestEvent(t, ws)
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "planner", "response": map[string]any{"id": "first", "status": "in_progress"}})
				for range count {
					assert.Equal(t, "response.inject", readResponsesWSTestEvent(t, ws)["type"])
				}
				// A mismatched lane cannot release a pending injection's capacity.
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first", "stream_id": "other"})
				<-advance
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
				assert.Equal(t, "response.inject", readResponsesWSTestEvent(t, ws)["type"])
				sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "first"})
				completeResponsesWSTestRequest(t, ws, "planner", "first", 100)
				_, _, _ = ws.ReadMessage()
			})
			sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "planner", "model": "ws-billing", "input": "first"})
			require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			inject := map[string]any{"type": "response.inject", "response_id": "first", "event_id": eventID, "input": []any{map[string]any{"type": "function_call_output", "call_id": "c1", "output": "done"}}}
			for range count {
				sendResponsesWSTestEvent(t, fixture.client, inject)
			}
			assert.Equal(t, "other", readResponsesWSTestEvent(t, fixture.client)["stream_id"])
			sendResponsesWSTestEvent(t, fixture.client, inject)
			limited := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, float64(429), limited["status"])
			assert.Equal(t, "first", limited["response_id"])
			release()
			assert.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			sendResponsesWSTestEvent(t, fixture.client, inject)
			assert.Equal(t, "response.inject.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, []int{100})
		})
	}
}
