package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

type codexOAuthFlowPayload struct {
	Verifier string                    `json:"verifier"`
	Proxy    string                    `json:"proxy,omitempty"`
	Identity model.AuthSessionIdentity `json:"identity"`
}

func StartCodexOAuth(c *gin.Context) {
	identity, ok := requireBrowserSession(c)
	if !ok {
		return
	}
	var request struct {
		Proxy string `json:"proxy"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10), &request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "CODEX_OAUTH_START_FAILED", "message": "Invalid authorization request."})
		return
	}
	request.Proxy = strings.TrimSpace(request.Proxy)
	if request.Proxy != "" {
		proxyURL, _, err := common.ParseProxyURLRuntime(request.Proxy)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "CODEX_OAUTH_PROXY_INVALID", "message": "Invalid proxy URL."})
			return
		}
		request.Proxy = proxyURL.String()
	}
	verifier := oauth2.GenerateVerifier()
	payload, err := common.Marshal(codexOAuthFlowPayload{Verifier: verifier, Proxy: request.Proxy, Identity: identity})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "code": "CODEX_OAUTH_START_FAILED", "message": "Failed to start Codex authorization. Please try again."})
		return
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
		// Database errors may contain the flow payload, including the verifier.
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "code": "CODEX_OAUTH_START_FAILED", "message": "Failed to start Codex authorization. Please try again."})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"authorize_url": service.CodexOAuthAuthorizationURL(state, verifier),
		"expires_at":    expiresAt.Unix(),
	}})
}

func parseCodexOAuthCallback(input string) (string, string, error) {
	callback, err := url.Parse(strings.TrimSpace(input))
	if err != nil {
		return "", "", model.ErrAuthFlowInvalid
	}
	if callback.Scheme+"://"+callback.Host+callback.EscapedPath() != service.CodexOAuthRedirectURI || callback.User != nil || callback.Fragment != "" || callback.Opaque != "" {
		return "", "", model.ErrAuthFlowInvalid
	}
	query, err := url.ParseQuery(callback.RawQuery)
	if err != nil || query.Has("error") || len(query["code"]) != 1 || len(query["state"]) != 1 {
		return "", "", model.ErrAuthFlowInvalid
	}
	code, state := query.Get("code"), query.Get("state")
	if code == "" || strings.TrimSpace(code) != code || state == "" || strings.TrimSpace(state) != state {
		return "", "", model.ErrAuthFlowInvalid
	}
	return code, state, nil
}

func CompleteCodexOAuth(c *gin.Context) {
	identity, ok := requireBrowserSession(c)
	if !ok {
		return
	}
	var request struct {
		Input string `json:"input"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10), &request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "CODEX_OAUTH_CALLBACK_INVALID", "message": "Paste the complete callback URL with code and state."})
		return
	}
	code, state, err := parseCodexOAuthCallback(request.Input)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "CODEX_OAUTH_CALLBACK_INVALID", "message": "Paste the complete callback URL with code and state."})
		return
	}
	var payload codexOAuthFlowPayload
	_, err = model.ConsumeAuthFlowWithAction(state, model.AuthFlowMatch{
		Purpose:   model.AuthFlowPurposeCodexOAuth,
		UserId:    identity.UserID,
		SessionId: identity.SessionID,
	}, func(tx *gorm.DB, flow *model.AuthFlow) error {
		if err := common.UnmarshalJsonStr(flow.Payload, &payload); err != nil {
			return model.ErrAuthFlowInvalid
		}
		if payload.Identity != identity || payload.Verifier == "" {
			return model.ErrAuthFlowInvalid
		}
		if err := model.ValidateAuthSessionWithTx(tx, payload.Identity); err != nil {
			return err
		}
		return tx.Model(flow).Update("payload", "").Error
	})
	if err != nil {
		status := http.StatusBadRequest
		if !errors.Is(err, model.ErrAuthFlowInvalid) && !errors.Is(err, model.ErrAuthFlowExpired) && !errors.Is(err, model.ErrAuthFlowConsumed) && !errors.Is(err, model.ErrUserSessionInactive) && !errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"success": false, "code": "CODEX_OAUTH_FLOW_INVALID", "message": "Codex authorization expired or was already used. Start again."})
		return
	}
	// Consume before contacting OpenAI: retries and concurrent requests cannot
	// exchange the same code twice, even if the upstream response is lost.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	tokens, err := service.ExchangeCodexAuthorizationCodeWithProxy(ctx, code, payload.Verifier, payload.Proxy)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "code": "CODEX_OAUTH_EXCHANGE_FAILED", "message": "Codex authorization failed. Start again."})
		return
	}
	accountID, ok := service.ExtractCodexAccountIDFromJWT(tokens.AccessToken)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "code": "CODEX_OAUTH_EXCHANGE_FAILED", "message": "Codex authorization failed. Start again."})
		return
	}
	// Do not release credentials if the session was revoked during the exchange.
	if err := model.DB.Transaction(func(tx *gorm.DB) error {
		return model.ValidateAuthSessionWithTx(tx, payload.Identity)
	}); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "code": "CODEX_OAUTH_FLOW_INVALID", "message": "Codex authorization expired or was already used. Start again."})
		return
	}
	email, _ := service.ExtractEmailFromJWT(tokens.AccessToken)
	key := service.CodexOAuthKey{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Type:         "codex",
		LastRefresh:  time.Now().Format(time.RFC3339),
		Expired:      tokens.ExpiresAt.Format(time.RFC3339),
	}
	encoded, err := common.Marshal(key)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "code": "CODEX_OAUTH_EXCHANGE_FAILED", "message": "Codex authorization failed. Start again."})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"key":          string(encoded),
		"account_id":   accountID,
		"email":        email,
		"expires_at":   key.Expired,
		"last_refresh": key.LastRefresh,
	}})
}
