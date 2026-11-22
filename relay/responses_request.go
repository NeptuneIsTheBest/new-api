package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/requestplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// PrepareResponsesRequest wraps the shared Responses preparation in a replayable
// HTTP body. The caller closes closer after the attempt;
// passthrough bodies remain owned by the incoming request's BodyStorage.
// Metered creates reserve the finalized request before dispatch. The returned
// adaptor retains route/conversion state for DoRequest/DoResponse.
func PrepareResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, req *dto.OpenAIResponsesRequest) (relaychannel.Adaptor, common.ReplayableBody, io.Closer, *types.NewAPIError) {
	adaptor, jsonData, storage, apiErr := prepareResponsesRequest(c, info, req, nil)
	if apiErr != nil {
		return nil, nil, nil, apiErr
	}
	if info.ResponsesBilling != nil {
		warmup := false
		if info.GetFinalRequestRelayFormat() == types.RelayFormatOpenAIResponses {
			var err error
			warmup, err = responsesHTTPPrewarm(jsonData, storage, req.PromptCacheOptions)
			if err != nil {
				return nil, nil, nil, types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
			}
		}
		if apiErr := prepareResponsesAttemptBilling(c, info, warmup); apiErr != nil {
			info.ResponsesBilling.ReservationFailed = true
			return nil, nil, nil, apiErr
		}
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

// responsesHTTPPrewarm reads the final JSON literally. The DTO is a hint only
// for passthrough, whose stored body is identical to the validated request.
func responsesHTTPPrewarm(body []byte, storage common.BodyStorage, incomingOptions common.RawMessage) (bool, error) {
	if storage != nil {
		if len(incomingOptions) == 0 {
			return false, nil
		}
		if storage.IsDisk() {
			reader, err := storage.NewReader()
			if err != nil {
				return false, err
			}
			defer reader.Close()
			warmup, err := common.ReadJSONLastObjectBool(reader, "prompt_cache_options", "prewarm")
			if errors.Is(err, common.ErrInvalidJSON) {
				// A validated form request can also reach passthrough.
				return false, nil
			}
			return warmup, err
		}
		var err error
		body, err = storage.Bytes()
		if err != nil {
			return false, err
		}
	}
	if !ojson.GetBytesNoCopy(body, "prompt_cache_options").Exists() {
		return false, nil
	}
	var options ojson.Result
	ojson.GetBytesNoCopy(body, "@this").ForEach(func(key, value ojson.Result) bool {
		if key.Str == "prompt_cache_options" {
			options = value
		}
		return true
	})
	return responsesCachePrewarm(options), nil
}

// Read the boolean with the same last-member precedence as the JSON decoder.
func responsesCachePrewarm(options ojson.Result) bool {
	warmup := false
	if options.IsObject() {
		options.ForEach(func(key, value ojson.Result) bool {
			if key.Str == "prewarm" {
				warmup = value.Type == ojson.True
			}
			return true
		})
	}
	return warmup
}

// prepareResponsesRequest returns either encoded JSON or borrowed passthrough
// storage. The incoming request owns that storage; neither transport closes it.
// Keep the adaptor instance: conversion can select its upstream route/protocol.
func prepareResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, req *dto.OpenAIResponsesRequest, wsStreamOptions common.RawMessage) (relaychannel.Adaptor, []byte, common.BodyStorage, *types.NewAPIError) {
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
		if apiErr := requestplugin.PassThrough(c, info, storage); apiErr != nil {
			return nil, nil, nil, apiErr
		}
		return adaptor, nil, storage, nil
	}

	if len(wsStreamOptions) > 0 {
		// include_usage belongs to Chat. Ignore its value on a typed WebSocket create.
		var options struct {
			dto.ResponsesStreamOptions
			IncludeUsage common.RawMessage `json:"include_usage"`
		}
		if err := common.UnmarshalNoCopy(wsStreamOptions, &options); err != nil {
			messageID := i18n.MsgResponsesWSInvalidStreamOptions
			if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
				// The decoder can prefix the JSON field with embedded Go type names.
				field := typeErr.Field[strings.LastIndexByte(typeErr.Field, '.')+1:]
				switch strings.ToLower(field) {
				case "include_obfuscation":
					messageID = i18n.MsgResponsesWSInvalidIncludeObfuscation
				case "reasoning_summary_delivery":
					messageID = i18n.MsgResponsesWSInvalidReasoningSummaryDelivery
				}
			}
			return nil, nil, nil, newResponsesWSInvalidRequestError(errors.New(i18n.T(c, messageID)))
		}
		if options.IncludeObfuscation != nil || options.ReasoningSummaryDelivery != nil {
			request.StreamOptions = &options.ResponsesStreamOptions
		}
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
	jsonData, apiErr := requestplugin.Outbound(c, info, jsonData)
	if apiErr != nil {
		return nil, nil, nil, apiErr
	}
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, nil, nil, newAPIErrorFromParamOverride(err)
		}
	}
	if finalizer, ok := adaptor.(relaychannel.ResponsesRequestFinalizer); ok {
		jsonData, err = finalizer.FinalizeResponsesRequest(info, jsonData)
		if err != nil {
			return nil, nil, nil, newConvertRequestFailedError(c, info, err)
		}
	}
	// egress reads the body as it is sent, after the channel finalized it.
	if apiErr = requestplugin.Egress(c, info, jsonData); apiErr != nil {
		return nil, nil, nil, apiErr
	}

	logger.LogDebug(c, "requestBody: %s", jsonData)
	return adaptor, jsonData, nil, nil
}
