package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func resetImageJobAccountingMemory() {
	imageJobAccounting.Lock()
	defer imageJobAccounting.Unlock()
	imageJobAccounting.ready = false
	imageJobAccounting.issueDir = ""
	imageJobAccountingIssues.Range(func(key, value any) bool { imageJobAccountingIssues.Delete(key); return true })
	imageJobAccounting.tokens = map[int]bool{}
	imageJobAccounting.keys = map[string]int{}
	imageJobAccounting.users = map[int]bool{}
	imageJobAccounting.pending = map[int]*ImageJobAccountingTransfer{}
	imageJobAccounting.frozen = map[int]int{}
	imageJobAccounting.retries = map[int]imageJobAccountingRetry{}
	for i := range batchUpdateStores {
		batchUpdateStores[i] = map[int]int{}
	}
}

func imageJobAccountingFixture(t *testing.T) (*Token, *Token) {
	t.Helper()
	resetImageJobAccountingMemory()
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	require.NoError(t, DB.AutoMigrate(&ImageJobAccountingGuard{}, &ImageJobAccountingTransfer{}))
	require.NoError(t, DB.Exec("DELETE FROM image_job_accounting_guards").Error)
	require.NoError(t, DB.Exec("DELETE FROM image_job_accounting_transfers").Error)
	a, b := imageJobFixture(t)
	t.Setenv("IMAGE_JOBS_SINGLE_WRITER", "true")
	t.Setenv("TWORK_IMAGE_TASKS_DATA_DIR", t.TempDir())
	require.NoError(t, InitImageJobAccountingBridge(context.Background()))
	common.BatchUpdateEnabled = true
	t.Cleanup(func() {
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
		resetImageJobAccountingMemory()
		require.NoError(t, DB.Exec("DELETE FROM image_job_accounting_guards").Error)
		require.NoError(t, DB.Exec("DELETE FROM image_job_accounting_transfers").Error)
	})
	return a, b
}

func TestImageJobAccountingPreservesOldPreconsumeAndFinalDeltaAcrossReserve(t *testing.T) {
	a, b := imageJobAccountingFixture(t)
	ctx := context.Background()
	// 原请求已经预扣，金额还在 batch 队列；新图片必须先看到这一笔。
	require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100000))
	require.NoError(t, DecreaseUserQuota(a.UserId, 100000, false))
	require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
	job, created, err := CreateImageJob(ctx, a.Id, ImageJobRequest{ClientRequestID: "bridge-overlap", ClientSessionID: "s", Prompt: "杯子"})
	require.NoError(t, err)
	require.True(t, created)
	// 旧请求实际花费 80000，退还 20000 差额；价格和统计语义保持原样。
	require.NoError(t, IncreaseTokenQuota(a.Id, a.Key, 20000))
	require.NoError(t, IncreaseUserQuota(a.UserId, 20000, false))
	UpdateUserUsedQuotaAndRequestCount(a.UserId, 80000)
	require.NoError(t, DB.Model(job).Updates(map[string]any{"status": "settling", "channel_id": 9126, "result_hash": "hash", "actual_size": "1024x1024", "output_format": "png", "bytes": 123}).Error)
	require.NoError(t, SettleImageJob(ctx, job.ID, true, ""))
	require.NoError(t, SettleImageJob(ctx, job.ID, true, ""))
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 770000, a.RemainQuota)
	assert.Equal(t, 230000, a.UsedQuota)
	var user User
	require.NoError(t, DB.First(&user, a.UserId).Error)
	assert.Equal(t, 2770000, user.Quota)
	assert.Equal(t, 230000, user.UsedQuota)
	assert.Equal(t, 2, user.RequestCount)
	// 同付款用户的其他 token 仍走原 token batch，付款钱包已经转为同步 DB。
	require.NoError(t, DecreaseTokenQuota(b.Id, b.Key, 1000))
	require.NoError(t, DecreaseUserQuota(b.UserId, 1000, false))
	require.NoError(t, DB.First(b, b.Id).Error)
	assert.Equal(t, 1000000, b.RemainQuota)
	quota, err := GetUserQuota(b.UserId, false)
	require.NoError(t, err)
	assert.Equal(t, 2769000, quota)
	batchUpdate()
	require.NoError(t, DB.First(b, b.Id).Error)
	assert.Equal(t, 999000, b.RemainQuota)
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 770000, a.RemainQuota)
}

