package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateCodexOAuthAuthorizationFlowBuildsAuthorizeURL(t *testing.T) {
	flow, err := CreateCodexOAuthAuthorizationFlow()
	require.NoError(t, err)
	require.NotEmpty(t, flow.State)
	require.NotEmpty(t, flow.Verifier)
	require.NotEmpty(t, flow.Challenge)
	require.NotEmpty(t, flow.AuthorizeURL)

	authorizeURL, err := url.Parse(flow.AuthorizeURL)
	require.NoError(t, err)
	query := authorizeURL.Query()

	assert.Equal(t, "https", authorizeURL.Scheme)
	assert.Equal(t, "auth.openai.com", authorizeURL.Host)
	assert.Equal(t, "/oauth/authorize", authorizeURL.Path)
	assert.Equal(t, "code", query.Get("response_type"))
	assert.Equal(t, codexOAuthClientID, query.Get("client_id"))
	assert.Equal(t, codexOAuthRedirectURI, query.Get("redirect_uri"))
	assert.Equal(t, codexOAuthScope, query.Get("scope"))
	assert.Equal(t, flow.State, query.Get("state"))
	assert.Equal(t, flow.Challenge, query.Get("code_challenge"))
	assert.Equal(t, "S256", query.Get("code_challenge_method"))
	assert.Equal(t, "true", query.Get("id_token_add_organizations"))
	assert.Equal(t, "true", query.Get("codex_cli_simplified_flow"))
	assert.Equal(t, "codex_cli_rs", query.Get("originator"))
}

func TestExchangeCodexAuthorizationCodeSendsPKCEAndParsesResponse(t *testing.T) {
	var receivedForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		require.NoError(t, r.ParseForm())
		receivedForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"access-token","refresh_token":"refresh-token","expires_in":3600}`))
	}))
	defer server.Close()

	result, err := exchangeCodexAuthorizationCode(
		context.Background(),
		server.Client(),
		server.URL,
		"client-id",
		" code-value ",
		" verifier-value ",
		"http://localhost/callback",
	)
	require.NoError(t, err)

	assert.Equal(t, "authorization_code", receivedForm.Get("grant_type"))
	assert.Equal(t, "client-id", receivedForm.Get("client_id"))
	assert.Equal(t, "code-value", receivedForm.Get("code"))
	assert.Equal(t, "verifier-value", receivedForm.Get("code_verifier"))
	assert.Equal(t, "http://localhost/callback", receivedForm.Get("redirect_uri"))
	assert.Equal(t, "access-token", result.AccessToken)
	assert.Equal(t, "refresh-token", result.RefreshToken)
	assert.False(t, result.ExpiresAt.IsZero())
}

func TestExchangeCodexAuthorizationCodeRejectsEmptyInput(t *testing.T) {
	_, err := exchangeCodexAuthorizationCode(
		context.Background(),
		http.DefaultClient,
		"https://example.invalid/token",
		"client-id",
		" ",
		"verifier",
		"http://localhost/callback",
	)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "authorization code"))

	_, err = exchangeCodexAuthorizationCode(
		context.Background(),
		http.DefaultClient,
		"https://example.invalid/token",
		"client-id",
		"code",
		" ",
		"http://localhost/callback",
	)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "code_verifier"))
}
