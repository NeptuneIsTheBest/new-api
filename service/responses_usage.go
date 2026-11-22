package service

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/tokenkit"
)

// ResponsesUsageAccumulator owns the accounting facts for one Responses create.
// Its owner supplies a buffered response or feeds HTTP SSE / WebSocket events
// until Finish, including when a stream is interrupted.
type ResponsesUsageAccumulator struct {
	info            *relaycommon.RelayInfo
	usage           *dto.Usage
	outputText      strings.Builder
	imageCounter    relaycommon.ImageGenerationCallCounter
	imageCommitted  bool
	started         bool
	finished        bool
	warmupOutput    bool
	reportedInput   bool
	reportedOutput  bool
	reportedTotal   bool
	reportedNonzero bool
	terminalZero    bool
}

func NewResponsesUsageAccumulator(info *relaycommon.RelayInfo) *ResponsesUsageAccumulator {
	return &ResponsesUsageAccumulator{info: info, usage: &dto.Usage{}}
}

// Observe feeds one decoded stream event. raw is the same event's wire bytes;
// the vendor tool-usage reader runs on it once, at the terminal event.
func (a *ResponsesUsageAccumulator) Observe(event *dto.ResponsesStreamResponse, raw []byte) {
	if a == nil || event == nil || a.finished {
		return
	}
	if event.Response != nil {
		a.info.ObserveResponseModel(event.Response.Model)
	}
	a.started = true
	ObserveResponsesOutcome(a.info, event)
	switch event.Type {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		a.observeTerminal(event.Response, raw, true)
		return
	case "response.output_text.delta", "response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_text.delta", "response.refusal.delta":
		// Every delta kind here is generated output that upstream bills as
		// output tokens, so all of them feed the missing-usage estimate.
		a.outputText.WriteString(event.Delta)
	case dto.ResponsesOutputTypeItemDone:
		a.observeOutputItem(event.Item, event.OutputIndex)
	}
	if a.info.IsResponsesWarmup() {
		a.warmupOutput = a.warmupOutput || event.Item != nil || event.Delta != "" || strings.HasSuffix(event.Type, "_call.completed")
		if !a.warmupOutput && (strings.HasSuffix(event.Type, ".done") || strings.HasSuffix(event.Type, ".added") || event.Type == "response.image_generation_call.partial_image") {
			// Some providers send only a done frame. Read the original frame
			// so JSON fields omitted from the lightweight SSE decoder count too.
			ojson.GetBytesNoCopy(raw, "@this").ForEach(func(key, value ojson.Result) bool {
				switch key.Str {
				case "part":
					a.warmupOutput = value.IsObject()
				case "partial_image_b64":
					a.warmupOutput = value.Str != ""
				case "text", "transcript", "refusal", "arguments", "input":
					if value.Str != "" {
						a.warmupOutput = true
						a.outputText.WriteString(value.Str)
					}
				}
				return !a.warmupOutput
			})
		}
		if response := event.Response; response != nil {
			a.warmupOutput = a.warmupOutput || len(response.Output) > 0
			a.observeWarmupUsage(response.Usage, ojson.GetBytesNoCopy(raw, "response.usage"))
		}
	}
}

func (a *ResponsesUsageAccumulator) observeOutputItem(item *dto.ResponsesOutput, index *int) {
	if item == nil {
		return
	}
	switch item.Type {
	case dto.BuildInCallWebSearchCall, dto.BuildInCallFileSearchCall, dto.BuildInCallFunctionCall, dto.BuildInCallCustomToolCall:
		a.info.CountBillableToolCall(item.Type, item.Name)
	case dto.ResponsesOutputTypeImageGenerationCall:
		if !a.imageCommitted {
			a.imageCounter.Observe(item, index)
		}
	}
}

// BufferedResponse accounts for a complete native Responses JSON body. Tool
// invocations that a stream reports in output_item.done are in its output list.
func (a *ResponsesUsageAccumulator) BufferedResponse(response *dto.OpenAIResponsesResponse, raw []byte) *dto.Usage {
	if a.finished {
		return a.usage
	}
	a.started = true
	a.observeTerminal(response, raw, false)
	if a.info.IsResponsesWarmup() {
		if response != nil {
			switch ojson.GetBytesNoCopy(response.Status, "@this").Str {
			case "queued", "in_progress":
				a.terminalZero = false
			case "failed":
				a.started = false
			}
		}
		return a.Finish()
	}
	a.finished = true
	return a.usage
}

