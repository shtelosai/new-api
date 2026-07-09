package model

import (
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChannelModelHealth 渠道×模型级健康监控状态
// 主键 (channel_id, model)：每个 (渠道, 模型) 一行
// 防抖计数用于健康检查路径达阈值时写/删 channel_model_disabled
type ChannelModelHealth struct {
	ChannelId            int    `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
	Model                string `json:"model" gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
	ConsecutiveFailures  int    `json:"consecutive_failures"`
	ConsecutiveSuccesses int    `json:"consecutive_successes"`
	LastTestedAt         int64  `json:"last_tested_at"`
	LastSuccessAt        int64  `json:"last_success_at"`
	LastError            string `json:"last_error" gorm:"type:varchar(1000)"`
	LatencyMs            int    `json:"latency_ms"`
}

type ChannelModelStatus struct {
	Model  string `json:"model"`
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (ChannelModelHealth) TableName() string {
	return "channel_model_health"
}

const (
	ChannelModelStatusHealthy  = "healthy"
	ChannelModelStatusDisabled = "disabled"
	ChannelModelStatusUnknown  = "unknown"
)

type channelModelStatusKey struct {
	channelId int
	model     string
}

func isChannelModelHealthHealthy(h ChannelModelHealth) bool {
	return h.LastSuccessAt > 0 && h.ConsecutiveFailures == 0 && strings.TrimSpace(h.LastError) == ""
}

func AttachChannelModelStatuses(channels []*Channel) error {
	if len(channels) == 0 {
		return nil
	}

	channelIds := make([]int, 0, len(channels))
	seen := make(map[int]bool, len(channels))
	for _, ch := range channels {
		if ch == nil || seen[ch.Id] {
			continue
		}
		seen[ch.Id] = true
		channelIds = append(channelIds, ch.Id)
	}
	if len(channelIds) == 0 {
		return nil
	}

	var disabledRows []ChannelModelDisabled
	if err := DB.Where("channel_id IN ?", channelIds).Find(&disabledRows).Error; err != nil {
		return err
	}
	disabledMap := make(map[channelModelStatusKey]ChannelModelDisabled, len(disabledRows))
	for _, row := range disabledRows {
		disabledMap[channelModelStatusKey{channelId: row.ChannelId, model: row.Model}] = row
	}

	var healthRows []ChannelModelHealth
	if err := DB.Where("channel_id IN ?", channelIds).Find(&healthRows).Error; err != nil {
		return err
	}
	healthMap := make(map[channelModelStatusKey]ChannelModelHealth, len(healthRows))
	for _, row := range healthRows {
		healthMap[channelModelStatusKey{channelId: row.ChannelId, model: row.Model}] = row
	}

	for _, ch := range channels {
		if ch == nil {
			continue
		}
		statuses := make([]ChannelModelStatus, 0, len(ch.GetModels()))
		for _, rawModel := range ch.GetModels() {
			modelName := strings.TrimSpace(rawModel)
			if modelName == "" {
				continue
			}
			key := channelModelStatusKey{channelId: ch.Id, model: modelName}
			if disabled, ok := disabledMap[key]; ok {
				statuses = append(statuses, ChannelModelStatus{
					Model:  modelName,
					Status: ChannelModelStatusDisabled,
					Source: disabled.Source,
					Reason: disabled.Reason,
				})
				continue
			}
			if health, ok := healthMap[key]; ok && isChannelModelHealthHealthy(health) {
				statuses = append(statuses, ChannelModelStatus{
					Model:  modelName,
					Status: ChannelModelStatusHealthy,
				})
				continue
			}
			statuses = append(statuses, ChannelModelStatus{
				Model:  modelName,
				Status: ChannelModelStatusUnknown,
			})
		}
		ch.ModelStatuses = statuses
	}

	return nil
}

// HealthAction 表示 ApplyTestResultInTx 实际触发的副作用（用于日志）
type HealthAction string

const (
	HealthActionNone      HealthAction = "none"      // 只更新了计数，未触发禁用/恢复
	HealthActionDisabled  HealthAction = "disabled"  // 达失败阈值，已写入 channel_model_disabled
	HealthActionRecovered HealthAction = "recovered" // 达成功阈值，已清理 auto/relay 禁用
)

func lockChannelModelHealthRow(tx *gorm.DB, channelId int, modelName string) *gorm.DB {
	query := tx.Where("channel_id = ? AND model = ?", channelId, modelName)
	// 生产仅使用 MySQL；MySQL/PG 使用 SELECT FOR UPDATE 行锁。
	// SQLite 本地/测试环境不支持该语义，退化为普通事务。
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		return query
	}
	return query.Clauses(clause.Locking{Strength: "UPDATE"})
}

// ApplyTestResultInTx 在单事务内原子更新健康计数 + 按阈值写/删 disabled 表
//
// 参数:
//
//	channelId, modelName — 渠道、模型
//	success — 本次测试结果（localErr 类错误调用方应跳过，不要进本函数）
//	errMsg — 失败错误信息（最多 1000 字符）
//	latencyMs — 响应耗时
//	failThreshold — 连续失败阈值
//	successThreshold — 连续成功阈值
func ApplyTestResultInTx(
	channelId int, modelName string,
	success bool, errMsg string, latencyMs int,
	failThreshold, successThreshold int,
) (HealthAction, error) {
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}
	now := time.Now().Unix()
	action := HealthActionNone

	txErr := DB.Transaction(func(tx *gorm.DB) error {
		// 1. 保证行存在（不存在则创建，存在则忽略）
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&ChannelModelHealth{
				ChannelId: channelId, Model: modelName,
			}).Error; err != nil {
			return err
		}

		// 2. 加行锁读取当前状态
		var h ChannelModelHealth
		if err := lockChannelModelHealthRow(tx, channelId, modelName).
			First(&h).Error; err != nil {
			return err
		}

		// 3. 更新计数
		if success {
			h.ConsecutiveSuccesses++
			h.ConsecutiveFailures = 0
			h.LastSuccessAt = now
			h.LastError = ""
		} else {
			h.ConsecutiveFailures++
			h.ConsecutiveSuccesses = 0
			h.LastError = errMsg
		}
		h.LastTestedAt = now
		h.LatencyMs = latencyMs

		// 4. 持久化计数
		if err := tx.Save(&h).Error; err != nil {
			return err
		}

		// 5. 根据阈值决定禁用/恢复动作
		if success && h.ConsecutiveSuccesses >= successThreshold {
			// 只清 auto/relay 禁用，保留 manual
			res := tx.Where(
				"channel_id = ? AND model = ? AND source IN ?",
				channelId, modelName,
				[]string{DisabledSourceAuto, DisabledSourceRelay},
			).Delete(&ChannelModelDisabled{})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				action = HealthActionRecovered
			}
		} else if !success && h.ConsecutiveFailures >= failThreshold {
			// 若当前无任何禁用记录才新建（避免覆盖 manual）
			var disCount int64
			if err := tx.Model(&ChannelModelDisabled{}).
				Where("channel_id = ? AND model = ?", channelId, modelName).
				Count(&disCount).Error; err != nil {
				return err
			}
			if disCount == 0 {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
					Create(&ChannelModelDisabled{
						ChannelId:  channelId,
						Model:      modelName,
						Source:     DisabledSourceAuto,
						Reason:     errMsg,
						DisabledAt: now,
					}).Error; err != nil {
					return err
				}
				action = HealthActionDisabled
			}
		}

		return nil
	})

	return action, txErr
}

