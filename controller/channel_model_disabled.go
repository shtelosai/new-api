package controller

import (
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// ClearChannelModelDisabled 显式「解除禁用」管理动作。
//
// 与自动恢复（只清 auto/relay）不同，这是管理员显式操作，删除该 (channel, model)
// 禁用行（任意 source，含 manual 人工锁）并重置健康失败状态，随后刷新渠道缓存。
// 这是 TTS/ASR/视频等无法构造探活请求的模型的唯一恢复通道。
// 幂等：行不存在也返回成功（changed=false）。
func ClearChannelModelDisabled(c *gin.Context) {
	channelId, err := strconv.Atoi(c.Query("channel_id"))
	if err != nil || channelId <= 0 {
		common.ApiErrorMsg(c, "无效的渠道 ID")
		return
	}
	modelName := strings.TrimSpace(c.Query("model"))
	if modelName == "" || len(modelName) > 255 {
		common.ApiErrorMsg(c, "无效的模型名")
		return
	}

	previousSource, changed, err := model.ClearChannelModelDisabledInTx(channelId, modelName)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if changed {
		model.InitChannelCache()
	}

	// 精确审计：解除人工锁必须可追责（generic 兜底审计不记录 query 参数）
	recordManageAudit(c, "channel.model_disabled_clear", map[string]interface{}{
		"id":              channelId,
		"model":           modelName,
		"previous_source": previousSource,
		"changed":         changed,
	})

	common.ApiSuccess(c, gin.H{
		"changed":         changed,
		"previous_source": previousSource,
	})
}
