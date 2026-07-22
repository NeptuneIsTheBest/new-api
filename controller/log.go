package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func parseOptionalTokenId(c *gin.Context) (int, error) {
	rawTokenId := c.Query("token_id")
	if rawTokenId == "" {
		return 0, nil
	}
	tokenId, err := strconv.Atoi(rawTokenId)
	if err != nil || tokenId <= 0 {
		return 0, errors.New("token_id must be a positive integer")
	}
	return tokenId, nil
}

func parseLogTimeRange(c *gin.Context) (model.LogTimeRange, error) {
	timeRange := model.LogTimeRange{}
	timeRange.StartTimestamp, _ = strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	timeRange.EndTimestamp, _ = strconv.ParseInt(c.Query("end_timestamp"), 10, 64)

	if rawStartNano := c.Query("start_written_at_nano"); rawStartNano != "" {
		startNano, err := strconv.ParseInt(rawStartNano, 10, 64)
		if err != nil || startNano <= 0 {
			return model.LogTimeRange{}, errors.New("start_written_at_nano must be a positive integer")
		}
		timeRange.StartWrittenAtNano = startNano
	}
	if rawEndNano := c.Query("end_written_at_nano"); rawEndNano != "" {
		endNano, err := strconv.ParseInt(rawEndNano, 10, 64)
		if err != nil || endNano <= 0 {
			return model.LogTimeRange{}, errors.New("end_written_at_nano must be a positive integer")
		}
		timeRange.EndWrittenAtNano = endNano
	}
	if rawExclusive := c.Query("start_timestamp_exclusive"); rawExclusive != "" {
		exclusive, err := strconv.ParseBool(rawExclusive)
		if err != nil {
			return model.LogTimeRange{}, errors.New("start_timestamp_exclusive must be a boolean")
		}
		timeRange.StartTimestampExclusive = exclusive
	}
	if timeRange.StartTimestampExclusive && timeRange.StartTimestamp == 0 && timeRange.StartWrittenAtNano == 0 {
		return model.LogTimeRange{}, errors.New("start_timestamp_exclusive requires a start boundary")
	}
	return timeRange, nil
}

func GetAllLogs(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	logType, _ := strconv.Atoi(c.Query("type"))
	timeRange, err := parseLogTimeRange(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	username := c.Query("username")
	tokenName := c.Query("token_name")
	tokenId, err := parseOptionalTokenId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	requestId := c.Query("request_id")
	upstreamRequestId := c.Query("upstream_request_id")
	logs, total, err := model.GetAllLogs(logType, timeRange, modelName, username, tokenName, tokenId, pageInfo.GetStartIdx(), pageInfo.GetPageSize(), channel, group, requestId, upstreamRequestId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(logs)
	common.ApiSuccess(c, pageInfo)
	return
}

func GetUserLogs(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	userId := c.GetInt("id")
	logType, _ := strconv.Atoi(c.Query("type"))
	timeRange, err := parseLogTimeRange(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	tokenName := c.Query("token_name")
	tokenId, err := parseOptionalTokenId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	modelName := c.Query("model_name")
	group := c.Query("group")
	requestId := c.Query("request_id")
	upstreamRequestId := c.Query("upstream_request_id")
	logs, total, err := model.GetUserLogs(userId, logType, timeRange, modelName, tokenName, tokenId, pageInfo.GetStartIdx(), pageInfo.GetPageSize(), group, requestId, upstreamRequestId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(logs)
	common.ApiSuccess(c, pageInfo)
	return
}

// Deprecated: SearchAllLogs 已废弃，前端未使用该接口。
func SearchAllLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": "该接口已废弃",
	})
}

// Deprecated: SearchUserLogs 已废弃，前端未使用该接口。
func SearchUserLogs(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": "该接口已废弃",
	})
}

func GetLogByKey(c *gin.Context) {
	tokenId := c.GetInt("token_id")
	if tokenId == 0 {
		c.JSON(200, gin.H{
			"success": false,
			"message": "无效的令牌",
		})
		return
	}
	logs, err := model.GetLogByTokenId(tokenId)
	if err != nil {
		c.JSON(200, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data":    logs,
	})
}

func GetLogsStat(c *gin.Context) {
	logType, _ := strconv.Atoi(c.Query("type"))
	timeRange, err := parseLogTimeRange(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	tokenName := c.Query("token_name")
	tokenId, err := parseOptionalTokenId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	username := c.Query("username")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	stat, err := model.SumUsedQuota(logType, timeRange, modelName, username, tokenName, tokenId, channel, group)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, "")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota":              stat.Quota,
			"token":              stat.Token,
			"rpm":                stat.Rpm,
			"tpm":                stat.Tpm,
			"cache_tokens":       stat.CacheTokens,
			"cache_input_tokens": stat.CacheInputTokens,
			"cache_hit_rate":     stat.CacheHitRate,
		},
	})
	return
}

func GetLogsSelfStat(c *gin.Context) {
	username := c.GetString("username")
	logType, _ := strconv.Atoi(c.Query("type"))
	timeRange, err := parseLogTimeRange(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	tokenName := c.Query("token_name")
	tokenId, err := parseOptionalTokenId(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	group := c.Query("group")
	quotaNum, err := model.SumUsedQuota(logType, timeRange, modelName, username, tokenName, tokenId, channel, group)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, tokenName)
	c.JSON(200, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota":              quotaNum.Quota,
			"token":              quotaNum.Token,
			"rpm":                quotaNum.Rpm,
			"tpm":                quotaNum.Tpm,
			"cache_tokens":       quotaNum.CacheTokens,
			"cache_input_tokens": quotaNum.CacheInputTokens,
			"cache_hit_rate":     quotaNum.CacheHitRate,
		},
	})
	return
}
