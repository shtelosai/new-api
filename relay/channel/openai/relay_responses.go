package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if service.TworkResponsesFailoverEnabled(c) {
		if responseErr := responsesFailure(&responsesResponse); responseErr != nil {
			return nil, responseErr
		}
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	if responsesResponse.HasImageGenerationCall() {
		c.Set("image_generation_call", true)
		c.Set("image_generation_call_quality", responsesResponse.GetQuality())
		c.Set("image_generation_call_size", responsesResponse.GetSize())
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := dto.Usage{}
	if responsesResponse.Usage != nil {
		usage.PromptTokens = responsesResponse.Usage.InputTokens
		usage.CompletionTokens = responsesResponse.Usage.OutputTokens
		usage.TotalTokens = responsesResponse.Usage.TotalTokens
		if responsesResponse.Usage.InputTokensDetails != nil {
			usage.PromptTokensDetails.CachedTokens = responsesResponse.Usage.InputTokensDetails.CachedTokens
			usage.PromptTokensDetails.CacheWriteTokens = responsesResponse.Usage.InputTokensDetails.CacheWriteTokens
		}
	}
	if info == nil || info.ResponsesUsageInfo == nil || info.ResponsesUsageInfo.BuiltInTools == nil {
		return &usage, nil
	}
	// 解析 Tools 用量
	for _, tool := range responsesResponse.Tools {
		buildToolinfo, ok := info.ResponsesUsageInfo.BuiltInTools[common.Interface2String(tool["type"])]
		if !ok || buildToolinfo == nil {
			logger.LogError(c, fmt.Sprintf("BuiltInTools not found for tool type: %v", tool["type"]))
			continue
		}
		buildToolinfo.CallCount++
	}
	return &usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	var responseTextBuilder strings.Builder
	guarded := service.TworkResponsesFailoverEnabled(c)
	var terminal bool
	var streamError *types.NewAPIError
	var pending []string
	pendingBytes := 0
	if guarded {
		// 首个有效输出之前保持 HTTP 可重试，不让心跳提前提交响应。
		previousDisablePing := info.DisablePing
		info.DisablePing = true
		defer func() { info.DisablePing = previousDisablePing }()
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			if guarded {
				streamError = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
				sr.Stop(err)
			} else {
				sr.Error(err)
			}
			return
		}
		if guarded {
			switch streamResponse.Type {
			case "error":
				var event struct {
					Code    any                `json:"code"`
					Message string             `json:"message"`
					Error   *types.OpenAIError `json:"error"`
				}
				_ = common.UnmarshalJsonStr(data, &event)
				upstream := types.OpenAIError{Type: "server_error", Code: event.Code, Message: event.Message}
				if event.Error != nil {
					upstream = *event.Error
				}
				if upstream.Message == "" {
					upstream.Message = "上游 Responses 流返回错误"
				}
				streamError = responsesUpstreamError(upstream)
			case "response.failed":
				streamError = responsesFailure(streamResponse.Response)
				if streamError == nil {
					streamError = types.NewOpenAIError(fmt.Errorf("上游 Responses 流失败"), types.ErrorCodeBadResponseBody, http.StatusBadGateway)
				}
			case "response.completed", "response.incomplete":
				streamError = responsesFailure(streamResponse.Response)
				terminal = streamError == nil
			}
			if streamError != nil {
				// 失败终态可能带有权威用量，优先于下方的文本估算。
				if streamResponse.Response != nil && streamResponse.Response.Usage != nil {
					reported := streamResponse.Response.Usage
					usage.PromptTokens = reported.InputTokens
					usage.CompletionTokens = reported.OutputTokens
					if reported.InputTokensDetails != nil {
						usage.PromptTokensDetails.CachedTokens = reported.InputTokensDetails.CachedTokens
						usage.PromptTokensDetails.CacheWriteTokens = reported.InputTokensDetails.CacheWriteTokens
					}
				}
				sr.Stop(streamError)
				return
			}
			if (streamResponse.Type == "response.created" || streamResponse.Type == "response.in_progress") && !c.Writer.Written() && pendingBytes+len(data) <= 64*1024 {
				pending = append(pending, data)
				pendingBytes += len(data)
				return
			}
			for _, buffered := range pending {
				var event dto.ResponsesStreamResponse
				_ = common.UnmarshalJsonStr(buffered, &event)
				sendResponsesStreamData(c, event, buffered)
			}
			pending = nil
		}
		sendResponsesStreamData(c, streamResponse, data)
		switch streamResponse.Type {
		case "response.completed", "response.incomplete":
			if streamResponse.Type == "response.incomplete" && !guarded {
				break
			}
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					if streamResponse.Response.Usage.InputTokens != 0 {
						usage.PromptTokens = streamResponse.Response.Usage.InputTokens
					}
					if streamResponse.Response.Usage.OutputTokens != 0 {
						usage.CompletionTokens = streamResponse.Response.Usage.OutputTokens
					}
					if streamResponse.Response.Usage.TotalTokens != 0 {
						usage.TotalTokens = streamResponse.Response.Usage.TotalTokens
					}
					if streamResponse.Response.Usage.InputTokensDetails != nil {
						usage.PromptTokensDetails.CachedTokens = streamResponse.Response.Usage.InputTokensDetails.CachedTokens
						usage.PromptTokensDetails.CacheWriteTokens = streamResponse.Response.Usage.InputTokensDetails.CacheWriteTokens
					}
				}
				if streamResponse.Response.HasImageGenerationCall() {
					c.Set("image_generation_call", true)
					c.Set("image_generation_call_quality", streamResponse.Response.GetQuality())
					c.Set("image_generation_call_size", streamResponse.Response.GetSize())
				}
			}
		case "response.output_text.delta":
			// 处理输出文本
			responseTextBuilder.WriteString(streamResponse.Delta)
		case dto.ResponsesOutputTypeItemDone:
			// 函数调用处理
			if streamResponse.Item != nil {
				switch streamResponse.Item.Type {
				case dto.BuildInCallWebSearchCall:
					if info != nil && info.ResponsesUsageInfo != nil && info.ResponsesUsageInfo.BuiltInTools != nil {
						if webSearchTool, exists := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview]; exists && webSearchTool != nil {
							webSearchTool.CallCount++
						}
					}
				}
			}
		}
		if guarded && terminal {
			sr.Done()
		}
	})

	if usage.CompletionTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			usage.CompletionTokens = completionTokens
		}
	}

	if usage.PromptTokens == 0 && usage.CompletionTokens != 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens

	if guarded {
		if streamError == nil && !terminal {
			streamError = types.NewOpenAIError(fmt.Errorf("上游 Responses 流在完成事件前中断"), types.ErrorCodeBadResponseBody, http.StatusBadGateway)
		}
		return usage, streamError
	}
	return usage, nil
}

