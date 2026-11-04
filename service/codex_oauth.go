package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

const (
	codexOAuthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthTokenURL    = "https://auth.openai.com/oauth/token"
	codexOAuthRedirectURI = "http://localhost:1455/auth/callback"
	codexJWTClaimPath     = "https://api.openai.com/auth"
	defaultHTTPTimeout    = 20 * time.Second
)

var (
	ErrCodexOAuthInput    = errors.New("Paste the full Codex callback URL with code and state.")
	ErrCodexOAuthProxy    = errors.New("Invalid proxy URL")
	ErrCodexOAuthExchange = errors.New("Codex authorization failed. Start a new authorization and try again.")
)

type CodexOAuthAuthorization struct {
	AuthorizeURL string `json:"authorize_url"`
	FlowToken    string `json:"flow_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

type codexOAuthFlowPayload struct {
	Identity AuthIdentity `json:"identity"`
}

// Derive a unique verifier using a server-only key and the random flow token.
// Unlike storing the verifier in Payload, this cannot leak it through SQL logs.
func codexOAuthVerifier(flowToken string) string {
	return common.GenerateHMACWithKey(authSigningKey("codex-oauth-pkce"), flowToken)
}

// StartCodexOAuthAuthorization binds the PKCE secret to a live dashboard session.
// This is a channel credential flow, not a new-api login or account binding.
func StartCodexOAuthAuthorization(identity AuthIdentity) (*CodexOAuthAuthorization, error) {
	if _, isPAT := model.ParseAccessTokenSessionID(identity.SessionID); isPAT {
		return nil, ErrLoginSessionInvalid
	}
	if _, _, err := ValidateLoginSession(identity); err != nil {
		return nil, err
	}
	payload, err := common.Marshal(codexOAuthFlowPayload{Identity: identity})
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(10 * time.Minute)
	state, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeCodexOAuth,
		UserId:    identity.UserID,
		SessionId: identity.SessionID,
		Payload:   string(payload),
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, err
	}
	challenge := sha256.Sum256([]byte(codexOAuthVerifier(state)))
	query := url.Values{
		"response_type":              {"code"},
		"client_id":                  {codexOAuthClientID},
		"redirect_uri":               {codexOAuthRedirectURI},
		"scope":                      {"openid profile email offline_access"},
		"state":                      {state},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"codex_cli_rs"},
	}
	return &CodexOAuthAuthorization{
		AuthorizeURL: "https://auth.openai.com/oauth/authorize?" + query.Encode(),
		FlowToken:    state,
		ExpiresAt:    expiresAt.Unix(),
	}, nil
}

// CompleteCodexOAuthAuthorization returns a draft credential without updating a
// channel. Consumption commits before the network call, including on failure.
func CompleteCodexOAuthAuthorization(ctx context.Context, identity AuthIdentity, flowToken, input, proxyURL string) (*CodexOAuthKey, error) {
	if _, isPAT := model.ParseAccessTokenSessionID(identity.SessionID); isPAT || identity.UserID <= 0 || identity.SessionID == "" {
		return nil, ErrLoginSessionInvalid
	}
	callback, err := url.Parse(strings.TrimSpace(input))
	if err != nil || len(input) > 8192 || callback.Scheme != "http" || callback.Host != "localhost:1455" ||
		callback.Path != "/auth/callback" || callback.RawPath != "" || callback.User != nil || callback.Fragment != "" {
		return nil, ErrCodexOAuthInput
	}
	query, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") == "" || len(flowToken) != base64.RawURLEncoding.EncodedLen(model.AuthFlowTokenBytes) {
		return nil, ErrCodexOAuthInput
	}
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(flowToken)) != 1 {
		return nil, model.ErrAuthFlowInvalid
	}
	denied := len(query["error"]) == 1 && query.Get("error") != ""
	code := query.Get("code")
	if (denied && len(query["code"]) != 0) || (!denied && (len(query["error"]) != 0 || len(query["code"]) != 1 || strings.TrimSpace(code) == "")) {
		return nil, ErrCodexOAuthInput
	}
	if len(proxyURL) > 4096 {
		return nil, ErrCodexOAuthProxy
	}
	proxy, err := common.ParseProxyURLStrict(proxyURL)
	if err != nil {
		return nil, ErrCodexOAuthProxy
	}
	proxyURL = ""
	if proxy != nil {
		proxyURL = proxy.String()
	}
	var payload codexOAuthFlowPayload
	_, err = model.ConsumeAuthFlowWithAction(flowToken, model.AuthFlowMatch{
		Purpose:   model.AuthFlowPurposeCodexOAuth,
		UserId:    identity.UserID,
		SessionId: identity.SessionID,
	}, func(tx *gorm.DB, flow *model.AuthFlow) error {
		if err := common.UnmarshalJsonStr(flow.Payload, &payload); err != nil || payload.Identity != identity {
			return model.ErrAuthFlowInvalid
		}
		if err := model.ValidateAuthSessionWithTx(tx, identity); err != nil {
			return err
		}
		// Retain only the audit metadata after consuming the flow.
		return tx.Model(flow).Update("payload", "").Error
	})
	if err != nil {
		return nil, err
	}
	if denied {
		return nil, ErrCodexOAuthExchange
	}
	client, err := getCodexOAuthHTTPClient(proxyURL)
	if err != nil {
		return nil, ErrCodexOAuthExchange
	}
	if common.TLSInsecureSkipVerify {
		// Relay transport settings must not disable verification for OAuth secrets.
		client, err = newHTTPClientFromPolicy(defaultHTTPTransportPolicy(), proxy, &tls.Config{MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, ErrCodexOAuthExchange
		}
		defer client.CloseIdleConnections()
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	exchangeCtx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexOAuthClientID},
		"code":          {code},
		"code_verifier": {codexOAuthVerifier(flowToken)},
		"redirect_uri":  {codexOAuthRedirectURI},
	}
	req, err := http.NewRequestWithContext(exchangeCtx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, ErrCodexOAuthExchange
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, ErrCodexOAuthExchange
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrCodexOAuthExchange
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := common.DecodeJson(io.LimitReader(response.Body, 1<<20), &tokens); err != nil ||
		strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" ||
		tokens.ExpiresIn <= 0 || tokens.ExpiresIn > math.MaxInt64/int64(time.Second) {
		return nil, ErrCodexOAuthExchange
	}
	// These claims are metadata from the fixed TLS-protected token endpoint;
	// they never establish a new-api identity or grant dashboard permissions.
	accountID, ok := ExtractCodexAccountIDFromJWT(tokens.AccessToken)
	if !ok {
		return nil, ErrCodexOAuthExchange
	}
	email, _ := ExtractEmailFromJWT(tokens.AccessToken)
	// A session revoked while the provider was responding must not receive keys.
	if err := model.DB.Transaction(func(tx *gorm.DB) error {
		return model.ValidateAuthSessionWithTx(tx, identity)
	}); err != nil {
		return nil, err
	}
	now := time.Now()
	return &CodexOAuthKey{
		AccessToken:  strings.TrimSpace(tokens.AccessToken),
		RefreshToken: strings.TrimSpace(tokens.RefreshToken),
		AccountID:    accountID,
		Email:        email,
		Type:         "codex",
		LastRefresh:  now.Format(time.RFC3339),
		Expired:      now.Add(time.Duration(tokens.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

type CodexOAuthTokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
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

	if strings.TrimSpace(payload.AccessToken) == "" || strings.TrimSpace(payload.RefreshToken) == "" || payload.ExpiresIn <= 0 {
		return nil, errors.New("codex oauth refresh response missing fields")
	}

	return &CodexOAuthTokenResult{
		AccessToken:  strings.TrimSpace(payload.AccessToken),
		RefreshToken: strings.TrimSpace(payload.RefreshToken),
		ExpiresAt:    time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
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
	v, ok := claims["email"]
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

func decodeJWTClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := common.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, false
	}
	return claims, true
}
