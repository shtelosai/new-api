package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrTaskRetryWalletQuota = errors.New("task retry wallet quota insufficient")
var ErrTaskRetryTokenQuota = errors.New("task retry token quota insufficient")

// ReserveTaskRetryQuota 为已知价格的提交前补扣提供原子余额门禁。
// userID=0 时按 subscriptionID 预留订阅与令牌；playground 的 tokenID=0 不扣令牌。
// 公司旧队列与图片额度桥共用此屏障：先把该主体的旧增量纳入事务，不能读取滞后的 DB 余额。
func ReserveTaskRetryQuota(userID, tokenID, subscriptionID int, tokenKey string, amount int, unlimited bool) error {
	if amount < 0 {
		return errors.New("task retry quota cannot be negative")
	}
	if amount == 0 {
		return nil
	}
	reserved := false
	defer func() {
		if !reserved {
			return
		}
		// 缓存只失效；此 defer 最后运行，避免持有计费锁等待 Redis。
		if common.RedisEnabled {
			if userID > 0 {
				if err := invalidateUserCache(userID); err != nil {
					common.SysLog("failed to invalidate reserved task wallet cache: " + err.Error())
				}
			}
			if tokenID > 0 {
				if err := cacheDeleteToken(tokenKey); err != nil {
					common.SysLog("failed to invalidate reserved task token cache: " + err.Error())
				}
			}
		}
	}()
	for {
		imageJobAccounting.Lock()
		var err error
		if userID > 0 {
			_, err = imageJobUserAccountingMode(userID)
		}
		if err == nil && tokenID > 0 {
			_, err = imageJobTokenAccountingMode(tokenID)
		}
		if err == nil {
			break
		}
		changed := imageJobAccounting.changed
		imageJobAccounting.Unlock()
		<-changed
	}
	defer imageJobAccounting.Unlock()
	// 写锁也等待正在落库的批处理退出；锁顺序与图片移交一致。
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	defer batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
	batchUpdateLocks[BatchUpdateTypeTokenQuota].Lock()
	defer batchUpdateLocks[BatchUpdateTypeTokenQuota].Unlock()
	userDelta, tokenDelta := 0, 0
	if userID > 0 {
		userDelta = batchUpdateStores[BatchUpdateTypeUserQuota][userID]
	}
	if tokenID > 0 {
		tokenDelta = batchUpdateStores[BatchUpdateTypeTokenQuota][tokenID]
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if subscriptionID > 0 {
			if err := postConsumeUserSubscriptionDeltaTx(tx, subscriptionID, int64(amount)); err != nil {
				return err
			}
		}
		if userID > 0 {
			if userDelta != 0 {
				result := tx.Model(&User{}).Where("id = ?", userID).Update("quota", gorm.Expr("quota + ?", userDelta))
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return gorm.ErrRecordNotFound
				}
			}
			result := tx.Model(&User{}).Where("id = ? AND quota >= ?", userID, amount).Update("quota", gorm.Expr("quota - ?", amount))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrTaskRetryWalletQuota
			}
		}
		if tokenID > 0 {
			if tokenDelta != 0 {
				result := tx.Model(&Token{}).Where("id = ?", tokenID).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", tokenDelta), "used_quota": gorm.Expr("used_quota - ?", tokenDelta)})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return gorm.ErrRecordNotFound
				}
			}
			query := tx.Model(&Token{}).Where("id = ?", tokenID)
			if !unlimited {
				query = query.Where("remain_quota >= ?", amount)
			}
			result := query.Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", amount), "used_quota": gorm.Expr("used_quota + ?", amount), "accessed_time": common.GetTimestamp()})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrTaskRetryTokenQuota
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if userID > 0 {
		delete(batchUpdateStores[BatchUpdateTypeUserQuota], userID)
	}
	if tokenID > 0 {
		delete(batchUpdateStores[BatchUpdateTypeTokenQuota], tokenID)
	}
	reserved = true
	return nil
}
