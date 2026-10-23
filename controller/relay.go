package controller

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func claudeStreamGateActive(c *gin.Context) bool {
	return common.GetContextKeyBool(c, constant.ContextKeyClaudeStreamGateActive)
}

func openAIStreamGateActive(c *gin.Context) bool {
	return common.GetContextKeyBool(c, constant.ContextKeyOpenAIStreamGateActive)
}

func resetAttemptStreamState(c *gin.Context) {
	for _, key := range []constant.ContextKey{
		constant.ContextKeyClaudeStreamGateActive, constant.ContextKeyClaudeStreamCommitted,
		constant.ContextKeyOpenAIStreamGateActive, constant.ContextKeyOpenAIStreamCommitted,
		constant.ContextKeyTworkResponsesStreamCommitted, constant.ContextKeyUpstreamFailedAfterCommit,
	} {
		common.SetContextKey(c, key, false)
	}
}

// relayOutputCommitted 判断是否已经把实际回复写给了客户端；只发过心跳或缓冲中的流不算，还能换渠道。
func relayOutputCommitted(c *gin.Context) bool {
	if c == nil || c.Writer == nil || !c.Writer.Written() {
		return false
	}
	if claudeStreamGateActive(c) && !common.GetContextKeyBool(c, constant.ContextKeyClaudeStreamCommitted) {
		return false
	}
	if openAIStreamGateActive(c) && !common.GetContextKeyBool(c, constant.ContextKeyOpenAIStreamCommitted) {
		return false
	}
	return !(service.TworkResponsesFailoverEnabled(c) && !service.TworkResponsesStreamCommitted(c))
}

func writeRelayError(c *gin.Context, relayFormat types.RelayFormat, newAPIError *types.NewAPIError) {
	if c.Writer.Written() && service.TworkResponsesFailoverEnabled(c) {
		oai := newAPIError.ToOpenAIError()
		if err := helper.ObjectData(c, gin.H{"type": "error", "code": oai.Code, "message": oai.Message}); err != nil {
			logger.LogError(c, "发送 Responses 流错误失败: "+err.Error())
		}
		return
	}
	if c.Writer.Written() && (claudeStreamGateActive(c) || openAIStreamGateActive(c)) {
		if relayFormat == types.RelayFormatClaude {
			err := helper.ClaudeData(c, dto.ClaudeResponse{
				Type:  "error",
				Error: newAPIError.ToClaudeError(),
			})
			if err != nil {
				logger.LogError(c, "send claude stream error failed: "+err.Error())
			}
			return
		}
		if err := helper.ObjectData(c, gin.H{"error": newAPIError.ToOpenAIError()}); err != nil {
			logger.LogError(c, "send openai stream error failed: "+err.Error())
		}
		return
	}

	if relayFormat == types.RelayFormatClaude {
		c.JSON(newAPIError.StatusCode, gin.H{
			"type":  "error",
			"error": newAPIError.ToClaudeError(),
		})
		return
	}
	c.JSON(newAPIError.StatusCode, gin.H{
		"error": newAPIError.ToOpenAIError(),
	})
}

// 所有协议的非固定渠道请求都参与冷却、切换与试探恢复。
func softFailureCooldownEnabledForRelay(c *gin.Context, relayFormat types.RelayFormat) bool {
	return service.ChannelFailoverEligible(c)
}

func relayAttemptLimit(c *gin.Context, relayFormat types.RelayFormat) int {
	// RetryTimes counts retries after the first request, while the cooldown
	// setting is an attempt count. Preserve the larger existing budget.
	currentLimit := common.RetryTimes + 1
	if !softFailureCooldownEnabledForRelay(c, relayFormat) {
		return currentLimit
	}
	return max(currentLimit, operation_setting.GetSoftFailureMaxAttempts())
}

