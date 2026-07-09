package service

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// HandleTestResult 健康检查结果入状态机
//
// 语义：
//   - success=true  + 连续成功达阈值 → 清理 (source IN 'auto','relay') 的禁用行
//   - success=false + 连续失败达阈值 → 写入 disabled(source='auto')
//   - manual 禁用的行永远不会被本函数清除（保留人工语义）
//
// 并发安全：底层 ApplyTestResultInTx 在 MySQL/PG 使用单事务 + SELECT FOR UPDATE 行锁。
func HandleTestResult(channelId int, modelName string, success bool, errMsg string, latencyMs int) {
	failN := operation_setting.GetChannelHealthFailureThreshold()
	successM := operation_setting.GetChannelHealthSuccessThreshold()

	action, err := model.ApplyTestResultInTx(
		channelId, modelName,
		success, errMsg, latencyMs,
		failN, successM,
	)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"[channel_health] ApplyTestResultInTx failed, channel=%d model=%s: %v",
			channelId, modelName, err,
		))
		return
	}

	switch action {
	case model.HealthActionDisabled:
		model.RemoveChannelModelFromCache(channelId, modelName)
		common.SysLog(fmt.Sprintf(
			"[channel_health] auto-disable channel=%d model=%s after %d consecutive failures: %s",
			channelId, modelName, failN, errMsg,
		))
	case model.HealthActionRecovered:
		model.InitChannelCache()
		common.SysLog(fmt.Sprintf(
			"[channel_health] auto-recover channel=%d model=%s after %d consecutive successes",
			channelId, modelName, successM,
		))
	}
}

// HandleConfirmedProbeResult 处理同一轮内已确认的模型健康探测结果
//
// 语义：
//   - success=true：成功一次即恢复 auto/relay 禁用
//   - success=false：调用方已经完成连续失败确认，立即写 disabled(source='auto')
func HandleConfirmedProbeResult(channelId int, modelName string, success bool, errMsg string, latencyMs int, attempts int) {
	action, err := model.ApplyConfirmedProbeResultInTx(
		channelId, modelName,
		success, errMsg, latencyMs,
		attempts,
	)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"[channel_health] ApplyConfirmedProbeResultInTx failed, channel=%d model=%s: %v",
			channelId, modelName, err,
		))
		return
	}

	switch action {
	case model.HealthActionDisabled:
		model.RemoveChannelModelFromCache(channelId, modelName)
		common.SysLog(fmt.Sprintf(
			"[channel_health] auto-disable channel=%d model=%s after %d immediate failures: %s",
			channelId, modelName, attempts, errMsg,
		))
	case model.HealthActionRecovered:
		model.InitChannelCache()
		common.SysLog(fmt.Sprintf(
			"[channel_health] auto-recover channel=%d model=%s after one successful probe",
			channelId, modelName,
		))
	}
}

// DisableChannelModelFromRelay Relay 真实请求失败触发的即时模型级禁用（不防抖）
//
// 语义：
//   - 立即写入 disabled(source='relay')，覆盖 (auto|relay) 的任何已有禁用
//   - 同步把 channel_model_health 计数设到失败阈值，避免下一轮健康检查因"已禁用"
//     却只失败 1~2 次而意外触发恢复
//
// 注意：manual 禁用存在时也会被 Upsert 覆盖为 relay——这里是有意的，因为真实
//
//	流量失败是比 manual 更强的信号；人工想锁死请把 channels.status 改为 2。
func DisableChannelModelFromRelay(channelId int, modelName, reason string) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return
	}
	if !operation_setting.IsChannelModelAutoDisableEnabled() {
		common.SysLog(fmt.Sprintf(
			"[channel_health] relay-disable skipped by channel_health_setting, channel=%d model=%s",
			channelId, modelName,
		))
		return
	}

	if err := model.UpsertChannelModelDisabled(
		channelId, modelName, model.DisabledSourceRelay, reason,
	); err != nil {
		common.SysError(fmt.Sprintf(
			"[channel_health] UpsertChannelModelDisabled failed, channel=%d model=%s: %v",
			channelId, modelName, err,
		))
		return
	}
	model.RemoveChannelModelFromCache(channelId, modelName)

	// 把计数推到失败阈值，防止健康检查误恢复
	failN := operation_setting.GetChannelHealthFailureThreshold()
	successM := operation_setting.GetChannelHealthSuccessThreshold()
	for i := 0; i < failN; i++ {
		_, _ = model.ApplyTestResultInTx(
			channelId, modelName,
			false, reason, 0,
			failN, successM,
		)
	}

	common.SysLog(fmt.Sprintf(
		"[channel_health] relay-disable channel=%d model=%s: %s",
		channelId, modelName, reason,
	))
}
