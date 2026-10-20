package openai

import (
	"fmt"
	"io"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

// chatStreamChunk keeps the wire payload alongside its decoded value. The value
// is read-only so output transformations cannot change accounting or later frames.
type chatStreamChunk struct {
	data     string
	response *dto.ChatCompletionsStreamResponse
	err      error
}

func parseChatStreamChunk(data string) chatStreamChunk {
	chunk := chatStreamChunk{data: data, response: &dto.ChatCompletionsStreamResponse{}}
	chunk.err = common.UnmarshalJsonStr(data, chunk.response)
	return chunk
}

func HandleStreamFormat(c *gin.Context, info *relaycommon.RelayInfo, data string, forceFormat bool, thinkToContent bool) error {
	chunk := chatStreamChunk{data: data}
	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if data != "" && (forceFormat || thinkToContent) {
			chunk = parseChatStreamChunk(data)
		}
	case types.RelayFormatClaude, types.RelayFormatGemini:
		chunk = parseChatStreamChunk(data)
	}
	return chunk.send(c, info, forceFormat, thinkToContent)
}

// HandleParsedStreamFormat forwards a synthesized Chat chunk without a JSON
// round trip. It does not mutate the caller's response.
func HandleParsedStreamFormat(c *gin.Context, info *relaycommon.RelayInfo, response *dto.ChatCompletionsStreamResponse, forceFormat bool, thinkToContent bool) error {
	if response == nil {
		return fmt.Errorf("stream response is nil")
	}
	return (chatStreamChunk{response: response}).send(c, info, forceFormat, thinkToContent)
}

func (chunk chatStreamChunk) send(c *gin.Context, info *relaycommon.RelayInfo, forceFormat bool, thinkToContent bool) error {
	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		info.SendResponseCount++
		return sendStreamData(c, info, chunk, forceFormat, thinkToContent)
	case types.RelayFormatClaude:
		info.SendResponseCount++
		if chunk.err != nil {
			return chunk.err
		}
		return handleClaudeFormat(c, chunk.response, info)
	case types.RelayFormatGemini:
		// The stateful relaykit path owns its chunk counter so multi-hop and
		// direct conversions observe the same stream state semantics.
		if chunk.err != nil {
			logger.LogError(c, common.LogText("failed to unmarshal stream response: %s", chunk.err.Error()))
			return chunk.err
		}
		return handleGeminiFormat(c, chunk.response, info)
	}
	return nil
}

func handleClaudeFormat(c *gin.Context, streamResponse *dto.ChatCompletionsStreamResponse, info *relaycommon.RelayInfo) error {
	if streamResponse.Usage != nil {
		// The Claude converter merges usage into its mutable stream state.
		// Keep that state independent of the cached event and synthesized inputs.
		usage := *streamResponse.Usage
		usage.PromptTokensDetails = usage.PromptTokensDetails.Clone()
		if usage.InputTokensDetails != nil {
			details := usage.InputTokensDetails.Clone()
			usage.InputTokensDetails = &details
		}
		usage.BillingUsage = dto.CloneBillingUsage(usage.BillingUsage)
		info.EnsureClaudeConvertInfo().Usage = &usage
	}
	result, err := service.ConvertStreamResponse(c, info, types.RelayFormatClaude, streamResponse)
	if err != nil {
		return err
	}
	claudeResponses, ok := result.Value.([]*dto.ClaudeResponse)
	if !ok {
		return fmt.Errorf("expected Claude stream responses, got %T", result.Value)
	}
	for _, resp := range claudeResponses {
		helper.ClaudeData(c, *resp)
	}
	return nil
}

func handleGeminiFormat(c *gin.Context, streamResponse *dto.ChatCompletionsStreamResponse, info *relaycommon.RelayInfo) error {
	state, err := chatToGeminiStreamState(info, streamResponse)
	if err != nil {
		return err
	}
	results, err := service.ConvertStreamResponseChunk(c, info, state, streamResponse)
	if err != nil {
		return err
	}
	return sendGeminiStreamResults(c, results)
}

