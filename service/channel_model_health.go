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
//
// 返回值 error 表示恢复/禁用事务本身失败（禁用状态未变更）；手动测试路径据此
// 向管理员返回失败，避免"测试成功但未恢复上线"的假成功。成功语义 = DB 禁用行
// 已清除；内存缓存重建为 best-effort，由周期同步兜底（最终一致）。
func HandleConfirmedProbeResult(channelId int, modelName string, success bool, errMsg string, latencyMs int, attempts int) error {
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
		return err
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
	return nil
}

// DisableChannelModelFromRelay Relay 真实请求失败触发的即时模型级禁用（不防抖）
//
// 语义：
//   - 立即写入 disabled(source='relay')，覆盖 auto/relay 的已有禁用
//   - manual 禁用存在时**不覆盖**：人工锁必须保持可靠——被动探活只恢复 auto/relay，
//     若 manual 被在途异步失败/指定渠道请求覆盖成 relay，随后会被探活自动清除，
//     管理员意图将被静默撤销。manual 场景下模型本就已下线，这里只做幂等摘缓存。
//   - 同步把 channel_model_health 计数设到失败阈值，避免下一轮健康检查因"已禁用"
//     却只失败 1~2 次而意外触发恢复
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

	changed, err := model.UpsertChannelModelDisabledPreservingManual(
		channelId, modelName, model.DisabledSourceRelay, reason,
	)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"[channel_health] UpsertChannelModelDisabledPreservingManual failed, channel=%d model=%s: %v",
			channelId, modelName, err,
		))
		return
	}
	model.RemoveChannelModelFromCache(channelId, modelName)
	if !changed {
		common.SysLog(fmt.Sprintf(
			"[channel_health] relay-disable kept existing manual lock, channel=%d model=%s",
			channelId, modelName,
		))
		return
	}

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
