package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/semaphore"

	"github.com/gin-gonic/gin"
)

type testResult struct {
	context     *gin.Context
	localErr    error
	newAPIError *types.NewAPIError
}

func shouldUseClaudeAgentProbe(channel *model.Channel) bool {
	return channel != nil &&
		channel.Type == constant.ChannelTypeAnthropic &&
		service.IsClaudeAgentProbeConfigured()
}

func buildClaudeAgentProbeCustomHeaders(info *relaycommon.RelayInfo) (map[string]string, string, string) {
	headers := make(map[string]string)
	claudeHeaders := http.Header{}
	model_setting.GetClaudeSettings().WriteHeaders(info.OriginModelName, &claudeHeaders)
	for key := range claudeHeaders {
		value := strings.TrimSpace(claudeHeaders.Get(key))
		if value != "" {
			headers[key] = value
		}
	}

	apiKey := info.ApiKey
	authToken := ""
	for key, value := range info.HeadersOverride {
		if relaychannel.IsHeaderPassthroughRuleKey(key) {
			continue
		}
		str, ok := value.(string)
		if !ok {
			continue
		}
		str = strings.TrimSpace(strings.ReplaceAll(str, "{api_key}", info.ApiKey))
		if str == "" || strings.HasPrefix(str, "{client_header:") {
			continue
		}

		switch strings.ToLower(strings.TrimSpace(key)) {
		case "authorization":
			authToken = strings.TrimSpace(strings.TrimPrefix(str, "Bearer "))
		case "x-api-key":
			apiKey = str
		default:
			headers[key] = str
		}
	}

	if len(headers) == 0 {
		headers = nil
	}
	return headers, apiKey, authToken
}

func testChannelWithClaudeAgentProbe(c *gin.Context, channel *model.Channel, info *relaycommon.RelayInfo) (testResult, bool) {
	if !shouldUseClaudeAgentProbe(channel) {
		return testResult{}, false
	}

	headers, apiKey, authToken := buildClaudeAgentProbeCustomHeaders(info)
	resp, err := service.ProbeClaudeAgent(c.Request.Context(), service.ClaudeAgentProbeRequest{
		BaseURL:       info.ChannelBaseUrl,
		APIKey:        apiKey,
		AuthToken:     authToken,
		Model:         info.UpstreamModelName,
		TimeoutMs:     getModelProbeTimeoutSec() * 1000,
		CustomHeaders: headers,
	})
	if err != nil && resp == nil {
		common.SysError(fmt.Sprintf(
			"claude agent probe unavailable: channel_id=%d model=%s error=%v",
			channel.Id,
			info.UpstreamModelName,
			err,
		))
		return testResult{
			context:  c,
			localErr: fmt.Errorf("claude agent probe unavailable: %w", err),
		}, true
	}
	if err != nil {
		return testResult{
			context:  c,
			localErr: fmt.Errorf("claude agent probe service error: %w", err),
		}, true
	}
	if resp == nil {
		return testResult{
			context:  c,
			localErr: errors.New("claude agent probe returned empty response"),
		}, true
	}
	if !resp.Success {
		errMsg := strings.TrimSpace(resp.Error)
		if errMsg == "" {
			errMsg = "claude agent probe failed"
		}
		err := fmt.Errorf("claude agent probe failed: %s", errMsg)
		if resp.LocalError {
			return testResult{
				context:  c,
				localErr: err,
			}, true
		}
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway),
		}, true
	}

	common.SysLog(fmt.Sprintf(
		"claude agent probe success: channel_id=%d model=%s latency_ms=%d session=%s",
		channel.Id,
		info.UpstreamModelName,
		resp.LatencyMs,
		resp.SDKSessionID,
	))
	return testResult{context: c}, true
}

func normalizeChannelTestEndpoint(channel *model.Channel, modelName, endpointType string) string {
	normalized := strings.TrimSpace(endpointType)
	if normalized != "" {
		return normalized
	}
	if strings.HasSuffix(modelName, ratio_setting.CompactModelSuffix) {
		return string(constant.EndpointTypeOpenAIResponseCompact)
	}
	if channel != nil && channel.Type == constant.ChannelTypeCodex {
		return string(constant.EndpointTypeOpenAIResponse)
	}
	return normalized
}

func resolveChannelTestUserID(c *gin.Context) (int, error) {
	if c != nil {
		if userID := c.GetInt("id"); userID > 0 {
			return userID, nil
		}
	}

	var rootUser model.User
	if err := model.DB.Select("id").Where("role = ?", common.RoleRootUser).First(&rootUser).Error; err != nil {
		return 0, fmt.Errorf("failed to resolve channel test user: %w", err)
	}
	if rootUser.Id == 0 {
		return 0, errors.New("failed to resolve channel test user")
	}
	return rootUser.Id, nil
}

