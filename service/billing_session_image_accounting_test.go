package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 保留真实钱包行为，仅观察异步 Refund 是否在冻结恢复前结束。
type imageAccountingObservedFunding struct {
	FundingSource
	refunded chan error
}

func (f *imageAccountingObservedFunding) Refund() error {
	err := f.FundingSource.Refund()
	f.refunded <- err
	return err
}

func TestBillingSessionWaitsForImageAccountingRecovery(t *testing.T) {
	// 桥的 guard 设计为进程终身生效；独立测试进程避免污染其他 service 测试。
	const marker = "NEWAPI_TEST_BILLING_IMAGE_ACCOUNTING"
	if os.Getenv(marker) != "1" {
		binary, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.Command(binary, "-test.run=^TestBillingSessionWaitsForImageAccountingRecovery$", "-test.v")
		cmd.Env = append(os.Environ(), marker+"=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}

	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	t.Setenv("IMAGE_JOBS_SINGLE_WRITER", "true")
	t.Setenv("TWORK_IMAGE_TASKS_DATA_DIR", t.TempDir())
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "false")
	require.NoError(t, model.DB.AutoMigrate(&model.ImageJobAccountingGuard{}, &model.ImageJobAccountingTransfer{}))
	require.NoError(t, model.InitImageJobAccountingBridge(context.Background()))
	common.BatchUpdateEnabled = true

	for index, tc := range []struct {
		name   string
		actual int
		shared bool
	}{
		{"refund", 0, false},
		{"settle_more", 130, false},
		{"settle_less", 70, false},
		{"shared_owner_refund", 0, true},
		{"shared_owner_settle_more", 130, true},
		{"shared_owner_settle_less", 70, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			user := model.User{Id: 950000 + index, Username: tc.name, AffCode: fmt.Sprintf("fr%04d", index), Status: common.UserStatusEnabled, Quota: 3000}
			token := model.Token{Id: 960000 + index, UserId: user.Id, Key: tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
			require.NoError(t, model.DB.Create(&user).Error)
			require.NoError(t, model.DB.Create(&token).Error)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, ForcePreConsume: true}
			info.UserSetting.BillingPreference = "wallet_only"
			session, apiErr := NewBillingSession(c, info, 100)
			require.Nil(t, apiErr)
			require.Equal(t, 100, session.GetPreConsumedQuota())
			transferToken := token
			if tc.shared {
				transferToken = model.Token{Id: 970000 + index, UserId: user.Id, Key: "image-" + tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
				require.NoError(t, model.DB.Create(&transferToken).Error)
			}

			callback := "test:billing-image-freeze"
			require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "image_job_accounting_transfers" {
					tx.AddError(errors.New("额度移交临时故障"))
				}
			}))
			require.Error(t, model.PrepareImageJobAccounting(ctx, transferToken.Id))
			require.NoError(t, model.DB.Callback().Create().Remove(callback))
			_, err := model.GetUserQuota(user.Id, false)
			require.Error(t, err, "冻结用户仍须拒绝新请求鉴权")
			done := make(chan error, 1)
			if tc.actual == 0 {
				session.funding = &imageAccountingObservedFunding{FundingSource: session.funding, refunded: done}
				session.Refund(c)
				session.Refund(c)
			} else {
				go func() { done <- session.Settle(tc.actual) }()
			}
			finished := false
			select {
			case err := <-done:
				finished = true
				assert.Fail(t, "冻结期间旧计费不能报错后提前结束", "err=%v", err)
			case <-time.After(30 * time.Millisecond):
			}
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, 3000, user.Quota, "移交未确认前不得修改钱包")

			// 模拟后台定期恢复，不重发 Prepare 或新建图片请求。
			require.Eventually(t, func() bool {
				if err := model.ReconcileImageJobAccounting(ctx); err != nil {
					return false
				}
				_, err := model.GetUserQuota(user.Id, false)
				return err == nil
			}, 3*time.Second, 10*time.Millisecond)
			if !finished {
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("数据库恢复后旧计费未自动完成")
				}
			}
			if tc.actual == 0 {
				// 退款令牌更新发生在资金退款之后；同 owner 另一 token 仍按旧 batch 路径。
				require.Eventually(t, func() bool {
					if err := model.PrepareImageJobAccounting(ctx, token.Id); err != nil {
						return false
					}
					var current model.Token
					return model.DB.First(&current, token.Id).Error == nil && current.RemainQuota == 1000
				}, time.Second, 10*time.Millisecond)
			} else {
				require.NoError(t, model.PrepareImageJobAccounting(ctx, token.Id))
			}
			require.NoError(t, model.ReconcileImageJobAccounting(ctx))
			require.NoError(t, model.DB.First(&token, token.Id).Error)
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, 1000-tc.actual, token.RemainQuota)
			assert.Equal(t, tc.actual, token.UsedQuota)
			assert.Equal(t, 3000-tc.actual, user.Quota)
			// 先证明没有原调用重试也完成，再验证旧会话重入仍只调整一次。
			if tc.actual != 0 {
				require.NoError(t, session.Settle(tc.actual))
			}
			session.Refund(c)
			require.NoError(t, model.DB.First(&token, token.Id).Error)
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, 1000-tc.actual, token.RemainQuota)
			assert.Equal(t, 3000-tc.actual, user.Quota)
			if tc.shared {
				require.NoError(t, model.DB.First(&transferToken, transferToken.Id).Error)
				assert.Equal(t, 1000, transferToken.RemainQuota, "移交不得将旧请求费用算给同 owner 的另一令牌")
			}
			var receipts int64
			require.NoError(t, model.DB.Model(&model.ImageJobAccountingTransfer{}).Where("token_id = ?", transferToken.Id).Count(&receipts).Error)
			assert.EqualValues(t, 1, receipts, fmt.Sprintf("%s 的移交只应用一次", tc.name))
		})
	}
}
