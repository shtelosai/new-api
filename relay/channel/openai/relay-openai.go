package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/openrouter"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func sendStreamData(c *gin.Context, info *relaycommon.RelayInfo, data string, forceFormat bool, thinkToContent bool) error {
	if data == "" {
		return nil
	}

	if !forceFormat && !thinkToContent {
		return helper.StringData(c, data)
	}

	var lastStreamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &lastStreamResponse); err != nil {
		return err
	}

	if !thinkToContent {
		return helper.ObjectData(c, lastStreamResponse)
	}

	hasThinkingContent := false
	hasContent := false
	var thinkingContent strings.Builder
	for _, choice := range lastStreamResponse.Choices {
		if len(choice.Delta.GetReasoningContent()) > 0 {
			hasThinkingContent = true
			thinkingContent.WriteString(choice.Delta.GetReasoningContent())
		}
		if len(choice.Delta.GetContentString()) > 0 {
			hasContent = true
		}
	}

	// Handle think to content conversion
	if info.ThinkingContentInfo.IsFirstThinkingContent {
		if hasThinkingContent {
			response := lastStreamResponse.Copy()
			for i := range response.Choices {
				// send `think` tag with thinking content
				response.Choices[i].Delta.SetContentString("<think>\n" + thinkingContent.String())
				response.Choices[i].Delta.ReasoningContent = nil
				response.Choices[i].Delta.Reasoning = nil
			}
			info.ThinkingContentInfo.IsFirstThinkingContent = false
			info.ThinkingContentInfo.HasSentThinkingContent = true
			return helper.ObjectData(c, response)
		}
	}

	if lastStreamResponse.Choices == nil || len(lastStreamResponse.Choices) == 0 {
		return helper.ObjectData(c, lastStreamResponse)
	}

	// Process each choice
	for i, choice := range lastStreamResponse.Choices {
		// Handle transition from thinking to content
		// only send `</think>` tag when previous thinking content has been sent
		if hasContent && !info.ThinkingContentInfo.SendLastThinkingContent && info.ThinkingContentInfo.HasSentThinkingContent {
			response := lastStreamResponse.Copy()
			for j := range response.Choices {
				response.Choices[j].Delta.SetContentString("\n</think>\n")
				response.Choices[j].Delta.ReasoningContent = nil
				response.Choices[j].Delta.Reasoning = nil
			}
			info.ThinkingContentInfo.SendLastThinkingContent = true
			helper.ObjectData(c, response)
		}

		// Convert reasoning content to regular content if any
		if len(choice.Delta.GetReasoningContent()) > 0 {
			lastStreamResponse.Choices[i].Delta.SetContentString(choice.Delta.GetReasoningContent())
			lastStreamResponse.Choices[i].Delta.ReasoningContent = nil
			lastStreamResponse.Choices[i].Delta.Reasoning = nil
		} else if !hasThinkingContent && !hasContent {
			// flush thinking content
			lastStreamResponse.Choices[i].Delta.ReasoningContent = nil
			lastStreamResponse.Choices[i].Delta.Reasoning = nil
		}
	}

	return helper.ObjectData(c, lastStreamResponse)
}

func OaiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	model := info.UpstreamModelName
	var responseId string
	var createAt int64 = 0
	var systemFingerprint string
	var containStreamUsage bool
	var responseTextBuilder strings.Builder
	var toolCount int
	var usage = &dto.Usage{}
	var lastStreamData string
	var secondLastStreamData string // 存储倒数第二个stream data，用于音频模型

	// 检查是否为音频模型
	isAudioModel := strings.Contains(strings.ToLower(model), "audio")

	// 可切换渠道的请求先缓冲没有实际内容的首段（角色、空增量）：在出现正文、思考、工具或结束标记之前出错或断流，
	// 客户端什么都没收到，交回外层换渠道；之后才出错则发出错误帧、不发成功收尾，由客户端续写。
	gate := service.ChannelFailoverEligible(c)
	if gate {
		common.SetContextKey(c, constant.ContextKeyOpenAIStreamGateActive, true)
		common.SetContextKey(c, constant.ContextKeyOpenAIStreamCommitted, false)
	}
	committed := !gate
	var pending []string
	forward := func(chunk string, sr *helper.StreamResult) {
		if err := HandleStreamFormat(c, info, chunk, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil {
			common.SysLog("error handling stream format: " + err.Error())
			sr.Error(err)
		}
	}
	commit := func(sr *helper.StreamResult) {
		if committed {
			return
		}
		committed = true
		common.SetContextKey(c, constant.ContextKeyOpenAIStreamCommitted, true)
		for _, chunk := range pending {
			forward(chunk, sr)
		}
		pending = nil
	}
	var upstreamErr *types.NewAPIError
	failedAfterCommit := false
	sawFinish := false
	lastForwarded := false
	var latestUsage *dto.Usage // 接收过程中记下最近一次有效用量，失败收尾时按它结算
	// 写出后只含用量的分段先扣住：格式转换（如转 Claude）会把它当成结束生成 message_stop。
	// OpenAI 格式在下一段内容前按原顺序放出；需要转换的协议一直扣到正常结束，用量按记下的真实值结算。
	var held []string
	flushHeld := func(sr *helper.StreamResult) {
		for _, chunk := range held {
			forward(chunk, sr)
		}
		held = nil
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if lastStreamData != "" {
			if committed && gate && isUsageOnlyChunk(lastStreamData) {
				held = append(held, lastStreamData)
			} else if committed {
				if info.RelayFormat == types.RelayFormatOpenAI {
					flushHeld(sr)
				}
				forward(lastStreamData, sr)
			} else {
				pending = append(pending, lastStreamData)
				if streamChunkHasOutput(lastStreamData) {
					commit(sr)
				}
			}
			lastForwarded = true
		}
		if !committed && !streamChunkParsable(data) {
			// 还没回复就收到解析不了的内容：按上游故障换渠道，不当成已回复。
			upstreamErr = types.NewOpenAIError(fmt.Errorf("unparsable upstream stream chunk"), types.ErrorCodeBadResponseBody, http.StatusBadGateway)
			sr.Stop(upstreamErr)
			return
		}
		if hasFinishReason(data) {
			sawFinish = true
		}
		if reported := streamChunkUsage(data); reported != nil {
			if latestUsage != nil {
				reported.PromptTokensDetails.InheritCacheWrites(latestUsage.PromptTokensDetails)
			}
			latestUsage = reported
		}
		if oaiErr := streamChunkError(data); oaiErr != nil {
			if !committed {
				upstreamErr = types.WithOpenAIError(*oaiErr, http.StatusBadGateway)
				sr.Stop(upstreamErr)
				return
			}
			// 已写出正文后才报错：这次回复不能重来。原样发出错误帧（不走普通增量转换），不发成功收尾。
			failedAfterCommit = true
			writeStreamErrorAfterCommit(c, info, data, oaiErr)
			sr.Stop(nil)
			return
		}
		if len(data) > 0 {
			// 对音频模型，保存倒数第二个stream data
			if isAudioModel && lastStreamData != "" {
				secondLastStreamData = lastStreamData
			}

			lastStreamData = data
			lastForwarded = false
			if err := processTokenData(info.RelayMode, data, &responseTextBuilder, &toolCount); err != nil {
				logger.LogError(c, "error processing stream token data: "+err.Error())
				sr.Error(err)
			}
		}
	})

	if upstreamErr != nil {
		return nil, upstreamErr
	}
	// 上游超时、读流出错，或正常断开却始终没给结束标记（[DONE] / finish_reason），都算这次回复没完成。
	brokenStream := streamEndedAbnormally(info) || (gate && !sawFinish && info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonEOF)
	if !committed {
		if brokenStream {
			return nil, types.NewOpenAIError(fmt.Errorf("upstream stream broke before any output: %s", info.StreamStatus.Summary()), types.ErrorCodeBadResponse, http.StatusBadGateway)
		}
		if lastStreamData == "" && len(pending) == 0 {
			return nil, types.NewError(fmt.Errorf("upstream stream ended without any content"), types.ErrorCodeEmptyResponse)
		}
	}
	if failedAfterCommit || (committed && brokenStream) {
		// 写出正文后断流或报错：先补发还没发出的最后一段，再发断流错误帧（客户端据 upstream_stream_error 续写），
		// 不补成功收尾；用量优先取上游最后一段里的真实值。
		common.SetContextKey(c, constant.ContextKeyUpstreamFailedAfterCommit, true)
		if !failedAfterCommit {
			// 只补发尾段里的实际内容，去掉结束原因：只有用量或结束标记的分段经格式转换会生成成功终态（如 Claude 的 message_stop）。
			if !lastForwarded && lastStreamData != "" && streamChunkHasOutput(lastStreamData) {
				_ = HandleStreamFormat(c, info, withoutFinishReason(lastStreamData), info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
			}
			writeStreamInterrupted(c, info)
		}
		if latestUsage != nil {
			usage, containStreamUsage = latestUsage, true
		}
		if !containStreamUsage {
			usage = service.ResponseText2Usage(c, responseTextBuilder.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
			usage.CompletionTokens += toolCount * 7
		}
		if latestUsage != nil {
			usage.PromptTokensDetails.InheritCacheWrites(latestUsage.PromptTokensDetails)
		}
		applyUsagePostProcessing(info, usage, common.StringToByteSlice(lastStreamData))
		return usage, nil
	}
	if len(held) > 0 && info.RelayFormat == types.RelayFormatOpenAI {
		// 正常结束：OpenAI 格式把扣住的用量分段按原顺序补发；需要转换的协议不转发，用量由下面按真实值结算。
		for _, chunk := range held {
			_ = HandleStreamFormat(c, info, chunk, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
		}
	}
	held = nil
	if len(pending) > 0 {
		// 正常结束但一直没有实际内容（如只有角色与结束标记）：照原样补发缓冲的分段。
		for _, chunk := range pending {
			_ = HandleStreamFormat(c, info, chunk, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
		}
		pending = nil
		common.SetContextKey(c, constant.ContextKeyOpenAIStreamCommitted, true)
	}

	// 对音频模型，从倒数第二个stream data中提取usage信息
	if isAudioModel && secondLastStreamData != "" {
		var streamResp struct {
			Usage *dto.Usage `json:"usage"`
		}
		err := common.Unmarshal([]byte(secondLastStreamData), &streamResp)
		if err == nil && streamResp.Usage != nil && service.ValidUsage(streamResp.Usage) {
			usage = streamResp.Usage
			containStreamUsage = true

			if common.DebugEnabled {
				logger.LogDebug(c, "Audio model usage extracted from second last SSE: PromptTokens=%d, CompletionTokens=%d, TotalTokens=%d, InputTokens=%d, OutputTokens=%d",
					usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
					usage.InputTokens, usage.OutputTokens)
			}
		}
	}

	// 处理最后的响应
	shouldSendLastResp := true
	if err := handleLastResponse(lastStreamData, &responseId, &createAt, &systemFingerprint, &model, &usage,
		&containStreamUsage, info, &shouldSendLastResp); err != nil {
		logger.LogError(c, fmt.Sprintf("error handling last response: %s, lastStreamData: [%s]", err.Error(), lastStreamData))
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		if shouldSendLastResp {
			_ = sendStreamData(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
		}
	}

	if !containStreamUsage && latestUsage != nil {
		// 最后一段没带用量时，用流中途上游报过的真实用量。
		usage, containStreamUsage = latestUsage, true
	}
	if !containStreamUsage {
		usage = service.ResponseText2Usage(c, responseTextBuilder.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		usage.CompletionTokens += toolCount * 7
	}

	if latestUsage != nil {
		usage.PromptTokensDetails.InheritCacheWrites(latestUsage.PromptTokensDetails)
	}
	applyUsagePostProcessing(info, usage, common.StringToByteSlice(lastStreamData))

	if info.RelayFormat == types.RelayFormatClaude && info.ClaudeConvertInfo != nil && latestUsage != nil {
		// 流里报过真实用量：自己转换最后一段；转换器还没收尾（用量不在最后一段时会这样），
		// 就在全部正文之后按这份用量补一次结束，结束事件带上用量。没报过用量的流仍走原收尾。
		info.ClaudeConvertInfo.Usage = usage
		_ = HandleStreamFormat(c, info, lastStreamData, false, false)
		if !info.ClaudeConvertInfo.Done {
			_ = HandleStreamFormat(c, info, usageOnlyChunk(responseId, createAt, model, usage), false, false)
		}
	} else {
		HandleFinalResponse(c, info, lastStreamData, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)
	}

	return usage, nil
}

func OpenaiHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	var simpleResponse dto.OpenAITextResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	logger.LogDebug(c, "upstream response body: %s", responseBody)
	// Unmarshal to simpleResponse
	if info.ChannelType == constant.ChannelTypeOpenRouter && info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		// 尝试解析为 openrouter enterprise
		var enterpriseResponse openrouter.OpenRouterEnterpriseResponse
		err = common.Unmarshal(responseBody, &enterpriseResponse)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if enterpriseResponse.Success {
			responseBody = enterpriseResponse.Data
		} else {
			logger.LogError(c, fmt.Sprintf("openrouter enterprise response success=false, data: %s", enterpriseResponse.Data))
			return nil, types.NewOpenAIError(fmt.Errorf("openrouter response success=false"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
	}

	err = common.Unmarshal(responseBody, &simpleResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := simpleResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		// 上游回 200 但内容是错误：按上游故障处理，才能换渠道。
		return nil, types.WithOpenAIError(*oaiError, upstreamErrorStatus(resp.StatusCode))
	}

	for _, choice := range simpleResponse.Choices {
		if choice.FinishReason == constant.FinishReasonContentFilter {
			common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "openai_finish_reason=content_filter")
			break
		}
	}

	forceFormat := false
	if info.ChannelSetting.ForceFormat {
		forceFormat = true
	}

	usageModified := false
	if simpleResponse.Usage.PromptTokens == 0 {
		completionTokens := simpleResponse.Usage.CompletionTokens
		if completionTokens == 0 {
			for _, choice := range simpleResponse.Choices {
				ctkm := service.CountTextToken(choice.Message.StringContent()+choice.Message.GetReasoningContent(), info.UpstreamModelName)
				completionTokens += ctkm
			}
		}
		simpleResponse.Usage = dto.Usage{
			PromptTokens:     info.GetEstimatePromptTokens(),
			CompletionTokens: completionTokens,
			TotalTokens:      info.GetEstimatePromptTokens() + completionTokens,
		}
		usageModified = true
	}

	applyUsagePostProcessing(info, &simpleResponse.Usage, responseBody)

	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if usageModified {
			var bodyMap map[string]interface{}
			err = common.Unmarshal(responseBody, &bodyMap)
			if err != nil {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			bodyMap["usage"] = simpleResponse.Usage
			responseBody, _ = common.Marshal(bodyMap)
		}
		if forceFormat {
			responseBody, err = common.Marshal(simpleResponse)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		} else {
			break
		}
	case types.RelayFormatClaude:
		convertResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatClaude, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		claudeRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = claudeRespStr
	case types.RelayFormatGemini:
		convertResult, err := relayconvert.ConvertResponse(c, info, types.RelayFormatGemini, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		geminiRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = geminiRespStr
	}

	// 此时已完整读完、解析并排除上游错误。Nginx/客户端可能在收到
	// Content-Length 指定的最后一字节后立即断开，须在写出前记录完成事实。
	if info.RelayMode == relayconstant.RelayModeEmbeddings && c.Request.Context().Err() == nil {
		common.SetContextKey(c, constant.ContextKeyEmbeddingResponseComplete, true)
	}
	service.IOCopyBytesGracefully(c, resp, responseBody)

	return &simpleResponse.Usage, nil
}
