package claude

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func stopReasonClaude2OpenAI(reason string) string {
	return relayconvert.StopReasonClaudeToOpenAI(reason)
}

func maybeMarkClaudeRefusal(c *gin.Context, stopReason string) {
	if c == nil {
		return
	}
	if strings.EqualFold(stopReason, "refusal") {
		common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "claude_stop_reason=refusal")
	}
}

func StreamResponseClaude2OpenAI(claudeResponse *dto.ClaudeResponse) *dto.ChatCompletionsStreamResponse {
	return relayconvert.StreamResponseClaude2OpenAI(claudeResponse)
}

func ResponseClaude2OpenAI(claudeResponse *dto.ClaudeResponse) *dto.OpenAITextResponse {
	return relayconvert.ResponseClaude2OpenAI(claudeResponse)
}

type ClaudeResponseInfo = relayconvert.ClaudeResponseInfo

func cacheCreationTokensForOpenAIUsage(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	openAIUsage := relayconvert.UsageFromClaudeUsage(usage)
	if openAIUsage == nil {
		return 0
	}
	return openAIUsage.PromptTokens - usage.PromptTokens - usage.PromptTokensDetails.CachedTokens
}

func hasBillableClaudeUsage(usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	return usage.PromptTokens > 0 || usage.CompletionTokens > 0 ||
		usage.PromptTokensDetails.CachedTokens > 0 || usage.PromptTokensDetails.CachedCreationTokens > 0 ||
		usage.ClaudeCacheCreation5mTokens > 0 || usage.ClaudeCacheCreation1hTokens > 0
}

func buildOpenAIStyleUsageFromClaudeUsage(usage *dto.Usage) dto.Usage {
	mapped := relayconvert.UsageFromClaudeUsage(usage)
	if mapped == nil {
		return dto.Usage{}
	}
	return *mapped
}

func buildMessageDeltaPatchUsage(claudeResponse *dto.ClaudeResponse, claudeInfo *ClaudeResponseInfo) *dto.ClaudeUsage {
	return relayconvert.BuildMessageDeltaPatchUsage(claudeResponse, claudeInfo)
}

func shouldSkipClaudeMessageDeltaUsagePatch(info *relaycommon.RelayInfo) bool {
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled {
		return true
	}
	if info == nil {
		return false
	}
	return info.ChannelSetting.PassThroughBodyEnabled
}

func patchClaudeMessageDeltaUsageData(data string, usage *dto.ClaudeUsage) string {
	return relayconvert.PatchClaudeMessageDeltaUsageData(data, usage)
}

func FormatClaudeResponseInfo(claudeResponse *dto.ClaudeResponse, oaiResponse *dto.ChatCompletionsStreamResponse, claudeInfo *ClaudeResponseInfo) bool {
	return relayconvert.FormatClaudeResponseInfo(claudeResponse, oaiResponse, claudeInfo)
}