func testChannel(ctx context.Context, channel *model.Channel, testUserID int, testModel string, endpointType string, isStream bool) testResult {
	if ctx == nil {
		ctx = context.Background()
	}
	tik := time.Now()
	var unsupportedTestChannelTypes = []int{
		constant.ChannelTypeMidjourney,
		constant.ChannelTypeMidjourneyPlus,
		constant.ChannelTypeSunoAPI,
		constant.ChannelTypeKling,
		constant.ChannelTypeJimeng,
		constant.ChannelTypeDoubaoVideo,
		constant.ChannelTypeVidu,
	}
	if lo.Contains(unsupportedTestChannelTypes, channel.Type) {
		channelTypeName := constant.GetChannelTypeName(channel.Type)
		return testResult{
			localErr: fmt.Errorf("%s channel test is not supported", channelTypeName),
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	testModel = strings.TrimSpace(testModel)
	if testModel == "" {
		if channel.TestModel != nil && *channel.TestModel != "" {
			testModel = strings.TrimSpace(*channel.TestModel)
		} else {
			models := channel.GetModels()
			if len(models) > 0 {
				testModel = strings.TrimSpace(models[0])
			}
			if testModel == "" {
				testModel = "gpt-4o-mini"
			}
		}
	}

	endpointType = normalizeChannelTestEndpoint(channel, testModel, endpointType)

	requestPath := "/v1/chat/completions"

	// 如果指定了端点类型，使用指定的端点类型
	if endpointType != "" {
		if endpointInfo, ok := common.GetDefaultEndpointInfo(constant.EndpointType(endpointType)); ok {
			requestPath = endpointInfo.Path
		}
	} else {
		// 如果没有指定端点类型，使用原有的自动检测逻辑

		if strings.Contains(strings.ToLower(testModel), "rerank") {
			requestPath = "/v1/rerank"
		}

		// 先判断是否为 Embedding 模型
		if strings.Contains(strings.ToLower(testModel), "embedding") ||
			strings.HasPrefix(testModel, "m3e") || // m3e 系列模型
			strings.Contains(testModel, "bge-") || // bge 系列模型
			strings.Contains(testModel, "embed") ||
			channel.Type == constant.ChannelTypeMokaAI { // 其他 embedding 模型
			requestPath = "/v1/embeddings" // 修改请求路径
		}

		// VolcEngine 图像生成模型
		if channel.Type == constant.ChannelTypeVolcEngine && strings.Contains(testModel, "seedream") {
			requestPath = "/v1/images/generations"
		}

		// responses-only models
		if strings.Contains(strings.ToLower(testModel), "codex") {
			requestPath = "/v1/responses"
		}

		// responses compaction models (must use /v1/responses/compact)
		if strings.HasSuffix(testModel, ratio_setting.CompactModelSuffix) {
			requestPath = "/v1/responses/compact"
		}
	}
	if strings.HasPrefix(requestPath, "/v1/responses/compact") {
		testModel = ratio_setting.WithCompactModelSuffix(testModel)
	}

	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, requestPath, nil)

	cache, err := model.GetUserCache(testUserID)
	if err != nil {
		return testResult{
			localErr:    err,
			newAPIError: nil,
		}
	}
	cache.WriteContext(c)
	c.Set("id", testUserID)

	//c.Request.Header.Set("Authorization", "Bearer "+channel.Key)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("channel", channel.Type)
	c.Set("base_url", channel.GetBaseURL())
	group, _ := model.GetUserGroup(testUserID, false)
	c.Set("group", group)

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, testModel)
	if newAPIError != nil {
		return testResult{
			context:     c,
			localErr:    newAPIError,
			newAPIError: newAPIError,
		}
	}

	// Determine relay format based on endpoint type or request path
	var relayFormat types.RelayFormat
	if endpointType != "" {
		// 根据指定的端点类型设置 relayFormat
		switch constant.EndpointType(endpointType) {
		case constant.EndpointTypeOpenAI:
			relayFormat = types.RelayFormatOpenAI
		case constant.EndpointTypeOpenAIResponse:
			relayFormat = types.RelayFormatOpenAIResponses
		case constant.EndpointTypeOpenAIResponseCompact:
			relayFormat = types.RelayFormatOpenAIResponsesCompaction
		case constant.EndpointTypeAnthropic:
			relayFormat = types.RelayFormatClaude
		case constant.EndpointTypeGemini:
			relayFormat = types.RelayFormatGemini
		case constant.EndpointTypeJinaRerank:
			relayFormat = types.RelayFormatRerank
		case constant.EndpointTypeImageGeneration:
			relayFormat = types.RelayFormatOpenAIImage
		case constant.EndpointTypeEmbeddings:
			relayFormat = types.RelayFormatEmbedding
		default:
			relayFormat = types.RelayFormatOpenAI
		}
	} else {
		// 根据请求路径自动检测
		relayFormat = types.RelayFormatOpenAI
		if c.Request.URL.Path == "/v1/embeddings" {
			relayFormat = types.RelayFormatEmbedding
		}
		if c.Request.URL.Path == "/v1/images/generations" {
			relayFormat = types.RelayFormatOpenAIImage
		}
		if c.Request.URL.Path == "/v1/messages" {
			relayFormat = types.RelayFormatClaude
		}
		if strings.Contains(c.Request.URL.Path, "/v1beta/models") {
			relayFormat = types.RelayFormatGemini
		}
		if c.Request.URL.Path == "/v1/rerank" || c.Request.URL.Path == "/rerank" {
			relayFormat = types.RelayFormatRerank
		}
		if c.Request.URL.Path == "/v1/responses" {
			relayFormat = types.RelayFormatOpenAIResponses
		}
		if strings.HasPrefix(c.Request.URL.Path, "/v1/responses/compact") {
			relayFormat = types.RelayFormatOpenAIResponsesCompaction
		}
	}

	request := buildTestRequest(testModel, endpointType, channel, isStream)

	info, err := relaycommon.GenRelayInfo(c, relayFormat, request, nil)

	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewError(err, types.ErrorCodeGenRelayInfoFailed),
		}
	}

	info.IsChannelTest = true
	info.InitChannelMeta(c)

	err = attachTestBillingRequestInput(info, request)
	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewError(err, types.ErrorCodeJsonMarshalFailed),
		}
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewError(err, types.ErrorCodeChannelModelMappedError),
		}
	}

	testModel = info.UpstreamModelName
	// 更新请求中的模型名称
	request.SetModelName(testModel)

	if result, handled := testChannelWithClaudeAgentProbe(c, channel, info); handled {
		return result
	}

	apiType, _ := common.ChannelType2APIType(channel.Type)
	if info.RelayMode == relayconstant.RelayModeResponsesCompact &&
		apiType != constant.APITypeOpenAI &&
		apiType != constant.APITypeCodex {
		return testResult{
			context:     c,
			localErr:    fmt.Errorf("responses compaction test only supports openai/codex channels, got api type %d", apiType),
			newAPIError: types.NewError(fmt.Errorf("unsupported api type: %d", apiType), types.ErrorCodeInvalidApiType),
		}
	}
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return testResult{
			context:     c,
			localErr:    fmt.Errorf("invalid api type: %d, adaptor is nil", apiType),
			newAPIError: types.NewError(fmt.Errorf("invalid api type: %d, adaptor is nil", apiType), types.ErrorCodeInvalidApiType),
		}
	}

	//// 创建一个用于日志的 info 副本，移除 ApiKey
	//logInfo := info
	//logInfo.ApiKey = ""
	common.SysLog(fmt.Sprintf("testing channel %d with model %s , info %+v ", channel.Id, testModel, info.ToString()))

	priceData, err := helper.ModelPriceHelper(c, info, 0, request.GetTokenCountMeta())
	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest)),
		}
	}

	adaptor.Init(info)

	var convertedRequest any
	// 根据 RelayMode 选择正确的转换函数
	switch info.RelayMode {
	case relayconstant.RelayModeEmbeddings:
		// Embedding 请求 - request 已经是正确的类型
		if embeddingReq, ok := request.(*dto.EmbeddingRequest); ok {
			convertedRequest, err = adaptor.ConvertEmbeddingRequest(c, info, *embeddingReq)
		} else {
			return testResult{
				context:     c,
				localErr:    errors.New("invalid embedding request type"),
				newAPIError: types.NewError(errors.New("invalid embedding request type"), types.ErrorCodeConvertRequestFailed),
			}
		}
	case relayconstant.RelayModeImagesGenerations:
		// 图像生成请求 - request 已经是正确的类型
		if imageReq, ok := request.(*dto.ImageRequest); ok {
			convertedRequest, err = adaptor.ConvertImageRequest(c, info, *imageReq)
		} else {
			return testResult{
				context:     c,
				localErr:    errors.New("invalid image request type"),
				newAPIError: types.NewError(errors.New("invalid image request type"), types.ErrorCodeConvertRequestFailed),
			}
		}
	case relayconstant.RelayModeRerank:
		// Rerank 请求 - request 已经是正确的类型
		if rerankReq, ok := request.(*dto.RerankRequest); ok {
			convertedRequest, err = adaptor.ConvertRerankRequest(c, info.RelayMode, *rerankReq)
		} else {
			return testResult{
				context:     c,
				localErr:    errors.New("invalid rerank request type"),
				newAPIError: types.NewError(errors.New("invalid rerank request type"), types.ErrorCodeConvertRequestFailed),
			}
		}
	case relayconstant.RelayModeResponses:
		// Response 请求 - request 已经是正确的类型
		if responseReq, ok := request.(*dto.OpenAIResponsesRequest); ok {
			convertedRequest, err = adaptor.ConvertOpenAIResponsesRequest(c, info, *responseReq)
		} else {
			return testResult{
				context:     c,
				localErr:    errors.New("invalid response request type"),
				newAPIError: types.NewError(errors.New("invalid response request type"), types.ErrorCodeConvertRequestFailed),
			}
		}
	case relayconstant.RelayModeResponsesCompact:
		// Response compaction request - convert to OpenAIResponsesRequest before adapting
		switch req := request.(type) {
		case *dto.OpenAIResponsesCompactionRequest:
			convertedRequest, err = adaptor.ConvertOpenAIResponsesRequest(c, info, dto.OpenAIResponsesRequest{
				Model:              req.Model,
				Input:              req.Input,
				Instructions:       req.Instructions,
				PreviousResponseID: req.PreviousResponseID,
			})
		case *dto.OpenAIResponsesRequest:
			convertedRequest, err = adaptor.ConvertOpenAIResponsesRequest(c, info, *req)
		default:
			return testResult{
				context:     c,
				localErr:    errors.New("invalid response compaction request type"),
				newAPIError: types.NewError(errors.New("invalid response compaction request type"), types.ErrorCodeConvertRequestFailed),
			}
		}
	default:
		// Chat/Completion 等其他请求类型
		if generalReq, ok := request.(*dto.GeneralOpenAIRequest); ok {
			convertedRequest, err = adaptor.ConvertOpenAIRequest(c, info, generalReq)
		} else {
			return testResult{
				context:     c,
				localErr:    errors.New("invalid general request type"),
				newAPIError: types.NewError(errors.New("invalid general request type"), types.ErrorCodeConvertRequestFailed),
			}
		}
	}

	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewError(err, types.ErrorCodeConvertRequestFailed),
		}
	}
	jsonData, err := common.Marshal(convertedRequest)
	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewError(err, types.ErrorCodeJsonMarshalFailed),
		}
	}

	//jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings)
	//if err != nil {
	//	return testResult{
	//		context:     c,
	//		localErr:    err,
	//		newAPIError: types.NewError(err, types.ErrorCodeConvertRequestFailed),
	//	}
	//}

	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			if fixedErr, ok := relaycommon.AsParamOverrideReturnError(err); ok {
				return testResult{
					context:     c,
					localErr:    fixedErr,
					newAPIError: relaycommon.NewAPIErrorFromParamOverride(fixedErr),
				}
			}
			return testResult{
				context:     c,
				localErr:    err,
				newAPIError: types.NewError(err, types.ErrorCodeChannelParamOverrideInvalid),
			}
		}
	}

	requestBody := bytes.NewBuffer(jsonData)
	c.Request.Body = io.NopCloser(bytes.NewBuffer(jsonData))
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError),
		}
	}
	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		if httpResp.StatusCode != http.StatusOK {
			err := service.RelayErrorHandler(c.Request.Context(), httpResp, true)
			common.SysError(fmt.Sprintf(
				"channel test bad response: channel_id=%d name=%s type=%d model=%s endpoint_type=%s status=%d err=%v",
				channel.Id,
				channel.Name,
				channel.Type,
				testModel,
				endpointType,
				httpResp.StatusCode,
				err,
			))
			return testResult{
				context:     c,
				localErr:    err,
				newAPIError: types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError),
			}
		}
	}
	usageA, respErr := adaptor.DoResponse(c, httpResp, info)
	if respErr != nil {
		return testResult{
			context:     c,
			localErr:    respErr,
			newAPIError: respErr,
		}
	}
	usage, usageErr := coerceTestUsage(usageA, isStream, info.GetEstimatePromptTokens())
	if usageErr != nil {
		return testResult{
			context:     c,
			localErr:    usageErr,
			newAPIError: types.NewOpenAIError(usageErr, types.ErrorCodeBadResponseBody, http.StatusInternalServerError),
		}
	}
	result := w.Result()
	respBody, err := readTestResponseBody(result.Body, isStream)
	if err != nil {
		return testResult{
			context:     c,
			localErr:    err,
			newAPIError: types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError),
		}
	}
	if bodyErr := validateTestResponseBody(respBody, isStream); bodyErr != nil {
		return testResult{
			context:     c,
			localErr:    bodyErr,
			newAPIError: types.NewOpenAIError(bodyErr, types.ErrorCodeBadResponseBody, http.StatusInternalServerError),
		}
	}
	info.SetEstimatePromptTokens(usage.PromptTokens)

	quota, tieredResult := settleTestQuota(info, priceData, usage)
	tok := time.Now()
	milliseconds := tok.Sub(tik).Milliseconds()
	consumedTime := float64(milliseconds) / 1000.0
	other := buildTestLogOther(c, info, priceData, usage, tieredResult)
	model.RecordConsumeLog(c, testUserID, model.RecordConsumeLogParams{
		ChannelId:        channel.Id,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		ModelName:        info.OriginModelName,
		TokenName:        "模型测试",
		Quota:            quota,
		Content:          "模型测试",
		UseTimeSeconds:   int(consumedTime),
		IsStream:         info.IsStream,
		Group:            info.UsingGroup,
		Other:            other,
	})
	common.SysLog(fmt.Sprintf("testing channel #%d, response: \n%s", channel.Id, string(respBody)))
	return testResult{
		context:     c,
		localErr:    nil,
		newAPIError: nil,
	}
}