// recordChannelSoftCooldownForRelay 渠道一出问题就让该渠道×模型冷却，所有用户绕开它；返回连续冷却次数，未记录时为 0。
func recordChannelSoftCooldownForRelay(c *gin.Context, relayFormat types.RelayFormat, relayInfo *relaycommon.RelayInfo, channelID int, modelName string, newAPIError *types.NewAPIError) int {
	if !softFailureCooldownEnabledForRelay(c, relayFormat) {
		return 0
	}
	if relayInfo == nil || relayInfo.IsChannelTest || relayRequestContextDone(c) || !service.IsChannelSideFailure(newAPIError) {
		return 0
	}

	errorClass := strings.TrimSpace(newAPIError.ToOpenAIError().Type)
	if errorClass == "" {
		errorClass = string(newAPIError.GetErrorType())
	}
	strikes := service.RecordChannelFailureCooldown(c, channelID, modelName, newAPIError.StatusCode, errorClass)
	// 会话改粘到这次成功的渠道；原渠道恢复后由新会话重新用上。
	if service.DeleteCurrentChannelAffinityBinding(c) {
		service.RecordAffinityClearedForLog(c)
	}
	return strikes
}

// disableChannelModelAfterFailure 不经 processChannelError 的失败路径（选中渠道配置出错、写出后断流）同样按规则升级禁用。
func disableChannelModelAfterFailure(c *gin.Context, channel *model.Channel, modelName string, err *types.NewAPIError, strikes int) {
	if channel == nil || !channel.GetAutoBan() || modelName == "" || !service.ShouldEscalateChannelFailure(err, strikes) {
		return
	}
	reason := err.ErrorWithStatusCode()
	channelID := channel.Id
	gopool.Go(func() { service.DisableChannelModelFromRelay(channelID, modelName, reason) })
}