func TestImageJobAccountingRollbackFreezesPendingAndRetryAppliesExactlyOnce(t *testing.T) {
	a, _ := imageJobAccountingFixture(t)
	ctx := context.Background()
	require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100))
	require.NoError(t, DecreaseUserQuota(a.UserId, 100, false))
	UpdateUserUsedQuotaAndRequestCount(a.UserId, 100)
	callback := "test:image-transfer-rollback"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "image_job_accounting_transfers" {
			tx.AddError(errors.New("移交存储故障"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Create().Remove(callback) })
	require.Error(t, PrepareImageJobAccounting(ctx, a.Id))
	mutations := make(chan error, 1)
	go func() {
		if err := DecreaseTokenQuota(a.Id, a.Key, 1); err != nil {
			mutations <- err
			return
		}
		mutations <- IncreaseUserQuota(a.UserId, 1, false)
	}()
	var finished bool
	select {
	case err := <-mutations:
		finished = true
		assert.Fail(t, "冻结期间旧金额写入不得提前结束", "err=%v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_, err := GetTokenByKey(a.Key, false)
	assert.ErrorIs(t, err, errImageJobAccountingFrozen)
	_, err = GetUserQuota(a.UserId, false)
	assert.ErrorIs(t, err, errImageJobAccountingFrozen)
	// void 统计入口也等待同一移交，避免冻结期间提前完成并依赖下一轮 batch。
	statistics := make(chan struct{})
	go func() {
		UpdateUserUsedQuotaAndRequestCount(a.UserId, 20)
		close(statistics)
	}()
	select {
	case <-statistics:
		assert.Fail(t, "冻结期间统计写入不得提前结束")
	case <-time.After(30 * time.Millisecond):
	}
	batchUpdate()
	var frozenToken Token
	require.NoError(t, DB.First(&frozenToken, a.Id).Error)
	assert.Equal(t, 1000000, frozenToken.RemainQuota)
	require.NoError(t, DB.Callback().Create().Remove(callback))
	require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
	if !finished {
		select {
		case err := <-mutations:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("移交确认后旧金额未恢复")
		}
	}
	select {
	case <-statistics:
	case <-time.After(time.Second):
		t.Fatal("移交确认后统计未恢复")
	}
	require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
	batchUpdate()
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 999899, a.RemainQuota)
	assert.Equal(t, 101, a.UsedQuota)
	var user User
	require.NoError(t, DB.First(&user, a.UserId).Error)
	assert.Equal(t, 2999901, user.Quota)
	assert.Equal(t, 120, user.UsedQuota)
	assert.Equal(t, 2, user.RequestCount)
	var count int64
	require.NoError(t, DB.Model(&ImageJobAccountingTransfer{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

type imageJobAmbiguousCommitPool struct {
	*sql.DB
	fail *atomic.Bool
}

func (p imageJobAmbiguousCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &imageJobAmbiguousCommitTx{Tx: tx, fail: p.fail}, nil
}

type imageJobAmbiguousCommitTx struct {
	*sql.Tx
	fail *atomic.Bool
}

func (t imageJobAmbiguousCommitTx) Commit() error {
	err := t.Tx.Commit()
	if err == nil && t.fail.CompareAndSwap(true, false) {
		return errors.New("COMMIT 回执连接中断")
	}
	return err
}

func TestImageJobAccountingUnknownCommitUsesReceiptInsteadOfRepeatingDeltas(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			a, _ := imageJobAccountingFixture(t)
			ctx := context.Background()
			require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100))
			require.NoError(t, DecreaseUserQuota(a.UserId, 100, false))
			originalDB := DB
			pool, err := DB.DB()
			require.NoError(t, err)
			unknown := &atomic.Bool{}
			unknown.Store(true)
			DB = DB.Session(&gorm.Session{NewDB: true})
			DB.Statement = &gorm.Statement{DB: DB, ConnPool: imageJobAmbiguousCommitPool{DB: pool, fail: unknown}, Context: ctx}
			DB.ConnPool = DB.Statement.ConnPool
			t.Cleanup(func() { DB = originalDB })
			require.ErrorContains(t, PrepareImageJobAccounting(ctx, a.Id), "COMMIT")
			require.NoError(t, DB.First(a, a.Id).Error)
			assert.Equal(t, 999900, a.RemainQuota)
			_, err = GetTokenByKey(a.Key, false)
			assert.ErrorIs(t, err, errImageJobAccountingFrozen)
			refund := make(chan error, 1)
			finished := false
			if !restart {
				go func() {
					if err := IncreaseTokenQuota(a.Id, a.Key, 20); err != nil {
						refund <- err
						return
					}
					refund <- IncreaseUserQuota(a.UserId, 20, false)
				}()
				select {
				case err := <-refund:
					finished = true
					assert.Fail(t, "COMMIT 未确认前旧退款不得提前结束", "err=%v", err)
				case <-time.After(30 * time.Millisecond):
				}
			}
			batchUpdate()
			if restart {
				resetImageJobAccountingMemory()
				require.NoError(t, InitImageJobAccountingBridge(ctx))
			}
			require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
			if !restart && !finished {
				select {
				case err := <-refund:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("确认原 COMMIT 回执后旧退款未恢复")
				}
			}
			require.NoError(t, DB.First(a, a.Id).Error)
			if restart {
				assert.Equal(t, 999900, a.RemainQuota)
			} else {
				assert.Equal(t, 999920, a.RemainQuota)
				assert.Equal(t, 80, a.UsedQuota)
				var user User
				require.NoError(t, DB.First(&user, a.UserId).Error)
				assert.Equal(t, 2999920, user.Quota)
			}
			assert.Empty(t, batchUpdateStores[BatchUpdateTypeTokenQuota])
			var count int64
			require.NoError(t, DB.Model(&ImageJobAccountingTransfer{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
		})
	}
}

func TestImageJobAccountingRestartKeepsGuardsWhenNewJobsDisabled(t *testing.T) {
	a, _ := imageJobAccountingFixture(t)
	ctx := context.Background()
	require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
	job, _, err := CreateImageJob(ctx, a.Id, ImageJobRequest{ClientRequestID: "restart", ClientSessionID: "s", Prompt: "杯子"})
	require.NoError(t, err)
	resetImageJobAccountingMemory()
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "false")
	require.NoError(t, InitImageJobAccountingBridge(ctx))
	require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100))
	require.NoError(t, DecreaseUserQuota(a.UserId, 100, false))
	require.NoError(t, SettleImageJob(ctx, job.ID, false, "failed"))
	require.NoError(t, SettleImageJob(ctx, job.ID, false, "failed"))
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 999900, a.RemainQuota)
	assert.Empty(t, batchUpdateStores[BatchUpdateTypeTokenQuota])
	quota, err := GetUserQuota(a.UserId, false)
	require.NoError(t, err)
	assert.Equal(t, 2999900, quota)
	resetImageJobAccountingMemory()
	t.Setenv("IMAGE_JOBS_SINGLE_WRITER", "false")
	require.ErrorContains(t, InitImageJobAccountingBridge(ctx), "SINGLE_WRITER")
}

