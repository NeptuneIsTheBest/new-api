package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	codexOAuthFlowTTL                  = 10 * time.Minute
	codexOAuthExchangeTimeout          = 15 * time.Second
	maxCodexAuthorizationInputLength   = 8192
	codexOAuthSessionRequiredMessage   = "A browser login session is required"
	codexOAuthStartFailedMessage       = "Failed to start Codex authorization"
	codexOAuthCallbackInvalidMessage   = "The authorization callback is invalid"
	codexOAuthFlowInvalidMessage       = "The Codex authorization flow is invalid or expired"
	codexOAuthExchangeFailedMessage    = "Failed to exchange the authorization code"
	codexOAuthAccountMissingMessage    = "The Codex credential is missing an account ID"
	codexOAuthChannelLoadFailedMessage = "The selected channel could not be loaded"
	codexOAuthChannelTypeMessage       = "The selected channel is not a Codex channel"
)

var errCodexOAuthChannelType = errors.New("channel type is not Codex")

type codexOAuthStartRequest struct {
	ChannelID *int `json:"channel_id,omitempty"`
}

type codexOAuthCompleteRequest struct {
	Input string `json:"input"`
}

type codexOAuthFlowPayload struct {
	Verifier  string `json:"verifier"`
	ChannelID int    `json:"channel_id,omitempty"`
}

type codexOAuthExchangeFunc func(context.Context, string, string, string) (*service.CodexOAuthTokenResult, error)

