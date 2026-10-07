package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ImageJobProjection 在金额事务中持久化，日志数据库不可用不丢失结算事实。
type ImageJobProjection struct {
	ID          string `gorm:"primaryKey;size:80"`
	JobID       string `gorm:"size:64;index"`
	CreatedAt   int64
	CompletedAt int64 `gorm:"index"`
}

// ImageJobLogReceipt 与日志必须处于同一可事务日志库，普通 logs.request_id 不具备唯一约束。
type ImageJobLogReceipt struct {
	ID        string `gorm:"primaryKey;size:80"`
	CreatedAt int64
}

func SettleImageJob(ctx context.Context, id string, success bool, errorCode string, leaseOwner ...string) error {
	var identity ImageJob
	if err := DB.WithContext(ctx).Select("token_id").Where("id = ?", id).First(&identity).Error; err != nil {
		return err
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx.Unscoped()).First(&token, identity.TokenID).Error; err != nil {
			return err
		}
		var job ImageJob
		if err := lockForUpdate(tx).First(&job, "id = ?", id).Error; err != nil {
			return err
		}
		if job.Terminal() {
			return nil
		}
		if len(leaseOwner) > 0 && (job.LeaseOwner != leaseOwner[0] || job.LeaseUntil <= time.Now().Unix()) {
			return ErrImageJobLease
		}
		_, price := (ImageJobRequest{Resolution: job.Resolution}).Price()
		if job.ReservedQuota != price || price <= 0 || job.ChargedQuota != 0 {
			return errors.New("图片任务金额状态不一致")
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).First(&user, job.UserID).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		fields := map[string]any{"reserved_quota": 0, "settled_at": now, "updated_at": now, "lease_owner": "", "lease_until": 0}
		if success {
			if job.Status != "settling" || job.ResultHash == "" || job.Bytes <= 0 || job.OutputFormat == "" || job.ActualSize == "" || job.ChannelID <= 0 {
				return errors.New("图片结果未经完整校验，不能结算")
			}
			fields["expires_at"] = now + 7*86400
			fields["status"] = "succeeded"
			fields["charged_quota"] = price
			fields["error_code"] = ""
			if err := tx.Unscoped().Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{"used_quota": gorm.Expr("used_quota + ?", price), "accessed_time": now}).Error; err != nil {
				return err
			}
			if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{"used_quota": gorm.Expr("used_quota + ?", price), "request_count": gorm.Expr("request_count + 1")}).Error; err != nil {
				return err
			}
			if err := tx.Model(&Channel{}).Where("id = ?", job.ChannelID).Update("used_quota", gorm.Expr("used_quota + ?", price)).Error; err != nil {
				return err
			}
			if err := tx.Create(&ImageJobProjection{ID: "imagejob_" + job.ID, JobID: job.ID, CreatedAt: now}).Error; err != nil {
				return err
			}
		} else {
			fields["status"] = "failed"
			fields["error_code"] = errorCode
			// 当前额度角色以 token 行为准；额度同步在同一锁后按 reservation 重新计算。
			if !token.UnlimitedQuota {
				if err := tx.Unscoped().Model(&Token{}).Where("id = ?", token.Id).Update("remain_quota", gorm.Expr("remain_quota + ?", price)).Error; err != nil {
					return err
				}
			}
			if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Update("quota", gorm.Expr("quota + ?", price)).Error; err != nil {
				return err
			}
		}
		return tx.Model(&ImageJob{}).Where("id = ?", job.ID).Updates(fields).Error
	})
}

func ProjectImageJobLogs(ctx context.Context) error {
	var pending []ImageJobProjection
	if err := DB.WithContext(ctx).Where("completed_at = ?", 0).Order("created_at ASC").Limit(100).Find(&pending).Error; err != nil {
		return err
	}
	for _, entry := range pending {
		var job ImageJob
		if err := DB.WithContext(ctx).First(&job, "id = ?", entry.JobID).Error; err != nil {
			return err
		}
		if job.Status != "succeeded" || job.ChargedQuota <= 0 {
			return errors.New("图片消费投影状态无效")
		}
		var token Token
		var user User
		if err := DB.WithContext(ctx).Unscoped().First(&token, job.TokenID).Error; err != nil {
			return err
		}
		if err := DB.WithContext(ctx).Unscoped().First(&user, job.UserID).Error; err != nil {
			return err
		}
		group := token.Group
		if group == "" {
			group = user.Group
		}
		err := LOG_DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ImageJobLogReceipt{ID: entry.ID, CreatedAt: time.Now().Unix()})
			if inserted.Error != nil {
				return inserted.Error
			}
			if inserted.RowsAffected == 0 {
				return nil
			}
			return tx.Create(&Log{UserId: job.UserID, TokenId: job.TokenID, CreatedAt: job.SettledAt, Type: LogTypeConsume, Content: "异步图片生成", Username: user.Username, TokenName: token.Name, ModelName: "twork-image-" + job.ModelFamily + "-async", Quota: job.ChargedQuota, ChannelId: job.ChannelID, Group: group, RequestId: entry.ID, UseTime: int(job.SettledAt - job.CreatedAt)}).Error
		})
		if err != nil {
			return err
		}
		if err := DB.WithContext(ctx).Model(&ImageJobProjection{}).Where("id = ? AND completed_at = ?", entry.ID, 0).Update("completed_at", time.Now().Unix()).Error; err != nil {
			return err
		}
	}
	return nil
}

// 旧 Redis/batch 只有完成永久 DB 主体屏障后才可共存；ClickHouse 仍不具备幂等日志事务。
func ImageJobAccountingSupported() bool {
	return !common.UsingLogDatabase(common.DatabaseTypeClickHouse) && ((!common.RedisEnabled && !common.BatchUpdateEnabled) || ImageJobAccountingBridgeReady())
}