func attachTestBillingRequestInput(info *relaycommon.RelayInfo, request dto.Request) error {
	if info == nil {
		return nil
	}

	input, err := helper.BuildBillingExprRequestInputFromRequest(request, info.RequestHeaders)
	if err != nil {
		return err
	}
	info.BillingRequestInput = &input
	return nil
}

func settleTestQuota(info *relaycommon.RelayInfo, priceData types.PriceData, usage *dto.Usage) (int, *billingexpr.TieredResult) {
	if usage != nil && info != nil && info.TieredBillingSnapshot != nil {
		isClaudeUsageSemantic := usage.UsageSemantic == "anthropic" || info.GetFinalRequestRelayFormat() == types.RelayFormatClaude
		usedVars := billingexpr.UsedVars(info.TieredBillingSnapshot.ExprString)
		if ok, quota, result := service.TryTieredSettle(info, service.BuildTieredTokenParams(usage, isClaudeUsageSemantic, usedVars)); ok {
			return common.QuotaRound(float64(quota) * info.ChannelMeta.GetChannelRatio()), result
		}
	}

	channelRatio := info.ChannelMeta.GetChannelRatio()
	quota := 0
	if !priceData.UsePrice {
		quota = usage.PromptTokens + int(math.Round(float64(usage.CompletionTokens)*priceData.CompletionRatio))
		quota = common.QuotaRound(float64(quota) * priceData.ModelRatio * channelRatio)
		if priceData.ModelRatio != 0 && channelRatio != 0 && quota <= 0 {
			quota = 1
		}
		return quota, nil
	}

	return common.QuotaFromFloat(priceData.ModelPrice * common.QuotaPerUnit * channelRatio), nil
}

