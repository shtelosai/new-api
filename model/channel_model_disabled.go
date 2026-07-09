package model

import (
	"time"

	"gorm.io/gorm/clause"
)

// 禁用来源常量
const (
	DisabledSourceAuto   = "auto"   // 健康检查自动禁用
	DisabledSourceRelay  = "relay"  // 真实请求失败触发的禁用
	DisabledSourceManual = "manual" // 人工手动禁用
)

// ChannelModelDisabled 渠道×模型级别禁用黑名单
// 主键 (channel_id, model)：同一 (渠道, 模型) 只保留一行
// source 字段区分来源，自动恢复只清理 auto/relay，不动 manual
type ChannelModelDisabled struct {
	ChannelId  int    `json:"channel_id" gorm:"primaryKey;autoIncrement:false;index:idx_disabled_channel"`
	Model      string `json:"model" gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
	Source     string `json:"source" gorm:"type:varchar(16);index:idx_disabled_source"`
	Reason     string `json:"reason" gorm:"type:varchar(500)"`
	DisabledAt int64  `json:"disabled_at"`
}

func (ChannelModelDisabled) TableName() string {
	return "channel_model_disabled"
}

// GetAllChannelModelDisabled 全量读（供 InitChannelCache 构建黑名单）
func GetAllChannelModelDisabled() ([]*ChannelModelDisabled, error) {
	var list []*ChannelModelDisabled
	err := DB.Find(&list).Error
	return list, err
}

// UpsertChannelModelDisabled 写入或覆盖 (channel, model) 禁用记录
// 同一 (channel, model) 再次禁用时以最新 source/reason/disabled_at 覆盖
func UpsertChannelModelDisabled(channelId int, modelName, source, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	row := ChannelModelDisabled{
		ChannelId:  channelId,
		Model:      modelName,
		Source:     source,
		Reason:     reason,
		DisabledAt: time.Now().Unix(),
	}
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "channel_id"}, {Name: "model"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"source", "reason", "disabled_at",
		}),
	}).Create(&row).Error
}

// IsChannelModelDisabled 判断 (channel, model) 是否处于禁用状态
func IsChannelModelDisabled(channelId int, modelName string) (bool, error) {
	var count int64
	err := DB.Model(&ChannelModelDisabled{}).
		Where("channel_id = ? AND model = ?", channelId, modelName).
		Count(&count).Error
	return count > 0, err
}

// DeleteAutoOrRelayDisabled 只删除 source IN ('auto', 'relay') 的行
// 用于健康检查自动恢复流程，保留 manual 手工禁用
func DeleteAutoOrRelayDisabled(channelId int, modelName string) error {
	return DB.Where(
		"channel_id = ? AND model = ? AND source IN ?",
		channelId, modelName, []string{DisabledSourceAuto, DisabledSourceRelay},
	).Delete(&ChannelModelDisabled{}).Error
}

// DeleteChannelModelDisabledByChannel 清理某渠道下全部禁用记录（渠道删除时调用）
func DeleteChannelModelDisabledByChannel(channelId int) error {
	return DB.Where("channel_id = ?", channelId).
		Delete(&ChannelModelDisabled{}).Error
}

// DeleteChannelModelDisabledNotInModels 清理某渠道下 model 不在 keepModels 列表中的禁用行
// 用于渠道编辑时的模型列表变更（删除了某些 model → 清理对应禁用记录）
// keepModels 为空时视为清理全部
func DeleteChannelModelDisabledNotInModels(channelId int, keepModels []string) error {
	q := DB.Where("channel_id = ?", channelId)
	if len(keepModels) > 0 {
		q = q.Where("model NOT IN ?", keepModels)
	}
	return q.Delete(&ChannelModelDisabled{}).Error
}