func (a *ResponsesUsageAccumulator) observeTerminal(response *dto.OpenAIResponsesResponse, raw []byte, stream bool) {
	if response != nil {
		if a.info.IsResponsesWarmup() {
			path := "usage"
			if stream {
				path = "response.usage"
			}
			a.terminalZero = a.observeWarmupUsage(response.Usage, ojson.GetBytesNoCopy(raw, path))
			a.warmupOutput = a.warmupOutput || len(response.Output) > 0
		} else {
			ApplyResponsesUsage(a.usage, response.Usage)
		}
		if (stream || a.info.IsResponsesWarmup()) && a.outputText.Len() == 0 {
			a.outputText.WriteString(relayconvert.ExtractOutputTextFromResponses(response))
		}
		for i := range response.Output {
			item := &response.Output[i]
			if !stream {
				a.observeOutputItem(item, &i)
			} else if !a.imageCommitted {
				a.imageCounter.Observe(item, &i)
			}
		}
	}
	// Vendor counts are cumulative and replace the per-item counts.
	a.info.ApplyVendorToolUsage(raw)
	if !a.imageCommitted {
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	}
}

// observeWarmupUsage distinguishes a reported zero from an omitted JSON
// number. Keep positive snapshots even when a later frame is sparse.
func (a *ResponsesUsageAccumulator) observeWarmupUsage(src *dto.Usage, raw ojson.Result) bool {
	if src == nil {
		return false
	}
	input, output, total := raw.Get("input_tokens"), raw.Get("output_tokens"), raw.Get("total_tokens")
	normalized := *src
	if input.Type != ojson.Number {
		input = raw.Get("prompt_tokens")
		if normalized.InputTokens == 0 {
			normalized.InputTokens = src.PromptTokens
		}
	}
	if output.Type != ojson.Number {
		output = raw.Get("completion_tokens")
		if normalized.OutputTokens == 0 {
			normalized.OutputTokens = src.CompletionTokens
		}
	}
	if normalized.InputTokensDetails == nil {
		normalized.InputTokensDetails = &src.PromptTokensDetails
	}
	ApplyResponsesUsage(a.usage, &normalized)
	// An in-progress all-zero placeholder cannot supply a missing field of
	// a later measured report. Only retain field presence from measured usage.
	if dto.HasOpenAIUsageTokens(src) || src.BillingUsage != nil && dto.HasOpenAIUsageTokens(effectiveBillingUsage(src)) ||
		src.OutputTokensDetails != nil && *src.OutputTokensDetails != (dto.OutputTokenDetails{}) ||
		normalized.InputTokensDetails.CacheCreationTokensTotal() != 0 || raw.Get("cost").Float() != 0 {
		a.reportedNonzero = true
		a.reportedInput = a.reportedInput || input.Type == ojson.Number || normalized.InputTokens > 0
		a.reportedOutput = a.reportedOutput || output.Type == ojson.Number || normalized.OutputTokens > 0
		a.reportedTotal = a.reportedTotal || src.TotalTokens > 0
	}
	return total.Type == ojson.Number && total.Float() == 0 ||
		input.Type == ojson.Number && input.Float() == 0 && output.Type == ojson.Number && output.Float() == 0
}