// ApplyConfirmedProbeResultInTx 持久化同一轮内已经确认过的探测结果
//
// 与 ApplyTestResultInTx 的区别：
//   - 调用方已经在本轮内做完失败重试确认，失败会立即写 disabled(source=auto)
//   - 成功一次就清理 auto/relay 禁用，保留 manual
//   - ConsecutiveFailures 记录本轮连续失败次数，避免旧的跨轮次计数影响新判定
func ApplyConfirmedProbeResultInTx(
	channelId int, modelName string,
	success bool, errMsg string, latencyMs int,
	failedAttempts int,
) (HealthAction, error) {
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}
	if failedAttempts <= 0 {
		failedAttempts = 1
	}
	now := time.Now().Unix()
	action := HealthActionNone

	txErr := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&ChannelModelHealth{
				ChannelId: channelId, Model: modelName,
			}).Error; err != nil {
			return err
		}

		var h ChannelModelHealth
		if err := lockChannelModelHealthRow(tx, channelId, modelName).
			First(&h).Error; err != nil {
			return err
		}

		if success {
			h.ConsecutiveSuccesses++
			h.ConsecutiveFailures = 0
			h.LastSuccessAt = now
			h.LastError = ""
		} else {
			h.ConsecutiveFailures = failedAttempts
			h.ConsecutiveSuccesses = 0
			h.LastError = errMsg
		}
		h.LastTestedAt = now
		h.LatencyMs = latencyMs

		if err := tx.Save(&h).Error; err != nil {
			return err
		}

		if success {
			res := tx.Where(
				"channel_id = ? AND model = ? AND source IN ?",
				channelId, modelName,
				[]string{DisabledSourceAuto, DisabledSourceRelay},
			).Delete(&ChannelModelDisabled{})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				action = HealthActionRecovered
			}
			return nil
		}

		var disCount int64
		if err := tx.Model(&ChannelModelDisabled{}).
			Where("channel_id = ? AND model = ?", channelId, modelName).
			Count(&disCount).Error; err != nil {
			return err
		}
		if disCount == 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
				Create(&ChannelModelDisabled{
					ChannelId:  channelId,
					Model:      modelName,
					Source:     DisabledSourceAuto,
					Reason:     errMsg,
					DisabledAt: now,
				}).Error; err != nil {
				return err
			}
			action = HealthActionDisabled
		}

		return nil
	})

	return action, txErr
}

// UpdateHealthObservability 仅更新观测字段（供 localErr 类中立结果使用，不参与状态机）
func UpdateHealthObservability(channelId int, modelName, errMsg string, latencyMs int) error {
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}
	now := time.Now().Unix()
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&ChannelModelHealth{
				ChannelId: channelId, Model: modelName,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&ChannelModelHealth{}).
			Where("channel_id = ? AND model = ?", channelId, modelName).
			Updates(map[string]interface{}{
				"last_tested_at": now,
				"last_error":     errMsg,
				"latency_ms":     latencyMs,
			}).Error
	})
}

// GetChannelModelHealth 读单条（调试/观测用）
func GetChannelModelHealth(channelId int, modelName string) (*ChannelModelHealth, error) {
	var h ChannelModelHealth
	err := DB.Where("channel_id = ? AND model = ?", channelId, modelName).First(&h).Error
	if err != nil {
		return nil, err
	}
	return &h, err
}

// DeleteHealthByChannel 清理某渠道的所有健康记录
func DeleteHealthByChannel(channelId int) error {
	return DB.Where("channel_id = ?", channelId).
		Delete(&ChannelModelHealth{}).Error
}

// DeleteHealthByChannelNotInModels 清理某渠道下 model 不在 keepModels 列表的健康记录
func DeleteHealthByChannelNotInModels(channelId int, keepModels []string) error {
	q := DB.Where("channel_id = ?", channelId)
	if len(keepModels) > 0 {
		q = q.Where("model NOT IN ?", keepModels)
	}
	return q.Delete(&ChannelModelHealth{}).Error
}
