package operation_setting

import (
	"github.com/QuantumNous/new-api/setting/config"
)

// ChannelHealthSetting 渠道×模型级健康监控配置
// 被 HandleTestResult / DisableChannelModelFromRelay 引用
type ChannelHealthSetting struct {
	// 是否允许真实 relay 失败写入渠道×模型级禁用记录
	ModelLevelAutoDisableEnabled bool `json:"model_level_auto_disable_enabled"`
	// 连续失败阈值：健康检查连续失败达此值才写入禁用表（防抖）
	FailureThreshold int `json:"failure_threshold"`
	// 连续成功阈值：unhealthy 连续成功达此值才清理 auto/relay 禁用（防抖）
	SuccessThreshold int `json:"success_threshold"`
	// 跨渠道并发度：同时进行健康检查的渠道数上限
	Concurrency int `json:"concurrency"`
	// 单个 probe 硬超时（秒）：超过即判定失败并释放 semaphore
	ProbeTimeoutSec int `json:"probe_timeout_sec"`
}

var channelHealthSetting = ChannelHealthSetting{
	ModelLevelAutoDisableEnabled: true,
	FailureThreshold:             3,
	SuccessThreshold:             2,
	Concurrency:                  5,
	ProbeTimeoutSec:              20,
}

func init() {
	config.GlobalConfig.Register("channel_health_setting", &channelHealthSetting)
}

func GetChannelHealthSetting() *ChannelHealthSetting {
	return &channelHealthSetting
}

// 便捷访问器（用于 hot-path，避免每次取 struct）

func IsChannelModelAutoDisableEnabled() bool {
	return channelHealthSetting.ModelLevelAutoDisableEnabled
}

func GetChannelHealthFailureThreshold() int {
	if channelHealthSetting.FailureThreshold <= 0 {
		return 3
	}
	return channelHealthSetting.FailureThreshold
}

func GetChannelHealthSuccessThreshold() int {
	if channelHealthSetting.SuccessThreshold <= 0 {
		return 2
	}
	return channelHealthSetting.SuccessThreshold
}

func GetChannelHealthConcurrency() int {
	if channelHealthSetting.Concurrency <= 0 {
		return 5
	}
	return channelHealthSetting.Concurrency
}

func GetChannelHealthProbeTimeoutSec() int {
	if channelHealthSetting.ProbeTimeoutSec <= 0 {
		return 20
	}
	return channelHealthSetting.ProbeTimeoutSec
}
