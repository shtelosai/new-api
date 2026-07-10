package model

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
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

// lockChannelModelDisabledRow 与 lockChannelModelHealthRow 同一模式：
// MySQL/PG 用 SELECT FOR UPDATE 行锁，SQLite 退化为普通事务查询。
func lockChannelModelDisabledRow(tx *gorm.DB, channelId int, modelName string) *gorm.DB {
	query := tx.Where("channel_id = ? AND model = ?", channelId, modelName)
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		return query
	}
	return query.Clauses(clause.Locking{Strength: "UPDATE"})
}

// UpsertChannelModelDisabledPreservingManual 写入/覆盖禁用记录，但保留 manual 人工锁：
// 已有行 source=manual 时不做任何覆盖（返回 changed=false）；auto/relay 行照常覆盖。
// manual 是可靠人工锁的前提——自动恢复只清 auto/relay，若 relay 覆盖 manual，
// 探活成功后人工禁用会被静默清除。
func UpsertChannelModelDisabledPreservingManual(channelId int, modelName, source, reason string) (bool, error) {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existing ChannelModelDisabled
		err := lockChannelModelDisabledRow(tx, channelId, modelName).First(&existing).Error
		if err == nil {
			if existing.Source == DisabledSourceManual {
				return nil // 保留人工锁，不覆盖
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row := ChannelModelDisabled{
			ChannelId:  channelId,
			Model:      modelName,
			Source:     source,
			Reason:     reason,
			DisabledAt: time.Now().Unix(),
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "channel_id"}, {Name: "model"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"source", "reason", "disabled_at",
			}),
		}).Create(&row).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// ClearChannelModelDisabledInTx 显式管理动作「解除禁用」：单事务删除该 (channel, model)
// 禁用行（任意 source，含 manual——区别于自动恢复）并重置健康失败状态，避免解除后
// 状态列仍按旧 LastError 展示异常。返回被删除行的 source（无行时 changed=false）。
func ClearChannelModelDisabledInTx(channelId int, modelName string) (previousSource string, changed bool, err error) {
	txErr := DB.Transaction(func(tx *gorm.DB) error {
		var existing ChannelModelDisabled
		findErr := lockChannelModelDisabledRow(tx, channelId, modelName).First(&existing).Error
		if findErr != nil {
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				return nil // 幂等：行不存在视为成功
			}
			return findErr
		}
		previousSource = existing.Source
		if err := tx.Where(
			"channel_id = ? AND model = ?", channelId, modelName,
		).Delete(&ChannelModelDisabled{}).Error; err != nil {
			return err
		}
		// 重置健康失败计数与错误观测（保留 LastSuccessAt，状态自然回落 unknown/healthy）
		if err := tx.Model(&ChannelModelHealth{}).
			Where("channel_id = ? AND model = ?", channelId, modelName).
			Updates(map[string]interface{}{
				"consecutive_failures":  0,
				"consecutive_successes": 0,
				"last_error":            "",
			}).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	return previousSource, changed, txErr
}