func recordChannelSoftFailoverTerminalOutcome(c *gin.Context, succeeded bool) {
	outcome := service.SoftFailoverOutcomeExhausted
	if succeeded {
		outcome = service.SoftFailoverOutcomeSameRequestSuccess
	} else if common.GetContextKeyBool(c, constant.ContextKeyClaudeStreamCommitted) || service.TworkResponsesStreamCommitted(c) {
		outcome = service.SoftFailoverOutcomeDeferredAfterCommit
	}
	service.RecordChannelSoftFailoverOutcome(c, outcome)
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
	)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		if newAPIError != nil {
			if relayRequestContextDone(c) {
				logger.LogInfo(c, "relay canceled because client request context ended")
				return
			}
			logger.LogError(c, fmt.Sprintf("relay error: %s", common.LocalLogPreview(newAPIError.Error())))
			// 渠道都失败（含上游欠费）不是用户的问题；原文已进日志，回给用户统一的友好文案，不透传上游报错。
			// 已写出实际回复时保留原错误码：客户端靠流内错误码判断断流能否续写（如 upstream_stream_error），欠费照旧改写。
			if service.IsUpstreamQuotaExhaustedError(newAPIError) || (!relayOutputCommitted(c) && service.IsChannelSideFailure(newAPIError)) {
				newAPIError = service.NewUpstreamUnavailableError(requestId)
				// 渠道已经都试过，客户端 SDK 再重试也是同一批渠道。
				if !c.Writer.Written() {
					c.Header("X-Should-Retry", "false")
				}
			} else {
				newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			}
			if relayFormat == types.RelayFormatOpenAIRealtime {
				helper.WssError(c, ws, newAPIError.ToOpenAIError())
			} else {
				writeRelayError(c, relayFormat, newAPIError)
			}
		}
	}()

	// 先标记未成功：分发中间件据此只在真正成功后记录会话粘性。
	common.SetContextKey(c, constant.ContextKeyTworkRelaySucceeded, false)
	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		// Map "request body too large" to 413 so clients can handle it correctly
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			newAPIError = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
		} else {
			newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeError(newAPIError)
			if relayInfo.Billing != nil {
				relayInfo.Billing.Refund(c)
			}
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		TokenId:     c.GetInt(string(constant.ContextKeyTokenId)),
		Retry:       common.GetPointer(0),
	}
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil
	useSoftFailureCooldown := softFailureCooldownEnabledForRelay(c, relayFormat)
	attemptLimit := relayAttemptLimit(c, relayFormat)
	effectiveAttemptLimit := common.RetryTimes + 1
	realAttempts := 0
	retryParam.UseSoftFailureCooldown = useSoftFailureCooldown

	setupSkips := 0
	for ; (!useSoftFailureCooldown && retryParam.GetRetry() <= common.RetryTimes) || (useSoftFailureCooldown && realAttempts < effectiveAttemptLimit); retryParam.IncreaseRetry() {
		relayInfo.RetryIndex = retryParam.GetRetry()
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			if channel != nil && useSoftFailureCooldown && setupSkips < 3 {
				// 选中的渠道自身配置出错（如没有可用密钥）：冷却它并换下一个，健康的渠道还没试。
				setupSkips++
				strikes := service.RecordChannelFailureCooldown(c, channel.Id, relayInfo.OriginModelName, http.StatusServiceUnavailable, "channel_setup")
				disableChannelModelAfterFailure(c, channel, relayInfo.OriginModelName, channelErr, strikes)
				retryParam.ExcludeChannel(channel.Id)
				if relayInfo.LastError == nil {
					relayInfo.LastError = channelErr
				}
				continue
			}
			newAPIError = channelErr
			break
		}
		realAttempts++

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			// Ensure consistent 413 for oversized bodies even when error occurs later (e.g., retry path)
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
			} else {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)
		// 每次尝试只看自己的流状态：上一次没写出回复才会走到这里，它留下的闸门标记不能影响这次。
		resetAttemptStreamState(c)

		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			newAPIError = relayHandler(c, relayInfo)
		}

		if newAPIError == nil {
			relayInfo.LastError = nil
			failedAfterCommit := common.GetContextKeyBool(c, constant.ContextKeyUpstreamFailedAfterCommit)
			if failedAfterCommit && useSoftFailureCooldown && !relayRequestContextDone(c) {
				strikes := service.RecordChannelFailureCooldown(c, channel.Id, relayInfo.OriginModelName, http.StatusBadGateway, "stream_error")
				disableChannelModelAfterFailure(c, channel, relayInfo.OriginModelName,
					types.NewOpenAIError(errors.New("upstream stream failed after output"), types.ErrorCodeBadResponse, http.StatusBadGateway), strikes)
				if service.DeleteCurrentChannelAffinityBinding(c) {
					service.RecordAffinityClearedForLog(c)
				}
			}
			if !failedAfterCommit && relayUpstreamFinished(c, relayInfo) {
				common.SetContextKey(c, constant.ContextKeyTworkRelaySucceeded, true)
				if useSoftFailureCooldown {
					service.CompleteResponsesRecovery(c, channel.Id, relayInfo.OriginModelName)
				}
			}
			if relayFormat == types.RelayFormatClaude && !relayRequestContextDone(c) && !failedAfterCommit {
				common.SetContextKey(c, constant.ContextKeyClaudeRelaySucceeded, true)
			}
			if useSoftFailureCooldown {
				recordChannelSoftFailoverTerminalOutcome(c, true)
			}
			return
		}

		newAPIError = service.NormalizeViolationFeeError(newAPIError)
		relayInfo.LastError = newAPIError
		if relayRequestContextDone(c) {
			break
		}

		strikes := recordChannelSoftCooldownForRelay(c, relayFormat, relayInfo, channel.Id, relayInfo.OriginModelName, newAPIError)
		if common.GetContextKeyBool(c, constant.ContextKeyTworkImageRoute) && service.IsTworkImageChannelFailure(newAPIError) {
			service.RecordChannelSoftCooldown(c, channel.Id, relayInfo.OriginModelName, newAPIError.StatusCode, "server_error")
		}
		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError, strikes)

		softModelHealthFailure := useSoftFailureCooldown && service.IsRelaySoftFailure(c, newAPIError)
		if softModelHealthFailure {
			effectiveAttemptLimit = attemptLimit
		}
		remainingRetries := common.RetryTimes - retryParam.GetRetry()
		if useSoftFailureCooldown {
			// 按实际试过的渠道数算预算：普通故障最多 RetryTimes+1 个，限流/过载最多 attemptLimit 个；配置出错跳过的不算。
			limit := common.RetryTimes + 1
			if softModelHealthFailure {
				limit = attemptLimit
			}
			remainingRetries = limit - realAttempts
		}
		if !shouldRetry(c, newAPIError, remainingRetries) {
			break
		}
		retryParam.ExcludeChannel(channel.Id)
	}
	if useSoftFailureCooldown {
		recordChannelSoftFailoverTerminalOutcome(c, false)
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
	if newAPIError != nil && !relayRequestContextDone(c) {
		gopool.Go(func() {
			perfmetrics.RecordRelaySample(relayInfo, false, 0)
		})
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	policy, _ := common.GetContextKeyType[model.TworkRoutePolicy](c, constant.ContextKeyTworkRoutePolicy)
	if info.ChannelMeta == nil || common.GetContextKeyBool(c, constant.ContextKeyTworkExplicitChannelRoute) || (policy.ModelRoute && retryParam.GetRetry() == 0) {
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		return &model.Channel{
			Id:      c.GetInt("channel_id"),
			Type:    c.GetInt("channel_type"),
			Name:    c.GetString("channel_name"),
			AutoBan: &autoBanInt,
		}, nil
	}
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		if (errors.Is(err, service.ErrNoUntriedChannel) || errors.Is(err, service.ErrAllChannelsCooling)) && info.LastError != nil {
			return nil, info.LastError
		}
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return channel, newAPIError
	}
	return channel, nil
}

