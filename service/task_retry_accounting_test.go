package service

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskRetryAccounting(t *testing.T) {
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	for index, tc := range []struct {
		name                          string
		wallet, token, target, actual int
		failed, reject                bool
	}{
		{"more expensive", 1000, 1000, 300, 300, false, false},
		{"cheaper", 1000, 1000, 50, 50, false, false},
		{"free retry", 1000, 1000, 0, 0, false, false},
		{"wallet insufficient", 200, 1000, 300, 0, true, true},
		{"token insufficient", 1000, 200, 300, 0, true, true},
		{"all attempts failed", 1000, 1000, 300, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			id := 911000 + index
			seedUser(t, id, tc.wallet)
			seedToken(t, id, id, fmt.Sprint(id), tc.token)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
			info := &relaycommon.RelayInfo{UserId: id, TokenId: id, TokenKey: fmt.Sprint(id), ForcePreConsume: true}
			info.UserSetting.BillingPreference = "wallet_only"
			require.Nil(t, PreConsumeBilling(c, 100, info))
			first := info.Billing
			retryErr := PreConsumeBilling(c, tc.target, info)
			assert.Same(t, first, info.Billing, "重试必须沿用原预扣和退款状态")
			reserved := 100
			if tc.reject {
				require.NotNil(t, retryErr)
				assert.Equal(t, 403, retryErr.StatusCode)
			} else {
				require.Nil(t, retryErr)
				if tc.target > reserved {
					reserved = tc.target
				}
				require.Nil(t, PreConsumeBilling(c, tc.target, info), "重复预留不应再次扣费")
			}
			var user model.User
			var token model.Token
			require.NoError(t, model.DB.First(&user, id).Error)
			require.NoError(t, model.DB.First(&token, id).Error)
			assert.Equal(t, tc.wallet-reserved, user.Quota)
			assert.Equal(t, tc.token-reserved, token.RemainQuota)
			assert.Equal(t, reserved, token.UsedQuota)
			assert.Equal(t, reserved, info.Billing.GetPreConsumedQuota())
			if tc.failed {
				info.Billing.Refund(c)
				info.Billing.Refund(c)
				require.Eventually(t, func() bool {
					var u model.User
					var tok model.Token
					return model.DB.First(&u, id).Error == nil && model.DB.First(&tok, id).Error == nil && u.Quota == tc.wallet && tok.RemainQuota == tc.token && tok.UsedQuota == 0
				}, time.Second, time.Millisecond, "失败后必须退还首次和所有追加预扣")
				require.NoError(t, info.Billing.Settle(350), "退款后迟到的结算不能再扣费")
			} else {
				require.NoError(t, info.Billing.Settle(tc.actual))
				require.NoError(t, info.Billing.Settle(tc.actual))
				info.Billing.Refund(c)
			}
			want := tc.actual
			if tc.failed {
				want = 0
			}
			require.NoError(t, model.DB.First(&user, id).Error)
			require.NoError(t, model.DB.First(&token, id).Error)
			assert.Equal(t, tc.wallet-want, user.Quota)
			assert.Equal(t, tc.token-want, token.RemainQuota)
			assert.Equal(t, want, token.UsedQuota)
		})
	}
}

func TestTaskRetrySubscriptionAccounting(t *testing.T) {
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	require.NoError(t, model.DB.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPreConsumeRecord{}))
	for index, tc := range []struct {
		name           string
		total          int64
		token          int
		failed, reject bool
	}{
		{"success", 1000, 1000, false, false},
		{"subscription insufficient", 200, 1000, true, true},
		{"token insufficient compensates subscription", 1000, 200, true, true},
		{"all failed refund", 1000, 1000, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			id := 911100 + index
			seedUser(t, id, 1000)
			seedToken(t, id, id, fmt.Sprint(id), tc.token)
			plan := model.SubscriptionPlan{Id: id, Title: tc.name, QuotaResetPeriod: "never"}
			require.NoError(t, model.DB.Create(&plan).Error)
			t.Cleanup(func() {
				model.DB.Unscoped().Delete(&plan)
				model.DB.Where("user_id = ?", id).Delete(&model.SubscriptionPreConsumeRecord{})
			})
			seedSubscription(t, id, id, tc.total, 0)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", id).Update("plan_id", id).Error)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			info := &relaycommon.RelayInfo{UserId: id, TokenId: id, TokenKey: fmt.Sprint(id), RequestId: fmt.Sprint("task-retry-", id), ForcePreConsume: true}
			info.UserSetting.BillingPreference = "subscription_only"
			require.Nil(t, PreConsumeBilling(c, 100, info))
			err := PreConsumeBilling(c, 300, info)
			reserved := 300
			if tc.reject {
				require.NotNil(t, err)
				assert.Equal(t, 403, err.StatusCode)
				reserved = 100
			} else {
				require.Nil(t, err)
				require.Nil(t, PreConsumeBilling(c, 300, info))
			}
			var sub model.UserSubscription
			var tok model.Token
			require.NoError(t, model.DB.First(&sub, id).Error)
			require.NoError(t, model.DB.First(&tok, id).Error)
			assert.EqualValues(t, reserved, sub.AmountUsed)
			assert.Equal(t, tc.token-reserved, tok.RemainQuota)
			want := 300
			if tc.failed {
				want = 0
				info.Billing.Refund(c)
				info.Billing.Refund(c)
				require.Eventually(t, func() bool {
					var s model.UserSubscription
					var token model.Token
					return model.DB.First(&s, id).Error == nil && model.DB.First(&token, id).Error == nil && s.AmountUsed == 0 && token.RemainQuota == tc.token
				}, time.Second, time.Millisecond)
				require.NoError(t, model.RefundSubscriptionPreConsume(info.RequestId), "重复处理预扣退款回执不能再次退费")
			} else {
				require.NoError(t, info.Billing.Settle(300))
				require.NoError(t, info.Billing.Settle(300))
				info.Billing.Refund(c)
			}
			require.NoError(t, model.DB.First(&sub, id).Error)
			require.NoError(t, model.DB.First(&tok, id).Error)
			assert.EqualValues(t, want, sub.AmountUsed)
			assert.Equal(t, tc.token-want, tok.RemainQuota)
			assert.Equal(t, want, tok.UsedQuota)
			var user model.User
			require.NoError(t, model.DB.First(&user, id).Error)
			assert.Equal(t, 1000, user.Quota, "订阅重试不能改扣钱包")
		})
	}
}