// responsesFailure 不把 HTTP 200 等同于模型成功；正常的输出长度截断仍保留协议语义。
func responsesFailure(response *dto.OpenAIResponsesResponse) *types.NewAPIError {
	if response != nil {
		if upstream := response.GetOpenAIError(); upstream != nil && (upstream.Message != "" || upstream.Type != "" || upstream.Code != nil) {
			if upstream.Type == "" {
				upstream.Type = "server_error"
			}
			return responsesUpstreamError(*upstream)
		}
		var status string
		_ = common.Unmarshal(response.Status, &status)
		if status == "completed" || status == "incomplete" && response.IncompleteDetails != nil && (response.IncompleteDetails.Reason == "max_output_tokens" || response.IncompleteDetails.Reason == "content_filter") {
			return nil
		}
	}
	return types.NewOpenAIError(fmt.Errorf("上游 Responses 未返回有效完成状态"), types.ErrorCodeBadResponseBody, http.StatusBadGateway)
}

// 200 响应内的错误仍按错误类型归类，避免将无效输入或配额不足重试为服务故障。
func responsesUpstreamError(upstream types.OpenAIError) *types.NewAPIError {
	status := http.StatusBadGateway
	code := fmt.Sprint(upstream.Code)
	switch {
	case upstream.Type == "invalid_request_error" || code == "invalid_request_error" || code == "context_length_exceeded":
		status = http.StatusBadRequest
	case upstream.Type == "authentication_error" || code == "invalid_api_key":
		status = http.StatusUnauthorized
	case upstream.Type == "permission_error" || code == "access_denied":
		status = http.StatusForbidden
	case upstream.Type == "rate_limit_error" || code == "rate_limit_exceeded" || code == "insufficient_quota":
		status = http.StatusTooManyRequests
	}
	return types.WithOpenAIError(upstream, status)
}
