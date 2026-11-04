package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func StartCodexOAuth(c *gin.Context) {
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		writeCodexOAuthError(c, service.ErrLoginSessionInvalid)
		return
	}
	flow, err := service.StartCodexOAuthAuthorization(identity)
	if err != nil {
		writeCodexOAuthError(c, err)
		return
	}
	common.ApiSuccess(c, flow)
}

func CompleteCodexOAuth(c *gin.Context) {
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		writeCodexOAuthError(c, service.ErrLoginSessionInvalid)
		return
	}
	var request struct {
		FlowToken string `json:"flow_token"`
		Input     string `json:"input"`
		Proxy     string `json:"proxy"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		writeCodexOAuthError(c, service.ErrCodexOAuthInput)
		return
	}
	key, err := service.CompleteCodexOAuthAuthorization(c.Request.Context(), identity, request.FlowToken, request.Input, request.Proxy)
	if err != nil {
		writeCodexOAuthError(c, err)
		return
	}
	encoded, err := common.Marshal(key)
	if err != nil {
		writeCodexOAuthError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"key":          string(encoded),
		"account_id":   key.AccountID,
		"email":        key.Email,
		"expires_at":   key.Expired,
		"last_refresh": key.LastRefresh,
	})
}

// Never expose raw provider, transport or database errors: they may contain
// authorization codes, tokens, proxy credentials or stored PKCE payloads.
func writeCodexOAuthError(c *gin.Context, err error) {
	status, code, message := http.StatusBadGateway, "CODEX_OAUTH_FAILED", service.ErrCodexOAuthExchange.Error()
	switch {
	case errors.Is(err, service.ErrLoginSessionInvalid), errors.Is(err, service.ErrLoginSessionRevoked), errors.Is(err, model.ErrUserSessionInactive), errors.Is(err, service.ErrAuthTokenInvalid):
		status, code, message = http.StatusUnauthorized, "CODEX_OAUTH_SESSION_REQUIRED", "A dashboard login session is required."
	case errors.Is(err, service.ErrCodexOAuthInput):
		status, code, message = http.StatusBadRequest, "CODEX_OAUTH_INPUT_INVALID", service.ErrCodexOAuthInput.Error()
	case errors.Is(err, service.ErrCodexOAuthProxy):
		status, code, message = http.StatusBadRequest, "CODEX_OAUTH_PROXY_INVALID", service.ErrCodexOAuthProxy.Error()
	case errors.Is(err, model.ErrAuthFlowInvalid), errors.Is(err, model.ErrAuthFlowExpired), errors.Is(err, model.ErrAuthFlowConsumed):
		status, code, message = http.StatusConflict, "CODEX_OAUTH_FLOW_INVALID", "The Codex authorization request is invalid, expired or already used. Start again."
	}
	common.ApiErrorStatus(c, status, common.NewMessage(message), gin.H{"code": code})
}
