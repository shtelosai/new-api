package model

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTaskRetryReservationIncludesPendingQuota(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "batch", true: "batch and Redis"}[cached], func(t *testing.T) {
			resetImageJobAccountingMemory()
			t.Cleanup(resetImageJobAccountingMemory)
			oldRedis, oldBatch, oldClient := common.RedisEnabled, common.BatchUpdateEnabled, common.RDB
			common.RedisEnabled = false
			common.BatchUpdateEnabled = true
			t.Cleanup(func() { common.RedisEnabled, common.BatchUpdateEnabled, common.RDB = oldRedis, oldBatch, oldClient })
			id := 922001
			require.NoError(t, DB.Create(&User{Id: id, Username: "retry-queue", AffCode: "retry-queue", Quota: 300}).Error)
			require.NoError(t, DB.Create(&Token{Id: id, UserId: id, Key: "retry-queue", RemainQuota: 300}).Error)
			t.Cleanup(func() { DB.Unscoped().Delete(&Token{}, id); DB.Unscoped().Delete(&User{}, id) })
			require.NoError(t, DecreaseUserQuota(id, 100, false))
			require.NoError(t, DecreaseTokenQuota(id, "retry-queue", 100))
			if cached {
				server := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: server.Addr()})
				t.Cleanup(func() { _ = client.Close() })
				common.RedisEnabled = true
				common.RDB = client
				// 故意放入较旧的高余额，提交前门禁仍以 DB+待落库增量为准。
				require.NoError(t, client.HSet(context.Background(), getUserCacheKey(id), "Quota", 300).Err())
			}
			assert.ErrorIs(t, ReserveTaskRetryQuota(id, id, 0, "retry-queue", 250, false), ErrTaskRetryWalletQuota)
			require.NoError(t, ReserveTaskRetryQuota(id, id, 0, "retry-queue", 200, false))
			batchUpdate()
			var user User
			var token Token
			require.NoError(t, DB.First(&user, id).Error)
			require.NoError(t, DB.First(&token, id).Error)
			assert.Zero(t, user.Quota)
			assert.Zero(t, token.RemainQuota)
			assert.Equal(t, 300, token.UsedQuota)
			assert.ErrorIs(t, ReserveTaskRetryQuota(id, id, 0, "retry-queue", 1, false), ErrTaskRetryWalletQuota)
		})
	}
}

func TestTaskRetryReservationCannotSpendSameBalanceTwice(t *testing.T) {
	resetImageJobAccountingMemory()
	t.Cleanup(resetImageJobAccountingMemory)
	id := 922002
	require.NoError(t, DB.Create(&User{Id: id, Username: "retry-concurrent", AffCode: "retry-concurrent", Quota: 100}).Error)
	require.NoError(t, DB.Create(&Token{Id: id, UserId: id, Key: "retry-concurrent", RemainQuota: 100}).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&Token{}, id); DB.Unscoped().Delete(&User{}, id) })
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- ReserveTaskRetryQuota(id, id, 0, "retry-concurrent", 80, false)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var passed, rejected int
	for err := range results {
		if err == nil {
			passed++
		} else {
			assert.ErrorIs(t, err, ErrTaskRetryWalletQuota)
			rejected++
		}
	}
	assert.Equal(t, 1, passed)
	assert.Equal(t, 1, rejected)
	var user User
	require.NoError(t, DB.First(&user, id).Error)
	assert.Equal(t, 20, user.Quota)
}

func TestTaskRetryReservationAfterImageAccountingTransfer(t *testing.T) {
	token, _ := imageJobAccountingFixture(t)
	require.NoError(t, DecreaseUserQuota(token.UserId, 100, false))
	require.NoError(t, DecreaseTokenQuota(token.Id, token.Key, 100))
	require.NoError(t, PrepareImageJobAccounting(context.Background(), token.Id))
	require.NoError(t, ReserveTaskRetryQuota(token.UserId, token.Id, 0, token.Key, 200, false))
	batchUpdate()
	var user User
	var got Token
	require.NoError(t, DB.First(&user, token.UserId).Error)
	require.NoError(t, DB.First(&got, token.Id).Error)
	assert.Equal(t, 3000000-300, user.Quota)
	assert.Equal(t, 1000000-300, got.RemainQuota)
	assert.Equal(t, 300, got.UsedQuota)
}

func TestTaskRetrySubscriptionRefundAtomicity(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&SubscriptionPreConsumeRecord{}))
	sub := UserSubscription{Id: 922100, UserId: 922100, AmountTotal: 1000, AmountUsed: 300, Status: "active"}
	require.NoError(t, DB.Create(&sub).Error)
	record := SubscriptionPreConsumeRecord{RequestId: "retry-refund-atomic", UserId: sub.UserId, UserSubscriptionId: sub.Id, PreConsumed: 100, Status: "consumed"}
	require.NoError(t, DB.Create(&record).Error)
	t.Cleanup(func() { DB.Unscoped().Delete(&sub); DB.Unscoped().Delete(&record) })
	failure := errors.New("退款回执保存失败")
	const callback = "test:task-refund-atomic"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "SubscriptionPreConsumeRecord" {
			tx.AddError(failure)
		}
	}))
	t.Cleanup(func() { DB.Callback().Update().Remove(callback) })
	assert.ErrorIs(t, RefundSubscriptionPreConsume(record.RequestId), failure)
	require.NoError(t, DB.First(&sub, sub.Id).Error)
	assert.EqualValues(t, 300, sub.AmountUsed, "回执失败时余额更新也必须回滚")
	require.NoError(t, DB.Callback().Update().Remove(callback))
	require.NoError(t, RefundSubscriptionPreConsume(record.RequestId))
	require.NoError(t, RefundSubscriptionPreConsume(record.RequestId))
	require.NoError(t, DB.First(&sub, sub.Id).Error)
	assert.EqualValues(t, 200, sub.AmountUsed)
	require.NoError(t, DB.First(&record, record.Id).Error)
	assert.Equal(t, "refunded", record.Status)
}