func TestImageJobAccountingWaitsForDetachedBatchBeforeActivating(t *testing.T) {
	a, _ := imageJobAccountingFixture(t)
	require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100))
	require.NoError(t, DecreaseUserQuota(a.UserId, 100, false))
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	callback := "test:block-token-batch"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" {
			first.Do(func() { close(entered); <-release })
		}
	}))
	t.Cleanup(func() { DB.Callback().Update().Remove(callback) })
	flushed := make(chan struct{})
	go func() { batchUpdate(); close(flushed) }()
	<-entered
	prepared := make(chan error, 1)
	go func() { prepared <- PrepareImageJobAccounting(context.Background(), a.Id) }()
	close(release)
	<-flushed
	require.NoError(t, <-prepared)
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 999900, a.RemainQuota)
	var user User
	require.NoError(t, DB.First(&user, a.UserId).Error)
	assert.Equal(t, 2999900, user.Quota)
	var receipt ImageJobAccountingTransfer
	require.NoError(t, DB.First(&receipt).Error)
	assert.Zero(t, receipt.TokenDelta)
	assert.Zero(t, receipt.UserDelta)
}

func TestImageJobAccountingFailedLegacyBatchBlocksNewJobsAcrossRestart(t *testing.T) {
	for _, markerStore := range []string{"database", "filesystem"} {
		t.Run(markerStore, func(t *testing.T) {
			a, _ := imageJobAccountingFixture(t)
			require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100))
			require.NoError(t, DecreaseUserQuota(a.UserId, 100, false))
			callback := "test:legacy-batch-error"
			require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "tokens" || tx.Statement.Table == "users" {
					tx.AddError(errors.New("旧批量计费写入故障"))
				}
			}))
			t.Cleanup(func() { DB.Callback().Update().Remove(callback) })
			if markerStore == "filesystem" {
				require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "image_job_accounting_guards" {
						tx.AddError(errors.New("数据库故障标记暂不可写"))
					}
				}))
				t.Cleanup(func() { DB.Callback().Create().Remove(callback) })
			}
			batchUpdate()
			require.NoError(t, DB.Callback().Update().Remove(callback))
			if markerStore == "filesystem" {
				require.NoError(t, DB.Callback().Create().Remove(callback))
			} else {
				// 删除磁盘副本，证明 DB 标记单独足以恢复拒绝新任务。
				entries, err := os.ReadDir(imageJobAccounting.issueDir)
				require.NoError(t, err)
				for _, entry := range entries {
					require.NoError(t, os.Remove(filepath.Join(imageJobAccounting.issueDir, entry.Name())))
				}
			}
			assert.ErrorIs(t, PrepareImageJobAccounting(context.Background(), a.Id), errImageJobAccountingUncertain)
			resetImageJobAccountingMemory()
			require.NoError(t, InitImageJobAccountingBridge(context.Background()))
			assert.ErrorIs(t, PrepareImageJobAccounting(context.Background(), a.Id), errImageJobAccountingUncertain)
			// 不声称恢复失败旧批量金额，也不修改旧入口后续路由或金额计算。
			require.NoError(t, DB.First(a, a.Id).Error)
			assert.Equal(t, 1000000, a.RemainQuota)
			require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 1))
			batchUpdate()
			require.NoError(t, DB.First(a, a.Id).Error)
			assert.Equal(t, 999999, a.RemainQuota)
		})
	}
}

