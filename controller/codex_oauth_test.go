package controller

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexOAuthTransport func(*http.Request) (*http.Response, error)

func (transport codexOAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type codexOAuthTestFixture struct {
	identity service.AuthIdentity
	token    string
	router   *gin.Engine
}

func setupCodexOAuthTest(t *testing.T) codexOAuthTestFixture {
	t.Helper()
	user, identity := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.Model(user).Update("role", common.RoleRootUser).Error)
	require.NoError(t, model.PublishUserAuthCache(user.Id))
	token, _, err := service.IssueAccessToken(identity)
	require.NoError(t, err)
	router := gin.New()
	group := router.Group("/api/channel/codex/oauth", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelSensitiveWrite), middleware.DisableCache())
	group.POST("/start", StartCodexOAuth)
	group.POST("/complete", CompleteCodexOAuth)
	return codexOAuthTestFixture{identity: identity, token: token, router: router}
}

func (fixture codexOAuthTestFixture) request(t *testing.T, action string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/channel/codex/oauth/"+action, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	response := httptest.NewRecorder()
	fixture.router.ServeHTTP(response, request)
	return response
}

func (fixture codexOAuthTestFixture) start(t *testing.T) service.CodexOAuthAuthorization {
	t.Helper()
	response := fixture.request(t, "start", gin.H{})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Success bool                            `json:"success"`
		Data    service.CodexOAuthAuthorization `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	return result.Data
}

func codexOAuthCallback(state string) string {
	return "http://localhost:1455/auth/callback?" + url.Values{"state": {state}, "code": {"private-code"}}.Encode()
}

func installCodexOAuthTransport(t *testing.T, proxy string, transport codexOAuthTransport) {
	t.Helper()
	client, err := service.GetHttpClientWithProxy(proxy)
	require.NoError(t, err)
	previous := client.Transport
	client.Transport = transport
	t.Cleanup(func() { client.Transport = previous })
}

func codexOAuthTokenResponse(request *http.Request) *http.Response {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-123"},"email":"codex@example.com"}`))
	body := `{"access_token":"header.` + claims + `.signature","refresh_token":"private-refresh-token","expires_in":3600}`
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func TestCodexOAuthReturnsDraftCredentialWithBoundPKCE(t *testing.T) {
	fixture := setupCodexOAuthTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))
	channel := &model.Channel{Name: "saved", Type: 57, Key: "original-credential"}
	require.NoError(t, model.DB.Create(channel).Error)
	flow := fixture.start(t)
	authorize, err := url.Parse(flow.AuthorizeURL)
	require.NoError(t, err)
	assert.Equal(t, "https://auth.openai.com/oauth/authorize", authorize.Scheme+"://"+authorize.Host+authorize.Path)
	assert.Equal(t, flow.FlowToken, authorize.Query().Get("state"))
	assert.Equal(t, "S256", authorize.Query().Get("code_challenge_method"))
	assert.InDelta(t, 600, flow.ExpiresAt-time.Now().Unix(), 2)
	stored, err := model.GetAuthFlow(flow.FlowToken, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeCodexOAuth})
	require.NoError(t, err)
	assert.NotEqual(t, flow.FlowToken, stored.TokenHash)
	var calls atomic.Int32
	installCodexOAuthTransport(t, "", func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		assert.Equal(t, "https://auth.openai.com/oauth/token", request.URL.String())
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", request.Header.Get("Content-Type"))
		assert.NoError(t, request.ParseForm())
		assert.Equal(t, "private-code", request.Form.Get("code"))
		assert.Equal(t, "authorization_code", request.Form.Get("grant_type"))
		assert.Equal(t, authorize.Query().Get("client_id"), request.Form.Get("client_id"))
		assert.Equal(t, authorize.Query().Get("redirect_uri"), request.Form.Get("redirect_uri"))
		verifier := request.Form.Get("code_verifier")
		assert.GreaterOrEqual(t, len(verifier), 43)
		assert.LessOrEqual(t, len(verifier), 128)
		assert.NotContains(t, stored.Payload, verifier)
		digest := sha256.Sum256([]byte(verifier))
		assert.Equal(t, authorize.Query().Get("code_challenge"), base64.RawURLEncoding.EncodeToString(digest[:]))
		assert.NotContains(t, flow.AuthorizeURL, verifier)
		_, hasDeadline := request.Context().Deadline()
		assert.True(t, hasDeadline)
		return codexOAuthTokenResponse(request), nil
	})
	body := gin.H{"flow_token": flow.FlowToken, "input": codexOAuthCallback(flow.FlowToken)}
	response := fixture.request(t, "complete", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	var result struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	var key service.CodexOAuthKey
	require.NoError(t, common.UnmarshalJsonStr(result.Data.Key, &key))
	assert.Equal(t, "account-123", key.AccountID)
	assert.Equal(t, "codex@example.com", key.Email)
	assert.Equal(t, "codex", key.Type)
	assert.Equal(t, "private-refresh-token", key.RefreshToken)
	expiresAt, err := time.Parse(time.RFC3339, key.Expired)
	require.NoError(t, err)
	assert.InDelta(t, 3600, time.Until(expiresAt).Seconds(), 2)
	require.NoError(t, model.DB.First(channel, channel.Id).Error)
	assert.Equal(t, "original-credential", channel.Key)
	require.NoError(t, model.DB.First(stored, stored.Id).Error)
	assert.Empty(t, stored.Payload)
	assert.NotNil(t, stored.ConsumedAt)
	assert.Equal(t, http.StatusConflict, fixture.request(t, "complete", body).Code)
	assert.EqualValues(t, 1, calls.Load())
	var audits []model.AuditLog
	require.NoError(t, model.LOG_DB.Find(&audits).Error)
	require.NotEmpty(t, audits)
	auditBytes, err := common.Marshal(audits)
	require.NoError(t, err)
	for _, secret := range []string{flow.FlowToken, "private-code", key.AccessToken, key.RefreshToken} {
		assert.NotContains(t, string(auditBytes), secret)
	}
}