func (a *ResponsesUsageAccumulator) Finish() *dto.Usage {
	if a.finished {
		return a.usage
	}
	a.finished = true
	// A final image item can already have reached the client before the stream
	// disconnects, so completed tool usage is retained even without a terminal.
	if !a.imageCommitted {
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	}
	if a.info.IsResponsesWarmup() {
		billable := a.reportedNonzero || a.warmupOutput
		for _, tool := range a.info.ResponsesUsageInfo.BuiltInTools {
			if tool != nil && tool.CallCount > 0 {
				billable = true
				break
			}
		}
		switch {
		case billable:
			a.info.ResponsesBilling.Usage = relaycommon.WarmupUsageBillable
		case a.terminalZero && !a.info.StreamStatus.HasErrors():
			a.info.ResponsesBilling.Usage = relaycommon.WarmupUsageZero
			return a.usage
		}
	}
	if a.reportedTotal && a.reportedInput && !a.reportedOutput {
		a.usage.CompletionTokens = max(a.usage.TotalTokens-a.usage.PromptTokens, 0)
	}
	if a.usage.CompletionTokens == 0 && !a.reportedOutput && !(a.reportedInput && a.reportedTotal) {
		if output := a.outputText.String(); output != "" {
			a.usage.CompletionTokens = tokenkit.Count(a.info.GetUpstreamModelName(), output)
		}
	}
	// Upstream bills the prompt as soon as it starts generating, so a stream
	// that produced any event but no usage still owes its input tokens unless
	// upstream reported an explicit failure.
	billsPrompt := a.usage.CompletionTokens != 0 || (a.started && !a.info.StreamStatus.ResponseFailed())
	if a.reportedTotal && !a.reportedInput {
		a.usage.PromptTokens = max(a.usage.TotalTokens-a.usage.CompletionTokens, 0)
	} else if a.usage.PromptTokens == 0 && billsPrompt && !a.reportedInput {
		if a.info.IsResponsesWarmup() {
			a.info.ResponsesBilling.NeedsPromptEstimate = true
		} else {
			a.usage.PromptTokens = a.info.GetEstimatePromptTokens()
		}
	}
	total := a.usage.PromptTokens + a.usage.CompletionTokens
	if a.info.IsResponsesWarmup() {
		total = max(total, a.usage.TotalTokens)
	}
	a.usage.TotalTokens = total
	if a.usage.BillingUsage != nil {
		a.usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(a.usage.BillingUsage, a.usage.CompletionTokens)
	}
	return a.usage
}

// ObserveResponsesOutcome records the protocol outcome of one Responses event
// on the stream status for health classification. Only codes and types are
// kept; messages never leave the event.
func ObserveResponsesOutcome(info *relaycommon.RelayInfo, event *dto.ResponsesStreamResponse) {
	if info == nil || info.StreamStatus == nil || event == nil {
		return
	}
	var responseStatus string
	if event.Response != nil {
		_ = common.Unmarshal(event.Response.Status, &responseStatus)
	}
	switch {
	case event.Type == "error" || event.Type == "response.failed" || event.Type == "response.error" || responseStatus == "failed":
		code, errorType := event.Code, ""
		if event.Response != nil {
			if oaiErr := event.Response.GetOpenAIError(); oaiErr != nil {
				if oaiErr.Code != nil {
					code = fmt.Sprint(oaiErr.Code)
				}
				errorType = oaiErr.Type
			}
		}
		info.StreamStatus.MarkFailed(code, errorType, 0)
	case event.Type == "response.incomplete" || responseStatus == "incomplete":
		reason := ""
		if event.Response != nil && event.Response.IncompleteDetails != nil {
			reason = event.Response.IncompleteDetails.Reason
		}
		info.StreamStatus.MarkIncomplete(reason)
	case event.Type == "response.cancelled" || event.Type == "response.canceled" || responseStatus == "cancelled":
		info.StreamStatus.MarkCancelled()
	case event.Type == "response.completed" || event.Type == "response.done" || responseStatus == "completed":
		info.StreamStatus.MarkCompleted()
	}
}

func ApplyResponsesUsage(dst *dto.Usage, src *dto.Usage) {
	if dst == nil || src == nil {
		return
	}
	incoming := relayconvert.NormalizeResponsesUsage(src)
	if src.InputTokensDetails != nil {
		inputDetails := src.InputTokensDetails.Clone()
		incoming.InputTokensDetails = &inputDetails
	}
	if src.OutputTokensDetails != nil {
		incoming.CompletionTokenDetails = *src.OutputTokensDetails
	}
	incoming.PromptCacheHitTokens = src.PromptCacheHitTokens
	dto.MergeUsageNonZero(dst, incoming)
	outputDetails := dst.CompletionTokenDetails
	if outputDetails != (dto.OutputTokenDetails{}) {
		dst.OutputTokensDetails = &outputDetails
	}
}
