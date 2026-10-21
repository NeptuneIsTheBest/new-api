package relay

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// PrepareResponsesRequest wraps the shared Responses preparation in a replayable
// HTTP body. The caller closes closer after the attempt;
// passthrough bodies remain owned by the incoming request's BodyStorage.
// The returned adaptor retains route/conversion state for DoRequest/DoResponse.
func PrepareResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, req *dto.OpenAIResponsesRequest) (relaychannel.Adaptor, common.ReplayableBody, io.Closer, *types.NewAPIError) {
	adaptor, jsonData, storage, apiErr := prepareResponsesRequest(c, info, req)
	if apiErr != nil {
		return nil, nil, nil, apiErr
	}
	if storage != nil {
		body := common.NewReplayableBodyReader(storage)
		return adaptor, body, io.NopCloser(body), nil
	}
	body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return nil, nil, nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	return adaptor, body, closer, nil
}

// prepareResponsesRequest returns either encoded JSON or borrowed passthrough
// storage. The incoming request owns that storage; neither transport closes it.
// Keep the adaptor instance: conversion can select its upstream route/protocol.
func prepareResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, req *dto.OpenAIResponsesRequest) (relaychannel.Adaptor, []byte, common.BodyStorage, *types.NewAPIError) {
	info.InitChannelMeta(c)
	if info.RelayMode == relayconstant.RelayModeResponsesCompact &&
		!common.SupportsResponsesCompact(info.ChannelType, info.ApiType) {
		return nil, nil, nil, types.NewErrorWithStatusCode(
			fmt.Errorf("unsupported endpoint %q for api type %d", "/v1/responses/compact", info.ApiType),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}

	if req == nil {
		return nil, nil, nil, types.NewError(errors.New("failed to copy responses request: copy source cannot be nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	passthrough := model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled
	request := *req
	if !passthrough {
		native := false
		switch info.ApiType {
		case constant.APITypeOpenAI, constant.APITypeOpenRouter, constant.APITypeXinference,
			constant.APITypeOllama, constant.APITypeAli, constant.APITypeVolcEngine,
			constant.APITypeDeepSeek, constant.APITypeXai, constant.APITypeMoonshot,
			constant.APITypeCodex, constant.APITypeNewAPI, constant.APITypeSub2API,
			constant.APITypeCloudflare, constant.APITypePerplexity, constant.APITypeZhipuV4:
			native = true
		case constant.APITypeAdvancedCustom:
			requestPath, _, _ := strings.Cut(info.RequestURLPath, "?")
			if c.Request != nil && c.Request.URL != nil {
				requestPath = c.Request.URL.Path
			}
			route, matched := info.ChannelOtherSettings.AdvancedCustom.MatchPathForModel(requestPath, info.OriginModelName)
			native = matched && route.IsNative()
		}
		if native {
			// These adaptors and the model helpers only mutate Reasoning.Effort
			// in place. Other pointers/raw JSON are read-only or replaced on this
			// attempt's struct. Recheck that contract before extending the list.
			if req.Reasoning != nil {
				reasoning := *req.Reasoning
				request.Reasoning = &reasoning
			}
		} else {
			cloned, err := common.DeepCopy(req)
			if err != nil {
				return nil, nil, nil, types.NewError(fmt.Errorf("failed to copy responses request: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
			}
			request = *cloned
		}
	}
	if err := helper.ModelMappedHelper(c, info, &request); err != nil {
		return nil, nil, nil, types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	if err := helper.ApplyReasoningModelSuffix(c, info, &request); err != nil {
		return nil, nil, nil, newConvertRequestFailedError(c, info, err)
	}

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return nil, nil, nil, types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	if passthrough {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return nil, nil, nil, types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
		}
		return adaptor, nil, storage, nil
	}

	convertedRequest, err := adaptor.ConvertOpenAIResponsesRequest(c, info, request)
	if err != nil {
		return nil, nil, nil, newConvertRequestFailedError(c, info, err)
	}
	relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)
	jsonData, err := common.Marshal(convertedRequest)
	if err != nil {
		return nil, nil, nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
	if err != nil {
		return nil, nil, nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, nil, nil, newAPIErrorFromParamOverride(err)
		}
	}

	logger.LogDebug(c, "requestBody: %s", jsonData)
	return adaptor, jsonData, nil, nil
}