func chatToGeminiStreamState(info *relaycommon.RelayInfo, streamResponse *dto.ChatCompletionsStreamResponse) (*relayconvert.ResponseStreamState, error) {
	if info != nil && info.ChatToGeminiStreamState != nil {
		state, ok := info.ChatToGeminiStreamState.(*relayconvert.ResponseStreamState)
		if !ok || state == nil {
			return nil, fmt.Errorf("invalid Chat-to-Gemini stream state %T", info.ChatToGeminiStreamState)
		}
		return state, nil
	}

	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAI, types.RelayFormatGemini, relayconvert.ResponseStreamOptions{
		ID:      streamResponse.Id,
		Model:   streamResponse.Model,
		Created: streamResponse.Created,
	})
	if err != nil {
		return nil, err
	}
	if info != nil {
		info.ChatToGeminiStreamState = state
	}
	return state, nil
}

func sendGeminiStreamResults(c *gin.Context, results []relayconvert.ResponseResult) error {
	for _, result := range results {
		geminiResponse, ok := result.Value.(*dto.GeminiChatResponse)
		if !ok {
			return fmt.Errorf("expected Gemini stream response, got %T", result.Value)
		}
		if geminiResponse == nil {
			continue
		}
		data, err := common.Marshal(geminiResponse)
		if err != nil {
			logger.LogError(c, common.LogText("failed to marshal gemini response: %s", err.Error()))
			return err
		}
		c.Render(-1, common.CustomEvent{Data: "data: " + string(data)})
		_ = helper.FlushWriter(c)
	}
	return nil
}

func ProcessStreamResponse(streamResponse dto.ChatCompletionsStreamResponse, responseTextBuilder *strings.Builder, toolCount *int) error {
	for _, choice := range streamResponse.Choices {
		responseTextBuilder.WriteString(choice.Delta.GetContentString())
		responseTextBuilder.WriteString(choice.Delta.GetReasoningContent())
		if choice.Delta.ToolCalls != nil {
			if len(choice.Delta.ToolCalls) > *toolCount {
				*toolCount = len(choice.Delta.ToolCalls)
			}
			for _, tool := range choice.Delta.ToolCalls {
				responseTextBuilder.WriteString(tool.Function.Name)
				responseTextBuilder.WriteString(tool.Function.Arguments)
			}
		}
	}
	return nil
}

func processTokenData(info *relaycommon.RelayInfo, chunk chatStreamChunk, responseTextBuilder *strings.Builder, toolCount *int) error {
	switch info.RelayMode {
	case relayconstant.RelayModeChatCompletions:
		if chunk.err != nil {
			return chunk.err
		}
		info.ObserveResponseModel(chunk.response.Model)
		return ProcessStreamResponse(*chunk.response, responseTextBuilder, toolCount)
	case relayconstant.RelayModeCompletions:
		var streamResponse dto.CompletionsStreamResponse
		if err := common.UnmarshalJsonStr(chunk.data, &streamResponse); err != nil {
			return err
		}
		processCompletionsStreamResponse(streamResponse, responseTextBuilder)
	}
	return nil
}

func processCompletionsStreamResponse(streamResponse dto.CompletionsStreamResponse, responseTextBuilder *strings.Builder) {
	for _, choice := range streamResponse.Choices {
		responseTextBuilder.WriteString(choice.Text)
	}
}

func handleLastResponse(chunk chatStreamChunk, responseId *string, createAt *int64,
	systemFingerprint *string, model *string, usage **dto.Usage,
	containStreamUsage *bool, info *relaycommon.RelayInfo,
	shouldSendLastResp *bool) error {

	if chunk.err != nil {
		return chunk.err
	}
	lastStreamResponse := chunk.response
	if lastStreamResponse == nil {
		return io.ErrUnexpectedEOF
	}

	*responseId = lastStreamResponse.Id
	*createAt = lastStreamResponse.Created
	*systemFingerprint = lastStreamResponse.GetSystemFingerprint()
	*model = lastStreamResponse.Model

	if service.ValidUsage(lastStreamResponse.Usage) {
		*containStreamUsage = true
		*usage = dto.MergeUsageNonZero(*usage, lastStreamResponse.Usage)
		if !info.ShouldIncludeUsage {
			*shouldSendLastResp = lo.SomeBy(lastStreamResponse.Choices, func(choice dto.ChatCompletionsStreamResponseChoice) bool {
				return choice.Delta.GetContentString() != "" || choice.Delta.GetReasoningContent() != ""
			})
		}
	}

	return nil
}