func buildTestLogOther(c *gin.Context, info *relaycommon.RelayInfo, priceData types.PriceData, usage *dto.Usage, tieredResult *billingexpr.TieredResult) map[string]interface{} {
	other := service.GenerateTextOtherInfo(c, info, priceData.ModelRatio, priceData.GroupRatioInfo.GroupRatio, priceData.CompletionRatio,
		usage.PromptTokensDetails.CachedTokens, priceData.CacheRatio, priceData.ModelPrice, priceData.GroupRatioInfo.GroupSpecialRatio)
	if tieredResult != nil {
		service.InjectTieredBillingInfo(other, info, tieredResult)
	}
	return other
}

func coerceTestUsage(usageAny any, isStream bool, estimatePromptTokens int) (*dto.Usage, error) {
	switch u := usageAny.(type) {
	case *dto.Usage:
		return u, nil
	case dto.Usage:
		return &u, nil
	case nil:
		if !isStream {
			return nil, errors.New("usage is nil")
		}
		usage := &dto.Usage{
			PromptTokens: estimatePromptTokens,
		}
		usage.TotalTokens = usage.PromptTokens
		return usage, nil
	default:
		if !isStream {
			return nil, fmt.Errorf("invalid usage type: %T", usageAny)
		}
		usage := &dto.Usage{
			PromptTokens: estimatePromptTokens,
		}
		usage.TotalTokens = usage.PromptTokens
		return usage, nil
	}
}

func readTestResponseBody(body io.ReadCloser, isStream bool) ([]byte, error) {
	defer func() { _ = body.Close() }()
	const maxStreamLogBytes = 8 << 10
	if isStream {
		return io.ReadAll(io.LimitReader(body, maxStreamLogBytes))
	}
	return io.ReadAll(body)
}

func detectErrorFromTestResponseBody(respBody []byte) error {
	b := bytes.TrimSpace(respBody)
	if len(b) == 0 {
		return nil
	}
	if message := detectErrorMessageFromJSONBytes(b); message != "" {
		return fmt.Errorf("upstream error: %s", message)
	}

	for _, line := range bytes.Split(b, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if message := detectErrorMessageFromJSONBytes(payload); message != "" {
			return fmt.Errorf("upstream error: %s", message)
		}
	}

	return nil
}

func validateStreamTestResponseBody(respBody []byte) error {
	b := bytes.TrimSpace(respBody)
	if len(b) == 0 {
		return errors.New("stream response body is empty")
	}

	for _, line := range bytes.Split(b, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		return nil
	}

	return errors.New("stream response body does not contain a valid stream event")
}

func validateTestResponseBody(respBody []byte, isStream bool) error {
	if bodyErr := detectErrorFromTestResponseBody(respBody); bodyErr != nil {
		return bodyErr
	}
	if isStream {
		return validateStreamTestResponseBody(respBody)
	}
	return nil
}

func shouldUseStreamForAutomaticChannelTest(channel *model.Channel) bool {
	return channel != nil && channel.Type == constant.ChannelTypeCodex
}

func detectErrorMessageFromJSONBytes(jsonBytes []byte) string {
	if len(jsonBytes) == 0 {
		return ""
	}
	if jsonBytes[0] != '{' && jsonBytes[0] != '[' {
		return ""
	}
	errVal := gjson.GetBytes(jsonBytes, "error")
	if !errVal.Exists() || errVal.Type == gjson.Null {
		return ""
	}

	message := gjson.GetBytes(jsonBytes, "error.message").String()
	if message == "" {
		message = gjson.GetBytes(jsonBytes, "error.error.message").String()
	}
	if message == "" && errVal.Type == gjson.String {
		message = errVal.String()
	}
	if message == "" {
		message = errVal.Raw
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "upstream returned error payload"
	}
	return message
}