func TestCodexOAuthRejectsMalformedCallbacksWithoutSpendingFlow(t *testing.T) {
	fixture := setupCodexOAuthTest(t)
	flow := fixture.start(t)
	valid := codexOAuthCallback(flow.FlowToken)
	for _, input := range []string{
		"private-code", "", "https://example.com/?code=x&state=" + flow.FlowToken,
		strings.Replace(valid, "localhost:1455", "localhost:1456", 1),
		strings.Replace(valid, "/auth/callback", "/callback", 1),
		strings.Replace(valid, "/auth/callback", "/auth/%63allback", 1),
		valid + "&code=other", valid + "&state=other", valid + "#fragment",
		strings.Replace(valid, "localhost", "attacker@localhost", 1),
		strings.Replace(valid, "private-code", "", 1), valid + "&error=access_denied",
		valid + "&extra=%ZZ", strings.Repeat("x", 8193),
	} {
		response := fixture.request(t, "complete", gin.H{"flow_token": flow.FlowToken, "input": input})
		assert.Equal(t, http.StatusBadRequest, response.Code)
	}
	response := fixture.request(t, "complete", gin.H{"flow_token": flow.FlowToken, "input": valid, "proxy": "file:///private-secret"})
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.NotContains(t, response.Body.String(), "private-secret")
	_, err := model.GetAuthFlow(flow.FlowToken, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeCodexOAuth})
	assert.NoError(t, err)
}

func TestCodexOAuthRejectsExpiredAndMismatchedFlows(t *testing.T) {
	for _, scenario := range []string{"state", "user", "session", "version", "expired", "purpose", "revoked", "PAT"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := setupCodexOAuthTest(t)
			flow := fixture.start(t)
			identity := fixture.identity
			input := codexOAuthCallback(flow.FlowToken)
			switch scenario {
			case "state":
				input = codexOAuthCallback(strings.Repeat("x", 43))
			case "user":
				identity.UserID++
			case "session":
				identity.SessionID = "another-session"
			case "version":
				identity.SessionVersion++
			case "expired":
				require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("purpose = ?", model.AuthFlowPurposeCodexOAuth).Update("expires_at", time.Now().Add(-time.Second)).Error)
			case "purpose":
				require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("purpose = ?", model.AuthFlowPurposeCodexOAuth).Update("purpose", model.AuthFlowPurposeOAuth).Error)
			case "revoked":
				_, err := model.RevokeUserSession(identity.UserID, identity.SessionID, "test")
				require.NoError(t, err)
			case "PAT":
				identity.SessionID = model.AccessTokenSessionID(1)
			}
			var calls atomic.Int32
			installCodexOAuthTransport(t, "", func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return codexOAuthTokenResponse(request), nil
			})
			key, err := service.CompleteCodexOAuthAuthorization(t.Context(), identity, flow.FlowToken, input, "")
			assert.Error(t, err)
			assert.Nil(t, key)
			assert.Zero(t, calls.Load())
		})
	}
}

func TestCodexOAuthConsumesFailedExchangesAndRejectsRedirects(t *testing.T) {
	for _, scenario := range []string{"transport", "redirect", "provider error", "invalid JSON", "missing tokens", "missing account", "invalid expiry", "revoked during exchange", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := setupCodexOAuthTest(t)
			flow := fixture.start(t)
			var calls atomic.Int32
			installCodexOAuthTransport(t, "", func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				response := codexOAuthTokenResponse(request)
				switch scenario {
				case "transport":
					return nil, errors.New("private-code private-refresh-token")
				case "redirect":
					response.StatusCode = http.StatusTemporaryRedirect
					response.Header.Set("Location", "https://attacker.example/")
				case "provider error":
					response.StatusCode = http.StatusBadRequest
				case "invalid JSON":
					response.Body = io.NopCloser(strings.NewReader("private-code not-json"))
				case "missing tokens":
					response.Body = io.NopCloser(strings.NewReader(`{"access_token":"private-token","expires_in":10}`))
				case "missing account":
					response.Body = io.NopCloser(strings.NewReader(`{"access_token":"private-token","refresh_token":"private-refresh-token","expires_in":10}`))
				case "invalid expiry":
					response.Body = io.NopCloser(strings.NewReader(`{"access_token":"private-token","refresh_token":"private-refresh-token","expires_in":9223372036854775807}`))
				case "revoked during exchange":
					_, err := model.RevokeUserSession(fixture.identity.UserID, fixture.identity.SessionID, "test")
					assert.NoError(t, err)
				}
				return response, nil
			})
			input := codexOAuthCallback(flow.FlowToken)
			if scenario == "denied" {
				input = "http://localhost:1455/auth/callback?error=access_denied&state=" + flow.FlowToken
			}
			response := fixture.request(t, "complete", gin.H{"flow_token": flow.FlowToken, "input": input})
			assert.GreaterOrEqual(t, response.Code, 400)
			assert.NotContains(t, response.Body.String(), "private-code")
			assert.NotContains(t, response.Body.String(), "private-refresh-token")
			_, err := model.GetAuthFlow(flow.FlowToken, model.AuthFlowMatch{Purpose: model.AuthFlowPurposeCodexOAuth})
			assert.ErrorIs(t, err, model.ErrAuthFlowConsumed)
			if scenario == "denied" {
				assert.Zero(t, calls.Load())
			} else {
				assert.EqualValues(t, 1, calls.Load())
			}
		})
	}
}