// tryParseClaudeGenericError 兜底解析非标准错误体（如 dashscope 风格 {"code":"...","message":"..."}）。
// 这类响应的 message 是字符串，按 ClaudeResponse 反序列化必然失败；此处提取上游真实错误信息透传，
// 避免用户只看到 "cannot unmarshal ... ClaudeMediaMessage" 的 500。仅在主解析失败后调用。
func tryParseClaudeGenericError(data []byte) *types.NewAPIError {
	var generic struct {
		Code    any    `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := common.Unmarshal(data, &generic); err != nil || generic.Message == "" {
		return nil
	}
	message := generic.Message
	if generic.Code != nil {
		message = fmt.Sprintf("%s (upstream code: %v)", generic.Message, generic.Code)
	}
	return types.WithClaudeError(types.ClaudeError{Type: "upstream_error", Message: message}, http.StatusInternalServerError)
}

type claudeStreamEvent struct {
	response  dto.ClaudeResponse
	data      string
	formatted bool
}

func parseClaudeStreamEvent(data string) (*claudeStreamEvent, *types.NewAPIError) {
	var claudeResponse dto.ClaudeResponse
	if err := common.UnmarshalJsonStr(data, &claudeResponse); err != nil {
		common.SysLog("error unmarshalling stream response: " + err.Error())
		if genericErr := tryParseClaudeGenericError([]byte(data)); genericErr != nil {
			return nil, genericErr
		}
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if claudeError := claudeResponse.GetClaudeError(); claudeError != nil && claudeError.Type != "" {
		return nil, types.WithClaudeError(*claudeError, http.StatusInternalServerError)
	}
	return &claudeStreamEvent{response: claudeResponse, data: data}, nil
}

func observeClaudeStreamEvent(claudeInfo *ClaudeResponseInfo, event *claudeStreamEvent) {
	claudeResponse := &event.response
	event.formatted = FormatClaudeResponseInfo(claudeResponse, nil, claudeInfo)
}

func emitClaudeStreamEvent(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo, event *claudeStreamEvent) {
	claudeResponse := &event.response
	data := event.data
	if claudeResponse.StopReason != "" {
		maybeMarkClaudeRefusal(c, claudeResponse.StopReason)
	}
	if claudeResponse.Delta != nil && claudeResponse.Delta.StopReason != nil {
		maybeMarkClaudeRefusal(c, *claudeResponse.Delta.StopReason)
	}
	if info.RelayFormat == types.RelayFormatClaude {
		if claudeResponse.Type == "message_start" && claudeResponse.Message != nil {
			info.UpstreamModelName = claudeResponse.Message.Model
		}
		if claudeResponse.Type == "message_delta" {
			// 确保 message_delta 的 usage 包含完整的 input_tokens 和 cache 相关字段
			// 解决 AWS Bedrock 等上游返回的 message_delta 缺少这些字段的问题
			if !shouldSkipClaudeMessageDeltaUsagePatch(info) {
				data = patchClaudeMessageDeltaUsageData(data, buildMessageDeltaPatchUsage(claudeResponse, claudeInfo))
			}
		}
		helper.ClaudeChunkData(c, *claudeResponse, data)
	} else if info.RelayFormat == types.RelayFormatOpenAI {
		if !event.formatted {
			return
		}
		response := StreamResponseClaude2OpenAI(claudeResponse)
		if response == nil {
			return
		}
		response.Id = claudeInfo.ResponseId
		response.Created = claudeInfo.Created
		response.Model = claudeInfo.Model
		if err := helper.ObjectData(c, response); err != nil {
			logger.LogError(c, "send_stream_response_failed: "+err.Error())
		}
	}
}

func HandleStreamResponseData(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo, data string) *types.NewAPIError {
	event, err := parseClaudeStreamEvent(data)
	if err != nil {
		return err
	}
	observeClaudeStreamEvent(claudeInfo, event)
	emitClaudeStreamEvent(c, info, claudeInfo, event)
	return nil
}

func HandleStreamFinalResponse(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo) {
	if claudeInfo.Usage.PromptTokens == 0 {
		//上游出错
	}
	if claudeInfo.Usage.CompletionTokens == 0 || !claudeInfo.Done {
		if common.DebugEnabled {
			common.SysLog("claude response usage is not complete, maybe upstream error")
		}
		// 只补缺失字段，不整份覆盖——保留 message_start 已拿到的 cache 字段
		fallback := service.ResponseText2Usage(c, claudeInfo.ResponseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		if claudeInfo.Usage.CompletionTokens == 0 ||
			(!claudeInfo.Done && fallback.CompletionTokens > claudeInfo.Usage.CompletionTokens) {
			claudeInfo.Usage.CompletionTokens = fallback.CompletionTokens
		}
		if claudeInfo.Usage.PromptTokens == 0 {
			claudeInfo.Usage.PromptTokens = fallback.PromptTokens
		}
		claudeInfo.Usage.TotalTokens = claudeInfo.Usage.PromptTokens + claudeInfo.Usage.CompletionTokens
	}
	if claudeInfo.Usage != nil {
		claudeInfo.Usage.UsageSemantic = "anthropic"
	}
	if claudeInfo.Usage != nil && claudeInfo.Usage.BillingUsage == nil {
		claudeInfo.Usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(buildMessageDeltaPatchUsage(nil, claudeInfo))
	}

	if info.RelayFormat == types.RelayFormatClaude {
		//
	} else if info.RelayFormat == types.RelayFormatOpenAI {
		if info.ShouldIncludeUsage {
			openAIUsage := buildOpenAIStyleUsageFromClaudeUsage(claudeInfo.Usage)
			response := helper.GenerateFinalUsageResponse(claudeInfo.ResponseId, claudeInfo.Created, info.UpstreamModelName, openAIUsage)
			err := helper.ObjectData(c, response)
			if err != nil {
				common.SysLog("send final response failed: " + err.Error())
			}
		}
		helper.Done(c)
	}
}

func ClaudeStreamHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	claudeInfo := &ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      common.GetTimestamp(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}
	common.SetContextKey(c, constant.ContextKeyClaudeStreamGateActive, true)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, false)

	committed := false
	pending := make([]*claudeStreamEvent, 0, 4)
	var err *types.NewAPIError
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		var event *claudeStreamEvent
		event, err = parseClaudeStreamEvent(data)
		if err != nil {
			sr.Stop(err)
			return
		}

		observeClaudeStreamEvent(claudeInfo, event)
		if committed {
			emitClaudeStreamEvent(c, info, claudeInfo, event)
			return
		}

		pending = append(pending, event)
		if hasBillableClaudeUsage(claudeInfo.Usage) || claudeInfo.HasSemanticOutput {
			committed = true
			common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, true)
			for _, pendingEvent := range pending {
				emitClaudeStreamEvent(c, info, claudeInfo, pendingEvent)
			}
			pending = nil
		}
	})
	if err != nil {
		return nil, err
	}

	if !committed {
		if claudeInfo.UsageReported {
			return nil, types.NewError(errors.New("upstream stream ended with zero usage and no output"), types.ErrorCodeEmptyResponse)
		}
		return nil, types.NewError(errors.New("upstream stream ended without any content"), types.ErrorCodeEmptyResponse)
	}

	HandleStreamFinalResponse(c, info, claudeInfo)
	return claudeInfo.Usage, nil
}

func HandleClaudeResponseData(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo, httpResp *http.Response, data []byte) *types.NewAPIError {
	var claudeResponse dto.ClaudeResponse
	err := common.Unmarshal(data, &claudeResponse)
	if err != nil {
		if genericErr := tryParseClaudeGenericError(data); genericErr != nil {
			return genericErr
		}
		return types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if claudeError := claudeResponse.GetClaudeError(); claudeError != nil && claudeError.Type != "" {
		return types.WithClaudeError(*claudeError, http.StatusInternalServerError)
	}
	maybeMarkClaudeRefusal(c, claudeResponse.StopReason)
	if claudeInfo.Usage == nil {
		claudeInfo.Usage = &dto.Usage{}
	}
	if claudeResponse.Usage != nil {
		claudeInfo.Usage.PromptTokens = claudeResponse.Usage.InputTokens
		claudeInfo.Usage.CompletionTokens = claudeResponse.Usage.OutputTokens
		claudeInfo.Usage.TotalTokens = claudeResponse.Usage.InputTokens + claudeResponse.Usage.OutputTokens
		claudeInfo.Usage.UsageSemantic = "anthropic"
		claudeInfo.Usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(claudeResponse.Usage)
		claudeInfo.Usage.PromptTokensDetails.CachedTokens = claudeResponse.Usage.CacheReadInputTokens
		claudeInfo.Usage.PromptTokensDetails.CachedCreationTokens = claudeResponse.Usage.CacheCreationInputTokens
		claudeInfo.Usage.ClaudeCacheCreation5mTokens = claudeResponse.Usage.GetCacheCreation5mTokens()
		claudeInfo.Usage.ClaudeCacheCreation1hTokens = claudeResponse.Usage.GetCacheCreation1hTokens()
	}
	var responseData []byte
	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		openaiResponse := ResponseClaude2OpenAI(&claudeResponse)
		openaiResponse.Usage = buildOpenAIStyleUsageFromClaudeUsage(claudeInfo.Usage)
		responseData, err = common.Marshal(openaiResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
	case types.RelayFormatClaude:
		responseData = data
	}

	if claudeResponse.Usage != nil && claudeResponse.Usage.ServerToolUse != nil && claudeResponse.Usage.ServerToolUse.WebSearchRequests > 0 {
		c.Set("claude_web_search_requests", claudeResponse.Usage.ServerToolUse.WebSearchRequests)
	}

	service.IOCopyBytesGracefully(c, httpResp, responseData)
	return nil
}

func ClaudeHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	claudeInfo := &ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      common.GetTimestamp(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	logger.LogDebug(c, "responseBody: %s", responseBody)
	handleErr := HandleClaudeResponseData(c, info, claudeInfo, resp, responseBody)
	if handleErr != nil {
		return nil, handleErr
	}
	return claudeInfo.Usage, nil
}