func TestImageJobAccountingReconcileUnfreezesOldRequestsWithoutNewJobRetry(t *testing.T) {
	a, b := imageJobAccountingFixture(t)
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "false")
	require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100))
	require.NoError(t, DecreaseUserQuota(a.UserId, 100, false))
	callback := "test:pending-reconcile"
	var attempts atomic.Int32
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "image_job_accounting_transfers" {
			attempts.Add(1)
			tx.AddError(errors.New("额度移交临时数据库故障"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Create().Remove(callback) })
	require.Error(t, PrepareImageJobAccounting(context.Background(), a.Id))
	_, err := GetUserCache(b.UserId)
	assert.ErrorIs(t, err, errImageJobAccountingFrozen)
	require.NoError(t, ReconcileImageJobAccounting(context.Background()))
	assert.EqualValues(t, 1, attempts.Load(), "退避期间不重复打故障数据库")
	require.NoError(t, DB.Callback().Create().Remove(callback))
	// 模拟下一个到期的 worker tick，不用 sleep 依赖机器速度。
	imageJobAccounting.Lock()
	retry := imageJobAccounting.retries[a.Id]
	retry.nextAt = time.Time{}
	imageJobAccounting.retries[a.Id] = retry
	imageJobAccounting.Unlock()
	require.NoError(t, ReconcileImageJobAccounting(context.Background()))
	user, err := GetUserCache(b.UserId)
	require.NoError(t, err)
	assert.Equal(t, 2999900, user.Quota)
	require.NoError(t, IncreaseTokenQuota(a.Id, a.Key, 20))
	require.NoError(t, IncreaseUserQuota(a.UserId, 20, false))
	token, err := GetTokenByKey(a.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 999920, token.RemainQuota)
	var count int64
	require.NoError(t, DB.Model(&ImageJob{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestImageJobAccountingPreparePrecedesExternalQuotaSynchronization(t *testing.T) {
	a, _ := imageJobAccountingFixture(t)
	ctx := context.Background()
	require.NoError(t, DecreaseTokenQuota(a.Id, a.Key, 100000))
	require.NoError(t, DecreaseUserQuota(a.UserId, 100000, false))
	// tbackend 必须先调用 prepare，再按已落库日志重算相同 token 额度。
	require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
	require.NoError(t, DB.Model(a).Updates(map[string]any{"remain_quota": 900000, "used_quota": 100000}).Error)
	_, created, err := CreateImageJob(ctx, a.Id, ImageJobRequest{ClientRequestID: "after-external-sync", ClientSessionID: "s", Prompt: "杯子"})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 750000, a.RemainQuota)
	assert.Equal(t, 100000, a.UsedQuota)
	batchUpdate()
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 750000, a.RemainQuota)
}

func TestImageJobAccountingRedisLateFillCannotRestoreReservedQuota(t *testing.T) {
	if os.Getenv("NEWAPI_TEST_LOCAL_REDIS") != "1" {
		t.Skip("设置 NEWAPI_TEST_LOCAL_REDIS=1 启动隔离 Redis，验证图片额度桥")
	}
	a, b := imageJobAccountingFixture(t)
	binary, err := exec.LookPath("redis-server")
	require.NoError(t, err)
	dir, err := os.MkdirTemp("/tmp", "newapi-image-redis-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socket := filepath.Join(dir, "redis.sock")
	cmd := exec.Command(binary, "--port", "0", "--unixsocket", socket, "--save", "", "--appendonly", "no")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Eventually(t, func() bool { return client.Ping(context.Background()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	previousClient := common.RDB
	common.RDB = client
	common.RedisEnabled = true
	t.Cleanup(func() { common.RedisEnabled = false; common.RDB = previousClient })
	ctx := context.Background()
	var oldUser User
	require.NoError(t, DB.First(&oldUser, a.UserId).Error)
	oldToken := *a
	require.NoError(t, cacheSetToken(oldToken))
	require.NoError(t, populateUserCache(oldUser))
	require.NoError(t, PrepareImageJobAccounting(ctx, a.Id))
	job, _, err := CreateImageJob(ctx, a.Id, ImageJobRequest{ClientRequestID: "redis", ClientSessionID: "s", Prompt: "杯子"})
	require.NoError(t, err)
	// 模拟激活前排入 gopool 的 DB 快照现在才回填，必须不能重置可消费额度。
	require.NoError(t, cacheSetToken(oldToken))
	require.NoError(t, populateUserCache(oldUser))
	require.NoError(t, updateUserQuotaCache(a.UserId, oldUser.Quota))
	readers := make(chan error, 2)
	go func() {
		token, e := GetTokenByKey(a.Key, false)
		if e == nil && token.RemainQuota != 850000 {
			e = errors.New("缓存回填恢复了已预扣令牌额度")
		}
		readers <- e
	}()
	go func() {
		user, e := GetUserCache(a.UserId)
		if e == nil && user.Quota != 2850000 {
			e = errors.New("缓存回填恢复了已预扣用户额度")
		}
		readers <- e
	}()
	require.NoError(t, <-readers)
	require.NoError(t, <-readers)
	// 一个晚到的 Redis HINCR 可以制造只有 quota、没有 Id 的 hash，仍按 key guard 读取 DB。
	require.NoError(t, cacheDeleteToken(a.Key))
	require.NoError(t, cacheIncrTokenQuota(a.Key, 9999999))
	token, err := GetTokenByKey(a.Key, false)
	require.NoError(t, err)
	assert.Equal(t, a.Id, token.Id)
	assert.Equal(t, 850000, token.RemainQuota)
	require.NoError(t, cacheDeleteToken(a.Key))
	require.NoError(t, invalidateUserCache(a.UserId))
	token, err = GetTokenByKey(a.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 850000, token.RemainQuota)
	user, err := GetUserCache(a.UserId)
	require.NoError(t, err)
	assert.Equal(t, 2850000, user.Quota)
	// 非参与 token 保留原 Redis 路径，不扩大为全系统缓存禁用。
	oldOther := *b
	oldOther.RemainQuota = 12345
	require.NoError(t, cacheSetToken(oldOther))
	other, err := GetTokenByKey(b.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 12345, other.RemainQuota)
	require.NoError(t, SettleImageJob(ctx, job.ID, false, "failed"))
	require.NoError(t, SettleImageJob(ctx, job.ID, false, "failed"))
	token, err = GetTokenByKey(a.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 1000000, token.RemainQuota)
}

func TestImageJobAccountingMySQLTransactions(t *testing.T) {
	dsn := os.Getenv("NEWAPI_TEST_IMAGE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("设置专用 NEWAPI_TEST_IMAGE_MYSQL_DSN 验证 MySQL 移交与 COMMIT 恢复；禁止使用生产库")
	}
	previousDB, previousLogDB := DB, LOG_DB
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		initCol()
		require.NoError(t, pool.Close())
	})
	DB, LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	initCol()
	require.NoError(t, DB.AutoMigrate(&User{}, &Token{}, &Channel{}, &Ability{}, &TokenModelChannel{}, &ChannelModelDisabled{}, &Log{}))
	t.Run("pending_old_and_new", TestImageJobAccountingPreservesOldPreconsumeAndFinalDeltaAcrossReserve)
	t.Run("rollback_and_retry", TestImageJobAccountingRollbackFreezesPendingAndRetryAppliesExactlyOnce)
	t.Run("commit_unknown_and_restart", TestImageJobAccountingUnknownCommitUsesReceiptInsteadOfRepeatingDeltas)
	t.Run("inflight_batch", TestImageJobAccountingWaitsForDetachedBatchBeforeActivating)
	t.Run("disabled_creation_keeps_guard", TestImageJobAccountingRestartKeepsGuardsWhenNewJobsDisabled)
	t.Run("legacy_batch_failure_blocks", TestImageJobAccountingFailedLegacyBatchBlocksNewJobsAcrossRestart)
	t.Run("worker_reconciles_without_retry", TestImageJobAccountingReconcileUnfreezesOldRequestsWithoutNewJobRetry)
	t.Run("external_quota_sync_order", TestImageJobAccountingPreparePrecedesExternalQuotaSynchronization)
}