func buildTestRequest(model string, endpointType string, channel *model.Channel, isStream bool) dto.Request {
	testResponsesInput := json.RawMessage(`[{"role":"user","content":"hi"}]`)

	// 根据端点类型构建不同的测试请求
	if endpointType != "" {
		switch constant.EndpointType(endpointType) {
		case constant.EndpointTypeEmbeddings:
			// 返回 EmbeddingRequest
			return &dto.EmbeddingRequest{
				Model: model,
				Input: []any{"hello world"},
			}
		case constant.EndpointTypeImageGeneration:
			// 返回 ImageRequest
			return &dto.ImageRequest{
				Model:  model,
				Prompt: "a cute cat",
				N:      lo.ToPtr(uint(1)),
				Size:   "1024x1024",
			}
		case constant.EndpointTypeJinaRerank:
			// 返回 RerankRequest
			return &dto.RerankRequest{
				Model:     model,
				Query:     "What is Deep Learning?",
				Documents: []any{"Deep Learning is a subset of machine learning.", "Machine learning is a field of artificial intelligence."},
				TopN:      lo.ToPtr(2),
			}
		case constant.EndpointTypeOpenAIResponse:
			// 返回 OpenAIResponsesRequest
			return &dto.OpenAIResponsesRequest{
				Model:  model,
				Input:  json.RawMessage(`[{"role":"user","content":"hi"}]`),
				Stream: lo.ToPtr(isStream),
			}
		case constant.EndpointTypeOpenAIResponseCompact:
			// 返回 OpenAIResponsesCompactionRequest
			return &dto.OpenAIResponsesCompactionRequest{
				Model: model,
				Input: testResponsesInput,
			}
		case constant.EndpointTypeAnthropic, constant.EndpointTypeGemini, constant.EndpointTypeOpenAI:
			// 返回 GeneralOpenAIRequest
			maxTokens := uint(16)
			if constant.EndpointType(endpointType) == constant.EndpointTypeGemini {
				maxTokens = 3000
			}
			req := &dto.GeneralOpenAIRequest{
				Model:  model,
				Stream: lo.ToPtr(isStream),
				Messages: []dto.Message{
					{
						Role:    "user",
						Content: "hi",
					},
				},
				MaxTokens: lo.ToPtr(maxTokens),
			}
			if isStream {
				req.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
			}
			return req
		}
	}

	// 自动检测逻辑（保持原有行为）
	if strings.Contains(strings.ToLower(model), "rerank") {
		return &dto.RerankRequest{
			Model:     model,
			Query:     "What is Deep Learning?",
			Documents: []any{"Deep Learning is a subset of machine learning.", "Machine learning is a field of artificial intelligence."},
			TopN:      lo.ToPtr(2),
		}
	}

	// 先判断是否为 Embedding 模型
	if strings.Contains(strings.ToLower(model), "embedding") ||
		strings.HasPrefix(model, "m3e") ||
		strings.Contains(model, "bge-") {
		// 返回 EmbeddingRequest
		return &dto.EmbeddingRequest{
			Model: model,
			Input: []any{"hello world"},
		}
	}

	// Responses compaction models (must use /v1/responses/compact)
	if strings.HasSuffix(model, ratio_setting.CompactModelSuffix) {
		return &dto.OpenAIResponsesCompactionRequest{
			Model: model,
			Input: testResponsesInput,
		}
	}

	// Responses-only models (e.g. codex series)
	if strings.Contains(strings.ToLower(model), "codex") {
		return &dto.OpenAIResponsesRequest{
			Model:  model,
			Input:  json.RawMessage(`[{"role":"user","content":"hi"}]`),
			Stream: lo.ToPtr(isStream),
		}
	}

	// Chat/Completion 请求 - 返回 GeneralOpenAIRequest
	testRequest := &dto.GeneralOpenAIRequest{
		Model:  model,
		Stream: lo.ToPtr(isStream),
		Messages: []dto.Message{
			{
				Role:    "user",
				Content: "hi",
			},
		},
	}
	if isStream {
		testRequest.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
	}

	if dto.IsOpenAIReasoningOModel(model) {
		testRequest.MaxCompletionTokens = lo.ToPtr(uint(16))
	} else if strings.Contains(model, "thinking") {
		if !strings.Contains(model, "claude") {
			testRequest.MaxTokens = lo.ToPtr(uint(50))
		}
	} else if strings.Contains(model, "gemini") {
		testRequest.MaxTokens = lo.ToPtr(uint(3000))
	} else {
		testRequest.MaxTokens = lo.ToPtr(uint(16))
	}

	return testRequest
}

func TestChannel(c *gin.Context) {
	channelId, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	channel, err := model.CacheGetChannel(channelId)
	if err != nil {
		channel, err = model.GetChannelById(channelId, true)
		if err != nil {
			common.ApiError(c, err)
			return
		}
	}
	//defer func() {
	//	if channel.ChannelInfo.IsMultiKey {
	//		go func() { _ = channel.SaveChannelInfo() }()
	//	}
	//}()
	// 统一 trim：测试、非空判断、恢复键必须是同一个值，避免带空格参数导致
	// "测试的模型"与"恢复的模型"错位（禁用行存的是原始模型名）。
	testModel := strings.TrimSpace(c.Query("model"))
	endpointType := c.Query("endpoint_type")
	isStream, _ := strconv.ParseBool(c.Query("stream"))
	testUserID, err := resolveChannelTestUserID(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	tik := time.Now()
	requestCtx := context.Background()
	if c.Request != nil {
		requestCtx = c.Request.Context()
	}
	result := testChannel(requestCtx, channel, testUserID, testModel, endpointType, isStream)
	if result.localErr != nil {
		resp := gin.H{
			"success": false,
			"message": result.localErr.Error(),
			"time":    0.0,
		}
		if result.newAPIError != nil {
			resp["error_code"] = result.newAPIError.GetErrorCode()
		}
		c.JSON(http.StatusOK, resp)
		return
	}
	tok := time.Now()
	milliseconds := tok.Sub(tik).Milliseconds()
	go channel.UpdateResponseTime(milliseconds)
	consumedTime := float64(milliseconds) / 1000.0
	if result.newAPIError != nil {
		c.JSON(http.StatusOK, gin.H{
			"success":    false,
			"message":    result.newAPIError.Error(),
			"time":       consumedTime,
			"error_code": result.newAPIError.GetErrorCode(),
		})
		return
	}
	// 显式单模型测试成功 → 立即恢复该模型的 auto/relay 禁用（保留 manual），
	// 让"手动测试成功"真正等于"模型恢复上线"；不带 model 的渠道级测试不猜测恢复对象。
	// 成功语义 = DB 禁用行已清除，缓存重建 best-effort 由周期同步兜底；
	// 恢复事务失败必须返回失败——测试成功但模型仍下线不能报成功。
	if testModel != "" {
		if recoverErr := service.HandleConfirmedProbeResult(channel.Id, testModel, true, "", int(milliseconds), 1); recoverErr != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": fmt.Sprintf("模型测试成功，但恢复上线状态失败: %s", recoverErr.Error()),
				"time":    consumedTime,
			})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"time":    consumedTime,
	})
}