func relayRequestContextDone(c *gin.Context) bool {
	return c != nil && c.Request != nil && c.Request.Context().Err() != nil
}

// relayUpstreamFinished 这次上游是否真的完整回完：客户端读到结尾就断开很常见（桌面端就是），
// 不能因此不确认试探成功、不记新渠道的会话粘性；但客户端中途放弃时上游未必正常，不能据此算成功。
func relayUpstreamFinished(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if !relayRequestContextDone(c) || !info.IsStream {
		// 非流式返回 nil 时已完整读完上游响应；客户端中途走了会取消上游读取并报错。
		return true
	}
	if info.StreamStatus == nil {
		return false
	}
	switch info.StreamStatus.EndReason {
	case relaycommon.StreamEndReasonDone, relaycommon.StreamEndReasonEOF:
		return true
	}
	return false
}

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	// 管理员渠道后缀同样是精确路由，配置错误也不能换到其他渠道。
	if _, specified := c.Get("specific_channel_id"); specified {
		return false
	}
	if common.GetContextKeyBool(c, constant.ContextKeyTworkExplicitChannelRoute) {
		return false
	}
	if relayRequestContextDone(c) {
		return false
	}
	if relayOutputCommitted(c) {
		return false
	}
	if openaiErr == nil {
		return false
	}
	// 可切换请求：只要是渠道的问题就换下一个渠道，不受状态码名单、会话粘性规则或单条错误的跳过标记限制；
	// 用户自己的问题（积分、权限、请求不合法、上下文超长、内容审核）换渠道也一样，明确不换。
	if service.ChannelFailoverEligible(c) {
		return service.IsChannelSideFailure(openaiErr) && retryTimes > 0
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) && !(service.TworkResponsesFailoverEnabled(c) && service.IsRelaySoftFailure(c, openaiErr)) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	code := openaiErr.StatusCode
	if code >= 200 && code < 300 {
		return false
	}
	if code < 100 || code > 599 {
		return true
	}
	if service.TworkResponsesFailoverEnabled(c) && service.IsRelaySoftFailure(c, openaiErr) {
		return true
	}
	// 上游账号余额耗尽与请求内容无关，换渠道即可，不受状态码重试名单限制。
	if service.IsUpstreamQuotaExhaustedError(openaiErr) {
		return true
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

// legacyDisableRule 让渠道测试、异步任务沿用原来的禁用规则。
const legacyDisableRule = -1

// shouldDisableAfterRelayFailure 决定这次失败是否把渠道×模型转为系统禁用。
// 可切换请求的渠道故障先冷却：明确坏掉（鉴权、欠费、没开通）或连续冷却 strikes 次才禁用；一次 500 不再直接禁用。
func shouldDisableAfterRelayFailure(c *gin.Context, err *types.NewAPIError, strikes int) bool {
	if strikes != legacyDisableRule && service.ChannelFailoverEligible(c) {
		return service.ShouldEscalateChannelFailure(err, strikes)
	}
	imageCooling := common.GetContextKeyBool(c, constant.ContextKeyTworkImageRoute) && service.IsTworkImageChannelFailure(err)
	return service.ShouldDisableChannel(err) && !imageCooling && !(service.TworkResponsesFailoverEnabled(c) && service.IsRelaySoftFailure(c, err))
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, strikes int) {
	if relayRequestContextDone(c) {
		return
	}
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, common.LocalLogPreview(err.Error())))
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if channelError.AutoBan && shouldDisableAfterRelayFailure(c, err, strikes) {
		modelName := c.GetString("original_model")
		if modelName != "" {
			reason := err.ErrorWithStatusCode()
			channelId := channelError.ChannelId
			gopool.Go(func() {
				service.DisableChannelModelFromRelay(channelId, modelName, reason)
			})
		}
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到mysql中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		service.AppendChannelAffinityAdminInfo(c, adminInfo)
		service.AppendChannelSoftCooldownAdminInfo(c, adminInfo)
		other["admin_info"] = adminInfo
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(c, userId, channelId, modelName, tokenName, err.MaskSensitiveErrorWithStatusCode(), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}

}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *dto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relay.RelaySwapFace(c, relayInfo)
	default:
		mjErr = relay.RelayMidjourneySubmit(c, relayInfo)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	if mjErr != nil {
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	if taskErr := relay.ResolveOriginTask(c, relayInfo); taskErr != nil {
		respondTaskError(c, taskErr)
		return
	}

	var result *relay.TaskSubmitResult
	var taskErr *dto.TaskError
	defer func() {
		if taskErr != nil && relayInfo.Billing != nil {
			relayInfo.Billing.Refund(c)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:         c,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: c.Request.URL.Path,
		TokenId:     c.GetInt(string(constant.ContextKeyTokenId)),
		Retry:       common.GetPointer(0),
	}

	for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
		var channel *model.Channel

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			if retryParam.GetRetry() > 0 {
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				break
			}
		}

		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusRequestEntityTooLarge)
			} else {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusBadRequest)
			}
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		result, taskErr = relay.RelayTaskSubmit(c, relayInfo)
		if taskErr == nil {
			break
		}
		if relayRequestContextDone(c) {
			break
		}

		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				types.NewOpenAIError(taskErr.Error, types.ErrorCodeBadResponseStatusCode, taskErr.StatusCode), legacyDisableRule)
		}

		if !shouldRetryTaskRelay(c, channel.Id, taskErr, common.RetryTimes-retryParam.GetRetry()) {
			break
		}
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	// ── 成功：结算 + 日志 + 插入任务 ──
	if taskErr == nil {
		if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
			common.SysError("settle task billing error: " + settleErr.Error())
		}
		service.LogTaskConsumption(c, relayInfo)

		task := model.InitTask(result.Platform, relayInfo)
		task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
		task.PrivateData.BillingSource = relayInfo.BillingSource
		task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
		task.PrivateData.TokenId = relayInfo.TokenId
		task.PrivateData.NodeName = common.NodeName
		channelRatio := relayInfo.ChannelMeta.GetChannelRatio()
		task.PrivateData.BillingContext = &model.TaskBillingContext{
			ModelPrice:      relayInfo.PriceData.ModelPrice,
			GroupRatio:      relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ModelRatio:      relayInfo.PriceData.ModelRatio,
			ChannelRatio:    &channelRatio,
			OtherRatios:     relayInfo.PriceData.OtherRatios(),
			OriginModelName: relayInfo.OriginModelName,
			PerCallBilling:  common.StringsContains(constant.TaskPricePatches, relayInfo.OriginModelName) || relayInfo.PriceData.UsePrice,
		}
		task.Quota = result.Quota
		task.Data = result.TaskData
		task.Action = relayInfo.Action
		if insertErr := task.Insert(); insertErr != nil {
			common.SysError("insert task error: " + insertErr.Error())
		}
	}

	if taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *dto.TaskError) {
	if relayRequestContextDone(c) {
		return
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	c.JSON(taskErr.StatusCode, taskErr)
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if relayRequestContextDone(c) {
		return false
	}
	if taskErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if operation_setting.IsAlwaysSkipRetryStatusCode(taskErr.StatusCode) {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}
