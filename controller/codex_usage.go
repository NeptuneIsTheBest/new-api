package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type codexWhamFetchFunc func(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	accessToken string,
	accountID string,
) (statusCode int, body []byte, err error)

var (
	codexWhamUsageFetcher              = service.FetchCodexWhamUsage
	codexWhamResetCreditsFetcher       = service.FetchCodexWhamRateLimitResetCredits
	codexWhamUsageResetter             = service.ResetCodexWhamUsage
	codexOAuthTokenRefresher           = service.RefreshCodexOAuthTokenWithProxy
	codexRedeemRequestIDGenerator      = uuid.NewString
	codexWhamUpstreamRequestTimeout    = 15 * time.Second
	codexWhamCredentialRefreshTimeout  = 10 * time.Second
	codexWhamOriginCredentialErrorText = "解析凭证失败，请检查渠道配置"
)

func loadCodexChannelCredential(c *gin.Context) (*model.Channel, *codex.OAuthKey, string, string, bool) {
	channelId, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, fmt.Errorf("invalid channel id: %w", err))
		return nil, nil, "", "", false
	}

	ch, err := model.GetChannelById(channelId, true)
	if err != nil {
		common.ApiError(c, err)
		return nil, nil, "", "", false
	}
	if ch == nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "channel not found"})
		return nil, nil, "", "", false
	}
	if ch.Type != constant.ChannelTypeCodex {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "channel type is not Codex"})
		return nil, nil, "", "", false
	}
	if ch.ChannelInfo.IsMultiKey {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "multi-key channel is not supported"})
		return nil, nil, "", "", false
	}

	oauthKey, err := codex.ParseOAuthKey(strings.TrimSpace(ch.Key))
	if err != nil {
		common.SysError("failed to parse oauth key: " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": codexWhamOriginCredentialErrorText})
		return nil, nil, "", "", false
	}
	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	accountID := strings.TrimSpace(oauthKey.AccountID)
	if accessToken == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "codex channel: access_token is required"})
		return nil, nil, "", "", false
	}
	if accountID == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "codex channel: account_id is required"})
		return nil, nil, "", "", false
	}

	return ch, oauthKey, accessToken, accountID, true
}

func handleCodexWhamProxyRequest(c *gin.Context, actionName string, failureMessage string, fetch codexWhamFetchFunc) {
	ch, oauthKey, accessToken, accountID, ok := loadCodexChannelCredential(c)
	if !ok {
		return
	}

	client, err := service.NewProxyHttpClient(ch.GetSetting().Proxy)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), codexWhamUpstreamRequestTimeout)
	defer cancel()

	statusCode, body, err := fetch(ctx, client, ch.GetBaseURL(), accessToken, accountID)
	if err != nil {
		common.SysError("failed to " + actionName + ": " + err.Error())
		c.JSON(http.StatusOK, gin.H{"success": false, "message": failureMessage})
		return
	}

	if (statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden) && strings.TrimSpace(oauthKey.RefreshToken) != "" {
		refreshCtx, refreshCancel := context.WithTimeout(c.Request.Context(), codexWhamCredentialRefreshTimeout)
		defer refreshCancel()

		res, refreshErr := codexOAuthTokenRefresher(refreshCtx, oauthKey.RefreshToken, ch.GetSetting().Proxy)
		if refreshErr == nil && res != nil {
			oauthKey.AccessToken = res.AccessToken
			oauthKey.RefreshToken = res.RefreshToken
			oauthKey.LastRefresh = time.Now().Format(time.RFC3339)
			oauthKey.Expired = res.ExpiresAt.Format(time.RFC3339)
			if strings.TrimSpace(oauthKey.Type) == "" {
				oauthKey.Type = "codex"
			}

			encoded, encErr := common.Marshal(oauthKey)
			if encErr == nil {
				_ = model.DB.Model(&model.Channel{}).Where("id = ?", ch.Id).Update("key", string(encoded)).Error
				model.InitChannelCache()
				service.ResetProxyClientCache()
			}

			ctx2, cancel2 := context.WithTimeout(c.Request.Context(), codexWhamUpstreamRequestTimeout)
			defer cancel2()
			statusCode, body, err = fetch(ctx2, client, ch.GetBaseURL(), oauthKey.AccessToken, accountID)
			if err != nil {
				common.SysError("failed to " + actionName + " after refresh: " + err.Error())
				c.JSON(http.StatusOK, gin.H{"success": false, "message": failureMessage})
				return
			}
		}
	}

	var payload any
	if common.Unmarshal(body, &payload) != nil {
		payload = string(body)
	}

	success := statusCode >= 200 && statusCode < 300
	resp := gin.H{
		"success":         success,
		"message":         "",
		"upstream_status": statusCode,
		"data":            payload,
	}
	if !success {
		resp["message"] = fmt.Sprintf("upstream status: %d", statusCode)
	}
	c.JSON(http.StatusOK, resp)
}

func GetCodexChannelUsage(c *gin.Context) {
	handleCodexWhamProxyRequest(c, "fetch codex usage", "获取用量信息失败，请稍后重试", func(ctx context.Context, client *http.Client, baseURL string, accessToken string, accountID string) (int, []byte, error) {
		return codexWhamUsageFetcher(ctx, client, baseURL, accessToken, accountID)
	})
}

func GetCodexChannelRateLimitResetCredits(c *gin.Context) {
	handleCodexWhamProxyRequest(c, "fetch codex rate limit reset credits", "获取重置额度失败，请稍后重试", func(ctx context.Context, client *http.Client, baseURL string, accessToken string, accountID string) (int, []byte, error) {
		return codexWhamResetCreditsFetcher(ctx, client, baseURL, accessToken, accountID)
	})
}

type resetCodexUsageRequest struct {
	CreditID        string `json:"credit_id,omitempty"`
	RedeemRequestID string `json:"redeem_request_id,omitempty"`
}

func resetCodexChannelUsage(c *gin.Context, actionName string, failureMessage string, creditID string, redeemRequestID string, requireCreditID bool) {
	creditID = strings.TrimSpace(creditID)
	if requireCreditID && creditID == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "credit_id is required"})
		return
	}

	redeemRequestID = strings.TrimSpace(redeemRequestID)
	if redeemRequestID == "" {
		redeemRequestID = codexRedeemRequestIDGenerator()
	}

	handleCodexWhamProxyRequest(c, actionName, failureMessage, func(ctx context.Context, client *http.Client, baseURL string, accessToken string, accountID string) (int, []byte, error) {
		return codexWhamUsageResetter(ctx, client, baseURL, accessToken, accountID, creditID, redeemRequestID)
	})
}

func ResetCodexChannelUsage(c *gin.Context) {
	var req resetCodexUsageRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil && err != io.EOF {
		common.ApiError(c, err)
		return
	}

	resetCodexChannelUsage(c, "reset codex usage", "重置用量失败，请稍后重试", req.CreditID, req.RedeemRequestID, false)
}

func ConsumeCodexChannelRateLimitResetCredit(c *gin.Context) {
	var req resetCodexUsageRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiError(c, err)
		return
	}

	resetCodexChannelUsage(c, "consume codex rate limit reset credit", "重置额度失败，请稍后重试", req.CreditID, req.RedeemRequestID, true)
}