func StartCodexOAuth(c *gin.Context) {
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthSessionRequiredMessage})
		return
	}

	var request codexOAuthStartRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthStartFailedMessage})
		return
	}

	channelID := 0
	if request.ChannelID != nil {
		channelID = *request.ChannelID
		if _, err := loadCodexOAuthChannel(channelID); err != nil {
			message := codexOAuthChannelLoadFailedMessage
			if errors.Is(err, errCodexOAuthChannelType) {
				message = codexOAuthChannelTypeMessage
			}
			c.JSON(http.StatusOK, gin.H{"success": false, "message": message})
			return
		}
	}

	verifier, challenge, err := service.GenerateCodexOAuthPKCEPair()
	if err != nil {
		common.SysError("failed to generate Codex OAuth PKCE pair: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthStartFailedMessage})
		return
	}
	payload, err := common.Marshal(codexOAuthFlowPayload{Verifier: verifier, ChannelID: channelID})
	if err != nil {
		common.SysError("failed to encode Codex OAuth flow: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthStartFailedMessage})
		return
	}

	expiresAt := time.Now().Add(codexOAuthFlowTTL)
	state, _, err := model.CreateAuthFlow(model.AuthFlowCreate{
		Purpose:   model.AuthFlowPurposeCodexOAuth,
		UserId:    identity.UserID,
		SessionId: identity.SessionID,
		Payload:   string(payload),
		ExpiresAt: expiresAt,
	})
	if err != nil {
		common.SysError("failed to create Codex OAuth flow: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthStartFailedMessage})
		return
	}

	authorizeURL, err := service.BuildCodexOAuthAuthorizeURL(state, challenge)
	if err != nil {
		common.SysError("failed to build Codex OAuth authorize URL: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthStartFailedMessage})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"authorize_url": authorizeURL,
			"expires_at":    expiresAt.Unix(),
		},
	})
}

func CompleteCodexOAuth(c *gin.Context) {
	completeCodexOAuth(c, service.ExchangeCodexAuthorizationCodeWithProxy)
}

func completeCodexOAuth(c *gin.Context, exchange codexOAuthExchangeFunc) {
	identity, ok := middleware.GetSessionAuthIdentity(c)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthSessionRequiredMessage})
		return
	}

	var request codexOAuthCompleteRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthCallbackInvalidMessage})
		return
	}
	code, state, err := parseCodexAuthorizationInput(request.Input)
	if err != nil || code == "" || state == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthCallbackInvalidMessage})
		return
	}

	flowMatch := model.AuthFlowMatch{
		Purpose:   model.AuthFlowPurposeCodexOAuth,
		UserId:    identity.UserID,
		SessionId: identity.SessionID,
	}
	flow, err := model.GetAuthFlow(state, flowMatch)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthFlowInvalidMessage})
		return
	}
	var payload codexOAuthFlowPayload
	if err := common.UnmarshalJsonStr(flow.Payload, &payload); err != nil || strings.TrimSpace(payload.Verifier) == "" {
		common.SysError("failed to decode Codex OAuth flow")
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthFlowInvalidMessage})
		return
	}

	proxyURL := ""
	if payload.ChannelID > 0 {
		channel, err := loadCodexOAuthChannel(payload.ChannelID)
		if err != nil {
			message := codexOAuthChannelLoadFailedMessage
			if errors.Is(err, errCodexOAuthChannelType) {
				message = codexOAuthChannelTypeMessage
			}
			c.JSON(http.StatusOK, gin.H{"success": false, "message": message})
			return
		}
		proxyURL = channel.GetSetting().Proxy
	}

	exchangeContext, cancel := context.WithTimeout(c.Request.Context(), codexOAuthExchangeTimeout)
	defer cancel()
	tokenResult, err := exchange(exchangeContext, code, payload.Verifier, proxyURL)
	if err != nil {
		common.SysError("failed to exchange Codex authorization code: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthExchangeFailedMessage})
		return
	}
	if tokenResult == nil {
		common.SysError("failed to exchange Codex authorization code: empty token response")
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthExchangeFailedMessage})
		return
	}

	accountID, ok := service.ExtractCodexAccountIDFromJWT(tokenResult.IDToken)
	if !ok {
		accountID, ok = service.ExtractCodexAccountIDFromJWT(tokenResult.AccessToken)
	}
	if !ok {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthAccountMissingMessage})
		return
	}
	email, ok := service.ExtractEmailFromJWT(tokenResult.IDToken)
	if !ok {
		email, _ = service.ExtractEmailFromJWT(tokenResult.AccessToken)
	}

	now := time.Now()
	key := service.CodexOAuthKey{
		IDToken:      tokenResult.IDToken,
		AccessToken:  tokenResult.AccessToken,
		RefreshToken: tokenResult.RefreshToken,
		AccountID:    accountID,
		LastRefresh:  now.Format(time.RFC3339),
		Email:        email,
		Type:         "codex",
		Expired:      tokenResult.ExpiresAt.Format(time.RFC3339),
	}
	encoded, err := common.Marshal(key)
	if err != nil {
		common.SysError("failed to encode Codex OAuth credential: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthExchangeFailedMessage})
		return
	}

	if _, err := model.ConsumeAuthFlow(state, flowMatch); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexOAuthFlowInvalidMessage})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"key":          string(encoded),
			"account_id":   accountID,
			"email":        email,
			"expires_at":   key.Expired,
			"last_refresh": key.LastRefresh,
		},
	})
}

func parseCodexAuthorizationInput(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" || len(input) > maxCodexAuthorizationInputLength {
		return "", "", errors.New("invalid authorization input")
	}

	if callbackURL, err := url.Parse(input); err == nil {
		query := callbackURL.Query()
		code := strings.TrimSpace(query.Get("code"))
		state := strings.TrimSpace(query.Get("state"))
		if code != "" || state != "" {
			return code, state, nil
		}
	}
	if query, err := url.ParseQuery(strings.TrimPrefix(input, "?")); err == nil {
		code := strings.TrimSpace(query.Get("code"))
		state := strings.TrimSpace(query.Get("state"))
		if code != "" || state != "" {
			return code, state, nil
		}
	}
	if strings.Contains(input, "#") {
		parts := strings.SplitN(input, "#", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
	}
	return "", "", errors.New("authorization state is missing")
}

func loadCodexOAuthChannel(channelID int) (*model.Channel, error) {
	if channelID <= 0 {
		return nil, errors.New("invalid channel id")
	}
	channel, err := model.GetChannelById(channelID, false)
	if err != nil {
		return nil, fmt.Errorf("load channel: %w", err)
	}
	if channel.Type != constant.ChannelTypeCodex {
		return nil, errCodexOAuthChannelType
	}
	return channel, nil
}
