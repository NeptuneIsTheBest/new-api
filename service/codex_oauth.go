package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	codexOAuthClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL     = "https://auth.openai.com/oauth/token"
	codexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	codexOAuthScope        = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	codexJWTClaimPath      = "https://api.openai.com/auth"
	defaultHTTPTimeout     = 20 * time.Second
)

type CodexOAuthTokenResult struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func GenerateCodexOAuthPKCEPair() (string, string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func BuildCodexOAuthAuthorizeURL(state string, challenge string) (string, error) {
	state = strings.TrimSpace(state)
	challenge = strings.TrimSpace(challenge)
	if state == "" {
		return "", errors.New("empty oauth state")
	}
	if challenge == "" {
		return "", errors.New("empty code_challenge")
	}

	authorizeURL, err := url.Parse(codexOAuthAuthorizeURL)
	if err != nil {
		return "", err
	}
	query := authorizeURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", codexOAuthClientID)
	query.Set("redirect_uri", codexOAuthRedirectURI)
	query.Set("scope", codexOAuthScope)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	query.Set("id_token_add_organizations", "true")
	query.Set("codex_cli_simplified_flow", "true")
	query.Set("originator", "codex_cli_rs")
	authorizeURL.RawQuery = query.Encode()
	return authorizeURL.String(), nil
}

func RefreshCodexOAuthToken(ctx context.Context, refreshToken string) (*CodexOAuthTokenResult, error) {
	return RefreshCodexOAuthTokenWithProxy(ctx, refreshToken, "")
}

func RefreshCodexOAuthTokenWithProxy(ctx context.Context, refreshToken string, proxyURL string) (*CodexOAuthTokenResult, error) {
	client, err := getCodexOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	return refreshCodexOAuthToken(ctx, client, codexOAuthTokenURL, codexOAuthClientID, refreshToken)
}

func ExchangeCodexAuthorizationCode(ctx context.Context, code string, verifier string) (*CodexOAuthTokenResult, error) {
	return ExchangeCodexAuthorizationCodeWithProxy(ctx, code, verifier, "")
}

func ExchangeCodexAuthorizationCodeWithProxy(ctx context.Context, code string, verifier string, proxyURL string) (*CodexOAuthTokenResult, error) {
	client, err := getCodexOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	return exchangeCodexAuthorizationCode(ctx, client, codexOAuthTokenURL, codexOAuthClientID, code, verifier, codexOAuthRedirectURI)
}

func refreshCodexOAuthToken(
	ctx context.Context,
	client *http.Client,
	tokenURL string,
	clientID string,
	refreshToken string,
) (*CodexOAuthTokenResult, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return nil, errors.New("empty refresh_token")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	form.Set("client_id", clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := common.DecodeJson(resp.Body, &payload); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex oauth refresh failed: status=%d", resp.StatusCode)
	}

	accessToken := strings.TrimSpace(payload.AccessToken)
	newRefreshToken := strings.TrimSpace(payload.RefreshToken)
	expiresAt, ok := resolveCodexOAuthExpiration(accessToken, payload.ExpiresIn)
	if accessToken == "" || newRefreshToken == "" || !ok {
		return nil, errors.New("codex oauth refresh response missing fields")
	}

	return &CodexOAuthTokenResult{
		IDToken:      strings.TrimSpace(payload.IDToken),
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

func exchangeCodexAuthorizationCode(
	ctx context.Context,
	client *http.Client,
	tokenURL string,
	clientID string,
	code string,
	verifier string,
	redirectURI string,
) (*CodexOAuthTokenResult, error) {
	code = strings.TrimSpace(code)
	verifier = strings.TrimSpace(verifier)
	if code == "" {
		return nil, errors.New("empty authorization code")
	}
	if verifier == "" {
		return nil, errors.New("empty code_verifier")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := common.DecodeJson(resp.Body, &payload); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex oauth code exchange failed: status=%d", resp.StatusCode)
	}
	accessToken := strings.TrimSpace(payload.AccessToken)
	refreshToken := strings.TrimSpace(payload.RefreshToken)
	expiresAt, ok := resolveCodexOAuthExpiration(accessToken, payload.ExpiresIn)
	if accessToken == "" || refreshToken == "" || !ok {
		return nil, errors.New("codex oauth token response missing fields")
	}

	return &CodexOAuthTokenResult{
		IDToken:      strings.TrimSpace(payload.IDToken),
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

func resolveCodexOAuthExpiration(accessToken string, expiresIn int) (time.Time, bool) {
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if decodeJWTPayload(accessToken, &claims) && claims.ExpiresAt > 0 {
		expiresAt := time.Unix(claims.ExpiresAt, 0)
		return expiresAt, expiresAt.After(time.Now())
	}

	const maxExpiresIn = int((365 * 24 * time.Hour) / time.Second)
	if expiresIn <= 0 || expiresIn > maxExpiresIn {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second), true
}

func getCodexOAuthHTTPClient(proxyURL string) (*http.Client, error) {
	baseClient, err := GetHttpClientWithProxy(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, err
	}
	if baseClient == nil {
		return &http.Client{Timeout: defaultHTTPTimeout}, nil
	}
	clientCopy := *baseClient
	clientCopy.Timeout = defaultHTTPTimeout
	return &clientCopy, nil
}

func ExtractCodexAccountIDFromJWT(token string) (string, bool) {
	claims, ok := decodeJWTClaims(token)
	if !ok {
		return "", false
	}
	raw, ok := claims[codexJWTClaimPath]
	if !ok {
		return "", false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := obj["chatgpt_account_id"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

func ExtractEmailFromJWT(token string) (string, bool) {
	claims, ok := decodeJWTClaims(token)
	if !ok {
		return "", false
	}
	if email, ok := jwtClaimString(claims["email"]); ok {
		return email, true
	}
	profile, ok := claims["https://api.openai.com/profile"].(map[string]any)
	if !ok {
		return "", false
	}
	return jwtClaimString(profile["email"])
}

func decodeJWTClaims(token string) (map[string]any, bool) {
	var claims map[string]any
	if !decodeJWTPayload(token, &claims) {
		return nil, false
	}
	return claims, true
}

func decodeJWTPayload(token string, target any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	if err := common.Unmarshal(payloadRaw, target); err != nil {
		return false
	}
	return true
}

func jwtClaimString(value any) (string, bool) {
	claim, ok := value.(string)
	claim = strings.TrimSpace(claim)
	return claim, ok && claim != ""
}