// channelTestSummary records the outcome of one channel test cycle so the
// system task can persist a per-run result for history.
type channelTestSummary struct {
	Tested    int `json:"tested"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Disabled  int `json:"disabled"`
	Enabled   int `json:"enabled"`
}

func (summary *channelTestSummary) add(other channelTestSummary) {
	summary.Tested += other.Tested
	summary.Succeeded += other.Succeeded
	summary.Failed += other.Failed
	summary.Disabled += other.Disabled
	summary.Enabled += other.Enabled
}

// 不支持 testChannel 自动探测的渠道类型（Midjourney / 视频类等）
var unsupportedProbeChannelTypes = []int{
	constant.ChannelTypeMidjourney,
	constant.ChannelTypeMidjourneyPlus,
	constant.ChannelTypeSunoAPI,
	constant.ChannelTypeKling,
	constant.ChannelTypeJimeng,
	constant.ChannelTypeDoubaoVideo,
	constant.ChannelTypeVidu,
}

func isUnsupportedProbeChannelType(channelType int) bool {
	return lo.Contains(unsupportedProbeChannelTypes, channelType)
}

func shouldProbeChannelModels(channel *model.Channel) bool {
	if channel == nil {
		return false
	}
	if channel.Status == common.ChannelStatusManuallyDisabled {
		return false
	}
	return channel.Type == constant.ChannelTypeAnthropic &&
		!isUnsupportedProbeChannelType(channel.Type) &&
		service.IsClaudeAgentProbeConfigured()
}

func getModelHealthProbeEndpointType(channel *model.Channel) string {
	// 与后台弹窗模型测试保持一致：不指定 endpoint_type，让 testChannel 自动检测。
	return ""
}

// probeResult 是单次 (channel, model) 健康探测的归一化结果。
type probeResult struct {
	success    bool
	errMsg     string
	latencyMs  int
	attempts   int
	isLocalErr bool // localErr != nil && newAPIError == nil → 跳过状态机
	isSoftErr  bool // 上游临时限流/过载/无可用资源 → 重试确认后进入状态机
	timedOut   bool // 触发硬超时
}

func probeTimeoutResult(timeoutSec int) probeResult {
	return probeResult{
		success:   false,
		errMsg:    fmt.Sprintf("probe timeout after %ds", timeoutSec),
		latencyMs: timeoutSec * 1000,
		timedOut:  true,
	}
}

// runProbe 执行单次健康探测，带硬超时保护。
func runProbe(ctx context.Context, testUserID int, ch *model.Channel, modelName string, timeoutSec int) probeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeoutSec <= 0 {
		timeoutSec = 20
	}
	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	resultCh := make(chan testResult, 1)
	go func() {
		// 流式规则与渠道级自动测试对齐：Codex 类渠道必须流式，其余（含 Anthropic）非流式
		resultCh <- testChannel(probeCtx, ch, testUserID, modelName, getModelHealthProbeEndpointType(ch), shouldUseStreamForAutomaticChannelTest(ch))
	}()

	select {
	case r := <-resultCh:
		latency := int(time.Since(start).Milliseconds())
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return probeTimeoutResult(timeoutSec)
		}
		if r.newAPIError != nil {
			return probeResult{
				success:   false,
				errMsg:    r.newAPIError.Error(),
				latencyMs: latency,
				isSoftErr: service.IsSoftModelHealthError(r.newAPIError),
			}
		}
		if r.localErr != nil {
			return probeResult{
				success:    false,
				errMsg:     "local: " + r.localErr.Error(),
				latencyMs:  latency,
				isLocalErr: true,
			}
		}
		return probeResult{
			success:   true,
			latencyMs: latency,
		}
	case <-probeCtx.Done():
		if ctx.Err() != nil {
			return probeResult{
				success:    false,
				errMsg:     "local: " + ctx.Err().Error(),
				latencyMs:  int(time.Since(start).Milliseconds()),
				isLocalErr: true,
			}
		}
		return probeTimeoutResult(timeoutSec)
	}
}

type probeFunc func(ctx context.Context, ch *model.Channel, modelName string, timeoutSec int) probeResult

// probeModelWithImmediateRetries 在同一轮巡检内立即确认失败。
//
// 语义:
//   - 成功一次即返回成功
//   - localErr 属于本地不支持/构造失败，不重试、不进入状态机
//   - 上游错误或超时会立即重试，直到达到 maxAttempts
func probeModelWithImmediateRetries(
	ctx context.Context,
	ch *model.Channel,
	modelName string,
	maxAttempts int,
	timeoutSec int,
	probe probeFunc,
) probeResult {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var last probeResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx != nil && ctx.Err() != nil {
			return probeResult{
				success:    false,
				errMsg:     "local: " + ctx.Err().Error(),
				isLocalErr: true,
				attempts:   attempt - 1,
			}
		}
		last = probe(ctx, ch, modelName, timeoutSec)
		last.attempts = attempt
		if last.success || last.isLocalErr {
			return last
		}
	}
	return last
}

// shouldSkipProbeHealthStateMachine 判定探测结果是否跳过健康状态机：
//   - localErr（本地不支持/构造失败）永远跳过，只更新观测；
//   - recoveryOnly（被动恢复模式）下失败也跳过——目标本就处于禁用态，无需再写禁用，
//     且不能改写禁用行的 relay 原始 source/reason；成功仍进状态机触发恢复。
func shouldSkipProbeHealthStateMachine(result probeResult, recoveryOnly bool) bool {
	if result.isLocalErr {
		return true
	}
	return recoveryOnly && !result.success
}

// getModelProbeTimeoutSec 模型级探针超时（秒），passive 与 scheduled_all 探针共享。
// 读专用配置 channel_health_setting.probe_timeout_sec（默认 20s）；此前误用
// ChannelDisableThreshold（5s 响应时间禁用阈值），SDK 冷启动探针必然超时误判。
func getModelProbeTimeoutSec() int {
	timeout := operation_setting.GetChannelHealthProbeTimeoutSec()
	if timeout <= 0 {
		return 20
	}
	return timeout
}

type channelModelProbeTarget struct {
	channel *model.Channel
	model   string
}

type channelModelProbeGroup struct {
	channel *model.Channel
	models  []string
}

func buildScheduledProbeTargets(channels []*model.Channel) []channelModelProbeTarget {
	targets := make([]channelModelProbeTarget, 0)
	for _, channel := range channels {
		if !shouldProbeChannelModels(channel) {
			continue
		}
		seen := make(map[string]bool)
		for _, rawModel := range channel.GetModels() {
			modelName := strings.TrimSpace(rawModel)
			if modelName == "" || seen[modelName] {
				continue
			}
			seen[modelName] = true
			targets = append(targets, channelModelProbeTarget{
				channel: channel,
				model:   modelName,
			})
		}
	}
	return targets
}

func buildPassiveRecoveryProbeTargets(channels []*model.Channel) ([]channelModelProbeTarget, error) {
	channelByID := make(map[int]*model.Channel, len(channels))
	for _, channel := range channels {
		if channel != nil {
			channelByID[channel.Id] = channel
		}
	}

	var disabledRows []model.ChannelModelDisabled
	if err := model.DB.Where(
		"source IN ?",
		[]string{model.DisabledSourceAuto, model.DisabledSourceRelay},
	).Order("channel_id ASC, model ASC").Find(&disabledRows).Error; err != nil {
		return nil, err
	}

	targets := make([]channelModelProbeTarget, 0, len(disabledRows))
	for _, row := range disabledRows {
		channel := channelByID[row.ChannelId]
		// 被动恢复覆盖全部常规渠道类型：Anthropic 走探针短路，其余走 testChannel
		// 真实最小请求；仅排除 testChannel 不支持的 Midjourney/视频类渠道。
		if channel == nil || channel.Status != common.ChannelStatusEnabled || isUnsupportedProbeChannelType(channel.Type) {
			continue
		}
		modelName := strings.TrimSpace(row.Model)
		if modelName == "" {
			continue
		}
		targets = append(targets, channelModelProbeTarget{
			channel: channel,
			model:   modelName,
		})
	}
	return targets, nil
}

func groupProbeTargetsByChannel(targets []channelModelProbeTarget) []channelModelProbeGroup {
	groups := make([]channelModelProbeGroup, 0)
	groupByChannelID := make(map[int]int)
	for _, target := range targets {
		if target.channel == nil || strings.TrimSpace(target.model) == "" {
			continue
		}
		index, ok := groupByChannelID[target.channel.Id]
		if !ok {
			index = len(groups)
			groupByChannelID[target.channel.Id] = index
			groups = append(groups, channelModelProbeGroup{channel: target.channel})
		}
		groups[index].models = append(groups[index].models, target.model)
	}
	return groups
}

// runAllChannelModelProbes 跨渠道并发、渠道内串行地探测模型健康。
//
// 两种语义：
//   - scheduled_all（recoveryOnly=false）：保留老 fork 巡检语义，失败模型同轮立即
//     重试确认（最多 FailureThreshold 次），确认结果写入状态机（可禁用可恢复）。
//   - passive_recovery（recoveryOnly=true）：目标本就是已禁用模型，每轮只探 1 次；
//     失败不进状态机（只更新观测，不改写禁用行的 source/reason），成功一次即恢复。
func runAllChannelModelProbes(ctx context.Context, targets []channelModelProbeTarget, testUserID int, recoveryOnly bool, report func(processed, total int)) channelTestSummary {
	if ctx == nil {
		ctx = context.Background()
	}
	summary := channelTestSummary{}
	total := len(targets)
	if report != nil {
		report(0, total)
	}
	if total == 0 {
		return summary
	}

	concurrency := operation_setting.GetChannelHealthConcurrency()
	maxAttempts := operation_setting.GetChannelHealthFailureThreshold()
	if recoveryOnly {
		maxAttempts = 1
	}
	probeTimeout := getModelProbeTimeoutSec()
	sem := semaphore.NewWeighted(int64(concurrency))
	groups := groupProbeTargetsByChannel(targets)

	var wg sync.WaitGroup
	var mu sync.Mutex
	processed := 0
	runSingleProbe := func(probeCtx context.Context, ch *model.Channel, modelName string, timeoutSec int) probeResult {
		return runProbe(probeCtx, testUserID, ch, modelName, timeoutSec)
	}

	for _, group := range groups {
		if ctx.Err() != nil {
			break
		}
		if err := sem.Acquire(ctx, 1); err != nil {
			if ctx.Err() == nil {
				common.SysError(fmt.Sprintf("[channel_health] sem.Acquire failed: %v", err))
			}
			break
		}
		wg.Add(1)

		go func(group channelModelProbeGroup) {
			defer wg.Done()
			defer sem.Release(1)

			for _, modelName := range group.models {
				if ctx.Err() != nil {
					return
				}

				p := probeModelWithImmediateRetries(ctx, group.channel, modelName, maxAttempts, probeTimeout, runSingleProbe)
				if ctx.Err() != nil {
					return
				}

				skipStateMachine := shouldSkipProbeHealthStateMachine(p, recoveryOnly)
				wasDisabled := false
				if !skipStateMachine {
					disabled, err := model.IsChannelModelDisabled(group.channel.Id, modelName)
					if err != nil {
						common.SysError(fmt.Sprintf(
							"[channel_health] IsChannelModelDisabled before probe failed, channel=%d model=%s: %v",
							group.channel.Id, modelName, err,
						))
					} else {
						wasDisabled = disabled
					}
				}

				isDisabled := wasDisabled
				if skipStateMachine {
					if err := model.UpdateHealthObservability(group.channel.Id, modelName, p.errMsg, p.latencyMs); err != nil {
						common.SysError(fmt.Sprintf(
							"[channel_health] UpdateHealthObservability failed, channel=%d model=%s: %v",
							group.channel.Id, modelName, err,
						))
					}
				} else {
					// 事务失败已在 service 内记日志，巡检路径下轮自然重试，无需中断本轮
					_ = service.HandleConfirmedProbeResult(group.channel.Id, modelName, p.success, p.errMsg, p.latencyMs, p.attempts)
					group.channel.UpdateResponseTime(int64(p.latencyMs))
					disabled, err := model.IsChannelModelDisabled(group.channel.Id, modelName)
					if err != nil {
						common.SysError(fmt.Sprintf(
							"[channel_health] IsChannelModelDisabled after probe failed, channel=%d model=%s: %v",
							group.channel.Id, modelName, err,
						))
					} else {
						isDisabled = disabled
					}
				}

				mu.Lock()
				summary.Tested++
				if p.success {
					summary.Succeeded++
					if !skipStateMachine && wasDisabled && !isDisabled {
						summary.Enabled++
					}
				} else {
					summary.Failed++
					if !skipStateMachine && !wasDisabled && isDisabled {
						summary.Disabled++
					}
				}
				processed++
				if report != nil {
					report(processed, total)
				}
				mu.Unlock()

				if common.RequestInterval > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(common.RequestInterval):
					}
				}
			}
		}(group)
	}

	wg.Wait()
	return summary
}

// performChannelTests runs the channel test loop synchronously, honoring ctx
// cancellation so a system-task runner that loses its lease stops promptly. When
// report is non-nil it is called after each channel with (processed, total) so
// the system task can surface progress.
func performChannelTests(ctx context.Context, channels []*model.Channel, testUserID int, allowDisable bool, report func(processed, total int)) channelTestSummary {
	summary := channelTestSummary{}
	var disableThreshold = int64(common.ChannelDisableThreshold * 1000)
	if disableThreshold == 0 {
		disableThreshold = 10000000 // a impossible value
	}

	total := len(channels)
	for index, channel := range channels {
		if ctx != nil && ctx.Err() != nil {
			break
		}
		if report != nil {
			report(index, total) // channels completed before this one
		}
		if channel.Status == common.ChannelStatusManuallyDisabled {
			continue
		}
		isChannelEnabled := channel.Status == common.ChannelStatusEnabled
		tik := time.Now()
		result := testChannel(ctx, channel, testUserID, "", "", shouldUseStreamForAutomaticChannelTest(channel))
		tok := time.Now()
		milliseconds := tok.Sub(tik).Milliseconds()
		if ctx != nil && ctx.Err() != nil {
			break
		}

		summary.Tested++

		shouldBanChannel := false
		newAPIError := result.newAPIError
		// request error disables the channel
		if newAPIError != nil {
			shouldBanChannel = service.ShouldDisableChannel(result.newAPIError)
		}

		// 当错误检查通过，才检查响应时间
		if common.AutomaticDisableChannelEnabled && !shouldBanChannel {
			if milliseconds > disableThreshold {
				err := fmt.Errorf("响应时间 %.2fs 超过阈值 %.2fs", float64(milliseconds)/1000.0, float64(disableThreshold)/1000.0)
				newAPIError = types.NewOpenAIError(err, types.ErrorCodeChannelResponseTimeExceeded, http.StatusRequestTimeout)
				shouldBanChannel = true
			}
		}

		if newAPIError == nil {
			summary.Succeeded++
		} else {
			summary.Failed++
		}

		// disable channel
		if allowDisable && isChannelEnabled && shouldBanChannel && channel.GetAutoBan() {
			processChannelError(result.context, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(result.context, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)
			summary.Disabled++
		}

		// enable channel
		if result.localErr == nil && !isChannelEnabled && service.ShouldEnableChannel(newAPIError, channel.Status) {
			service.EnableChannel(channel.Id, common.GetContextKeyString(result.context, constant.ContextKeyChannelKey), channel.Name)
			summary.Enabled++
		}

		channel.UpdateResponseTime(milliseconds)
		if common.RequestInterval > 0 {
			if ctx == nil {
				time.Sleep(common.RequestInterval)
			} else {
				select {
				case <-ctx.Done():
					return summary
				case <-time.After(common.RequestInterval):
				}
			}
		}
	}
	if report != nil && (ctx == nil || ctx.Err() == nil) {
		report(total, total) // mark complete only when the full set was tested
	}
	return summary
}

// runChannelTestTask runs one synchronous channel test cycle for the system task
// runner (both the scheduled job and the manual "test all channels" trigger go
// through here). It honors ctx cancellation so a runner that loses its lease
// stops promptly. mode selects the channel set: an empty mode falls back to the
// configured monitor ChannelTestMode (scheduled behavior), while a manual
// trigger passes ChannelTestModeScheduledAll to test every channel. When notify
// is set the root user is notified on completion. Cross-instance execution is
// guarded by the system task per-type lock, so no process-local guard is needed.
func runChannelTestTask(ctx context.Context, mode string, notify bool, report func(processed, total int)) (channelTestSummary, error) {
	testUserID, err := resolveChannelTestUserID(nil)
	if err != nil {
		return channelTestSummary{}, err
	}
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		return channelTestSummary{}, err
	}
	if strings.TrimSpace(mode) == "" {
		mode = operation_setting.GetMonitorSetting().ChannelTestMode
	}
	selected := selectChannelsForAutomaticTest(channels, mode)
	probeChannels, relayChannels := splitChannelsByProbe(selected)
	probeTargets := buildScheduledProbeTargets(probeChannels)
	recoveryOnly := mode == operation_setting.ChannelTestModePassiveRecovery
	if recoveryOnly {
		// 被动恢复只做模型级探活：不再对 status=AutoDisabled 的整渠道发默认模型测试，
		// 探活目标独立从 channel_model_disabled(source IN auto,relay) 生成。
		relayChannels = nil
		probeTargets, err = buildPassiveRecoveryProbeTargets(channels)
		if err != nil {
			return channelTestSummary{}, err
		}
	}

	totalWork := len(relayChannels) + len(probeTargets)
	if report != nil && totalWork == 0 {
		report(0, 0)
	}
	reportWithOffset := func(offset int) func(processed, total int) {
		if report == nil {
			return nil
		}
		return func(processed, _ int) {
			report(offset+processed, totalWork)
		}
	}

	allowDisable := !recoveryOnly
	summary := performChannelTests(ctx, relayChannels, testUserID, allowDisable, reportWithOffset(0))
	if ctx != nil && ctx.Err() != nil {
		return summary, nil
	}
	probeSummary := runAllChannelModelProbes(ctx, probeTargets, testUserID, recoveryOnly, reportWithOffset(len(relayChannels)))
	summary.add(probeSummary)
	if notify && (ctx == nil || ctx.Err() == nil) {
		service.NotifyRootUser(dto.NotifyTypeChannelTest, "通道测试完成", "所有通道测试已完成")
	}
	return summary, nil
}

func splitChannelsByProbe(channels []*model.Channel) ([]*model.Channel, []*model.Channel) {
	probeChannels := make([]*model.Channel, 0, len(channels))
	relayChannels := make([]*model.Channel, 0, len(channels))
	for _, channel := range channels {
		if shouldProbeChannelModels(channel) {
			probeChannels = append(probeChannels, channel)
			continue
		}
		relayChannels = append(relayChannels, channel)
	}
	return probeChannels, relayChannels
}

func selectChannelsForAutomaticTest(channels []*model.Channel, mode string) []*model.Channel {
	selected := make([]*model.Channel, 0, len(channels))
	for _, channel := range channels {
		if channel.Status == common.ChannelStatusManuallyDisabled {
			continue
		}
		if mode == operation_setting.ChannelTestModePassiveRecovery && channel.Status != common.ChannelStatusAutoDisabled {
			continue
		}
		selected = append(selected, channel)
	}
	return selected
}

// TestAllChannels enqueues a channel_test system task instead of running the
// test loop inline. If any channel_test task is already active, the manual run is
// rejected so the caller does not mistake a scheduled run for this manual one.
func TestAllChannels(c *gin.Context) {
	task, created, err := service.EnqueueSystemTask(model.SystemTaskTypeChannelTest, channelTestTaskPayload{
		Mode:   operation_setting.ChannelTestModeScheduledAll,
		Notify: true,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !created {
		c.JSON(http.StatusConflict, gin.H{
			"success": false,
			"message": "已有通道测试任务正在运行或等待中，不能启动本次手动任务",
			"data": gin.H{
				"task_id": task.TaskID,
				"status":  task.Status,
				"type":    task.Type,
			},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"task_id": task.TaskID,
			"status":  task.Status,
		},
	})
}
