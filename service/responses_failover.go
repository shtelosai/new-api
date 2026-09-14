package service

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// TworkResponsesFailoverEnabled 仅允许携带完整可移植历史的普通模型选路切换渠道。
// 固定供应商、旧 Codex 有状态请求及 compact 均保留原来的边界。
func TworkResponsesFailoverEnabled(c *gin.Context) bool {
	if !operation_setting.IsSoftFailureCooldownEnabled() || c == nil || c.Request == nil || c.Request.URL.Path != "/v1/responses" {
		return false
	}
	if _, fixed := c.Get("specific_channel_id"); fixed {
		return false
	}
	if _, fixed := c.Get(string(constant.ContextKeyTokenSpecificChannelId)); fixed {
		return false
	}
	if common.GetContextKeyBool(c, constant.ContextKeyTworkExplicitChannelRoute) {
		return false
	}
	policy, ok := common.GetContextKeyType[model.TworkRoutePolicy](c, constant.ContextKeyTworkRoutePolicy)
	return ok && policy.ModelRoute && policy.WireAPI == "responses"
}

// IsRelaySoftFailure 保留 Claude 的分类；Responses 另外将上游连接及服务故障纳入冷却。
func IsRelaySoftFailure(c *gin.Context, err *types.NewAPIError) bool {
	if !TworkResponsesFailoverEnabled(c) {
		return IsSoftModelHealthError(err)
	}
	if err == nil || (c.Request != nil && c.Request.Context().Err() != nil) {
		return false
	}
	oai := err.ToOpenAIError()
	if isHardModelHealthError(normalizeModelHealthText(oai.Type), normalizeModelHealthText(openAIErrorCodeToString(oai.Code)), normalizeModelHealthText(oai.Message)) {
		return false
	}
	// 本地参数、计费及配置错误不作为渠道临时故障。
	code := string(err.GetErrorCode())
	if strings.HasPrefix(code, "channel:") || types.IsSkipRetryError(err) {
		return false
	}
	if err.GetErrorCode() == types.ErrorCodeDoRequestFailed || err.GetErrorCode() == types.ErrorCodeBadResponseBody || err.GetErrorCode() == types.ErrorCodeReadResponseBodyFailed {
		return true
	}
	return err.GetErrorType() == types.ErrorTypeOpenAIError && (err.StatusCode == http.StatusTooManyRequests || err.StatusCode >= 500 && err.StatusCode <= 599)
}
