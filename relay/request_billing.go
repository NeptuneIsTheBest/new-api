package relay

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

// PrepareRequestBilling estimates and reserves one request's charge. Transports
// provide the current request body through BodyStorage or BillingRequestInput;
// channel retries retain the resulting billing session and pricing snapshot.
func PrepareRequestBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	meta := &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer}
	estimateInput := constant.CountToken && !info.IsResponsesWarmup()
	if info.Request != nil && estimateInput {
		meta = info.Request.GetTokenCountMeta()
	} else {
		// Avoid building CombineText when only the pricing quantities are needed.
		switch request := info.Request.(type) {
		case *dto.GeneralOpenAIRequest:
			meta.MaxTokens = int(max(lo.FromPtr(request.MaxTokens), lo.FromPtr(request.MaxCompletionTokens)))
		case *dto.OpenAIResponsesRequest:
			meta.MaxTokens = int(lo.FromPtr(request.MaxOutputTokens))
		case *dto.ClaudeRequest:
			meta.MaxTokens = int(lo.FromPtr(request.MaxTokens))
		case *dto.ImageRequest:
			meta = request.GetTokenCountMeta()
		}
	}

	tokens := 0
	if estimateInput {
		var err error
		tokens, err = service.EstimateRequestToken(c, meta, info)
		if err != nil {
			return types.NewError(err, types.ErrorCodeCountTokenFailed)
		}
	}
	info.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, info, tokens, meta)
	if err != nil {
		return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
	}
	if priceData.FreeModel {
		logger.LogInfo(c, common.LogText("model %s is free, skipping pre-consume", info.OriginModelName))
		return nil
	}
	return service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info)
}

// prepareResponsesAttemptBilling runs after the upstream create is finalized,
// before the HTTP request, WS handshake or WS frame leaves the gateway.
func prepareResponsesAttemptBilling(c *gin.Context, info *relaycommon.RelayInfo, warmup bool) *types.NewAPIError {
	state := info.ResponsesBilling
	previousWarmup := state.Warmup
	state.Warmup = warmup
	state.Usage = relaycommon.WarmupUsageUnknown
	state.NeedsPromptEstimate = false
	if !state.Prepared {
		if err := PrepareRequestBilling(c, info); err != nil {
			return err
		}
		state.Prepared = true
		return nil
	}
	snap := info.TieredBillingSnapshot
	if snap != nil && warmup == previousWarmup {
		return service.PrepareTieredBillingForSelectedGroup(c, info)
	}

	tokens := info.GetEstimatePromptTokens()
	if warmup {
		tokens = 0
	} else if previousWarmup && constant.CountToken {
		var err error
		tokens, err = service.EstimateRequestToken(c, info.Request.GetTokenCountMeta(), info)
		if err != nil {
			return types.NewError(err, types.ErrorCodeCountTokenFailed, types.ErrOptionWithSkipRetry())
		}
	}
	info.SetEstimatePromptTokens(tokens)
	price := &info.PriceData
	group := price.GroupRatioInfo.GroupRatio
	previousQuota := price.QuotaToPreConsume
	var quota int
	var err error
	if snap != nil {
		err = helper.EstimateTieredReservation(snap, *info.BillingRequestInput, tokens, group, warmup)
		quota = snap.EstimatedQuotaAfterGroup
	} else if !warmup {
		cost := float64(tokens) * state.PreConsumeMultiplier * price.ModelRatio * group
		if price.UsePrice {
			cost = price.ApplyOtherRatiosToFloat(price.ModelPrice * common.QuotaPerUnit * group)
		}
		quota, err = common.QuotaFromFloatStrict(cost)
	}
	if err != nil {
		return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithSkipRetry(), types.ErrOptionWithStatusCode(http.StatusBadRequest))
	}
	price.QuotaToPreConsume = quota
	freePrice := snap == nil && (price.UsePrice && price.ModelPrice == 0 || !price.UsePrice && price.ModelRatio == 0)
	price.FreeModel = info.Billing == nil && (group == 0 || freePrice) && !operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
	if price.FreeModel || info.Billing != nil && quota <= previousQuota {
		return nil
	}
	return service.PreConsumeBilling(c, quota, info)
}

// RefundFailedRequestBilling applies the common final-failure policy after all
// eligible attempts have ended. A settled BillingSession never refunds again.
func RefundFailedRequestBilling(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) *types.NewAPIError {
	if apiErr == nil {
		return nil
	}
	apiErr = service.NormalizeViolationFeeError(apiErr)
	if info.Billing != nil {
		info.Billing.Refund(c)
	}
	service.ChargeViolationFeeIfNeeded(c, info, apiErr)
	return apiErr
}