func TestCodexOAuthUsesProxyAndConsumesConcurrentRequestsOnce(t *testing.T) {
	fixture := setupCodexOAuthTest(t)
	flow := fixture.start(t)
	proxy := "http://proxy-user:proxy-secret@127.0.0.1:31991"
	var calls atomic.Int32
	installCodexOAuthTransport(t, "", func(*http.Request) (*http.Response, error) { return nil, errors.New("must use proxy") })
	installCodexOAuthTransport(t, proxy, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return codexOAuthTokenResponse(request), nil
	})
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			_, err := service.CompleteCodexOAuthAuthorization(t.Context(), fixture.identity, flow.FlowToken, codexOAuthCallback(flow.FlowToken), proxy)
			errors <- err
		})
	}
	close(start)
	workers.Wait()
	first, second := <-errors, <-errors
	if first == nil {
		assert.ErrorIs(t, second, model.ErrAuthFlowConsumed)
	} else {
		assert.ErrorIs(t, first, model.ErrAuthFlowConsumed)
		assert.NoError(t, second)
	}
	assert.EqualValues(t, 1, calls.Load())
}

func TestCodexOAuthRequiresDashboardSessionAndSensitivePermission(t *testing.T) {
	fixture := setupCodexOAuthTest(t)
	for _, action := range []string{"start", "complete"} {
		anonymous := fixture
		anonymous.token = ""
		assert.Equal(t, http.StatusUnauthorized, anonymous.request(t, action, gin.H{}).Code)
	}
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", fixture.identity.UserID).Update("role", common.RoleAdminUser).Error)
	require.NoError(t, model.PublishUserAuthCache(fixture.identity.UserID))
	for _, action := range []string{"start", "complete"} {
		assert.Equal(t, http.StatusForbidden, fixture.request(t, action, gin.H{}).Code)
	}
	// A token-bound identity must also be rejected if it reaches a controller.
	pat := fixture.identity
	pat.SessionID = model.AccessTokenSessionID(1)
	for _, handler := range []gin.HandlerFunc{StartCodexOAuth, CompleteCodexOAuth} {
		response := securityEnrollmentRequest(http.MethodPost, "/api/channel/codex/oauth/start", `{}`, "", pat, handler)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	}
}

func TestCodexOAuthVerifiesTLSDespiteInsecureRelaySetting(t *testing.T) {
	fixture := setupCodexOAuthTest(t)
	flow := fixture.start(t)
	previous := common.TLSInsecureSkipVerify
	common.TLSInsecureSkipVerify = true
	t.Cleanup(func() { common.TLSInsecureSkipVerify = previous })
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(certificateServer.Close)
	var receivedCredentials atomic.Int32
	var proxyConnections atomic.Int32
	var rejectedTLS atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxyConnections.Add(1)
		assert.Equal(t, http.MethodConnect, request.Method)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if !assert.NoError(t, err) {
			return
		}
		defer connection.Close()
		_, err = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if !assert.NoError(t, err) {
			return
		}
		tlsConnection := tls.Server(connection, certificateServer.TLS)
		defer tlsConnection.Close()
		if err := tlsConnection.Handshake(); err != nil {
			rejectedTLS.Add(1)
			return
		}
		if _, err := http.ReadRequest(bufio.NewReader(tlsConnection)); err == nil {
			receivedCredentials.Add(1)
			_, _ = io.WriteString(tlsConnection, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}
	}))
	t.Cleanup(proxy.Close)
	response := fixture.request(t, "complete", gin.H{"flow_token": flow.FlowToken, "input": codexOAuthCallback(flow.FlowToken), "proxy": proxy.URL})
	assert.Equal(t, http.StatusBadGateway, response.Code)
	assert.EqualValues(t, 1, proxyConnections.Load())
	assert.EqualValues(t, 1, rejectedTLS.Load())
	assert.Zero(t, receivedCredentials.Load(), "untrusted TLS peers must never receive the code or verifier")
}
