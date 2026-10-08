package controller

import (
	"errors"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func GetTokenStats(c *gin.Context) {
	startTimestamp, startErr := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, endErr := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	ids := c.Query("token_ids")
	if startErr != nil || endErr != nil || startTimestamp < 0 || endTimestamp < startTimestamp || ids == "" || strings.Count(ids, ",") >= 100 {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	tokenIDs := make([]int, 0)
	seen := make(map[int]struct{})
	for value := range strings.SplitSeq(ids, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || id <= 0 {
			common.ApiErrorI18n(c, i18n.MsgInvalidParams)
			return
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		tokenIDs = append(tokenIDs, id)
	}
	stats, err := model.GetTokenUsageStats(c.Request.Context(), c.GetInt("id"), tokenIDs, startTimestamp, endTimestamp)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorI18n(c, i18n.MsgTokenInvalid)
		return
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, stats)
}