func HandleFinalResponse(c *gin.Context, info *relaycommon.RelayInfo, lastStreamData string,
	responseId string, createAt int64, model string, systemFingerprint string,
	usage *dto.Usage, containStreamUsage bool) {
	var chunk chatStreamChunk
	if info.RelayFormat == types.RelayFormatClaude || info.RelayFormat == types.RelayFormatGemini {
		chunk = parseChatStreamChunk(lastStreamData)
	}
	chunk.finish(c, info, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)
}

// HandleParsedFinalResponse finishes a converted stream using its existing Chat
// chunk. Like HandleFinalResponse, native OpenAI output needs only the metadata.
func HandleParsedFinalResponse(c *gin.Context, info *relaycommon.RelayInfo, response *dto.ChatCompletionsStreamResponse,
	responseId string, createAt int64, model string, systemFingerprint string,
	usage *dto.Usage, containStreamUsage bool) {
	(chatStreamChunk{response: response}).finish(c, info, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)
}

func (chunk chatStreamChunk) finish(c *gin.Context, info *relaycommon.RelayInfo,
	responseId string, createAt int64, model string, systemFingerprint string,
	usage *dto.Usage, containStreamUsage bool) {
	if info.RelayFormat == types.RelayFormatClaude || info.RelayFormat == types.RelayFormatGemini {
		if chunk.err == nil && chunk.response == nil {
			chunk.err = io.ErrUnexpectedEOF
		}
		if chunk.err != nil {
			common.SysLog(common.LogText("error unmarshalling stream response: %s", chunk.err.Error()))
			return
		}
	}
	streamResponse := chunk.response

	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if info.ShouldIncludeUsage && !containStreamUsage {
			response := helper.GenerateFinalUsageResponse(responseId, createAt, model, *usage)
			response.SetSystemFingerprint(systemFingerprint)
			helper.ObjectData(c, response)
		}
		helper.Done(c)

	case types.RelayFormatClaude:
		info.ClaudeConvertInfo.Usage = usage

		result, err := service.ConvertStreamResponse(c, info, types.RelayFormatClaude, streamResponse)
		if err != nil {
			common.SysLog(common.LogText("error converting Claude stream response: %s", err.Error()))
			return
		}
		claudeResponses, ok := result.Value.([]*dto.ClaudeResponse)
		if !ok {
			common.SysLog(common.LogText("expected Claude stream responses, got %T", result.Value))
			return
		}
		for _, resp := range claudeResponses {
			_ = helper.ClaudeData(c, *resp)
		}
		info.ClaudeConvertInfo.Done = true

	case types.RelayFormatGemini:
		state, err := chatToGeminiStreamState(info, streamResponse)
		if err != nil {
			common.SysLog(common.LogText("error creating Gemini stream state: %s", err.Error()))
			return
		}
		state.SetUsage(usage)

		results, err := service.ConvertStreamResponseChunk(c, info, state, streamResponse)
		if err != nil {
			common.SysLog(common.LogText("error converting final Gemini stream response: %s", err.Error()))
			return
		}
		if err := sendGeminiStreamResults(c, results); err != nil {
			common.SysLog(common.LogText("error sending final Gemini stream response: %s", err.Error()))
			return
		}

		results, err = service.FinalizeStreamResponse(c, info, state)
		if err != nil {
			common.SysLog(common.LogText("error finalizing Gemini stream response: %s", err.Error()))
			return
		}
		if err := sendGeminiStreamResults(c, results); err != nil {
			common.SysLog(common.LogText("error sending finalized Gemini stream response: %s", err.Error()))
		}
	}
}

func sendResponsesStreamData(c *gin.Context, streamResponse dto.ResponsesStreamResponse, data string) {
	if data == "" {
		return
	}
	_ = helper.ResponseChunkData(c, streamResponse, data)
}
