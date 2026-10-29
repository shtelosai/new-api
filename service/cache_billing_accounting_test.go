package service

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain 使用内存 SQLite；核对真实预扣、钱包、令牌、日志和幂等结算。
func TestCacheBillingAccounting(t *testing.T) {
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	for index, tc := range []struct {
		name      string
		usageJSON string
		claude    bool
		want      int
	}{
		{"alias", `{"prompt_tokens":1510,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":1200,"cache_creation_input_tokens":300}}`, false, 525},
		{"duplicate aliases", `{"prompt_tokens":1510,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":1200,"cache_creation_input_tokens":300,"cache_write_tokens":300,"cached_creation_tokens":300}}`, false, 525},
		{"no alias", `{"prompt_tokens":1510,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":1200}}`, false, 450},
		{"12h split", `{"input_tokens":12000,"output_tokens":200,"input_tokens_details":{"cached_tokens":4000,"cache_write_tokens":7000,"cache_write_tokens_details":{"ttl_30m_tokens":3000,"ttl_12h_tokens":4000}}}`, false, 16150},
		{"Claude unsplit", `{"prompt_tokens":100,"completion_tokens":20,"usage_semantic":"anthropic","prompt_tokens_details":{"cached_tokens":50,"cached_creation_tokens":10000}}`, true, 12705},
		{"Claude partial split", `{"prompt_tokens":100,"completion_tokens":20,"usage_semantic":"anthropic","prompt_tokens_details":{"cached_tokens":50,"cached_creation_tokens":10000},"claude_cache_creation_5_m_tokens":2000,"claude_cache_creation_1_h_tokens":3000}`, true, 14955},
		{"Claude cache only", `{"usage_semantic":"anthropic","prompt_tokens_details":{"cached_creation_tokens":10000}}`, true, 12500},
		{"OpenAI total without TTL", `{"prompt_tokens":12000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":4000,"cache_write_tokens":7000}}`, false, 11150},
		{"no usage refunds preconsume", `{}`, false, 0},
	} {
		for _, expression := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/expression=%t", tc.name, expression), func(t *testing.T) {
				id := 980000 + index*10
				if expression {
					id++
				}
				user := model.User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Status: common.UserStatusEnabled, Quota: 1000000}
				token := model.Token{Id: id, UserId: id, Key: fmt.Sprint(id), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000}
				require.NoError(t, model.DB.Create(&user).Error)
				require.NoError(t, model.DB.Create(&token).Error)
				channel := model.Channel{Id: id, Name: fmt.Sprint(id), Type: constant.ChannelTypeOpenAI, Key: "test-only"}
				require.NoError(t, model.DB.Create(&channel).Error)
				t.Cleanup(func() {
					model.DB.Delete(&model.User{}, id)
					model.DB.Delete(&model.Token{}, id)
					model.DB.Delete(&model.Channel{}, id)
					model.LOG_DB.Where("token_id = ?", id).Delete(&model.Log{})
				})
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				info := &relaycommon.RelayInfo{
					UserId: id, TokenId: id, TokenKey: token.Key, OriginModelName: "cache-accounting", StartTime: time.Now(), ForcePreConsume: true,
					ChannelMeta: &relaycommon.ChannelMeta{ChannelId: id, ChannelType: constant.ChannelTypeOpenAI, ChannelRatio: common.GetPointer(1.5)},
					PriceData:   types.PriceData{ModelRatio: 1, CompletionRatio: 5, CacheRatio: 0.1, CacheCreationRatio: 1.25, CacheCreation5mRatio: 1.25, CacheCreation1hRatio: 2, CacheCreation12hRatio: 2.5, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 0.8}},
				}
				// 别名样例的独立手算使用输出倍率 1。
				if index < 3 {
					info.PriceData.CompletionRatio = 1
				}
				info.UserSetting.BillingPreference = "wallet_only"
				if tc.claude {
					info.FinalRequestRelayFormat = types.RelayFormatClaude
				}
				var usage dto.Usage
				require.NoError(t, common.Unmarshal([]byte(tc.usageJSON), &usage))
				if index == 3 {
					usage.BillingUsage = dto.NewOpenAIResponsesBillingUsage(&usage)
				}
				if expression {
					expr := `tier("base", p * 2 + c * 10 + cr * 0.2 + cc * 2.5 + cc1h * 4 + cc12h * 5)`
					if index < 3 {
						expr = `tier("base", p * 2 + c * 2 + cr * 0.2 + cc * 2.5 + cc12h * 5)`
					}
					info.TieredBillingSnapshot = makeSnapshot(expr, 0.8, 0, 0)
				}
				billing, apiErr := NewBillingSession(ctx, info, 100)
				require.Nil(t, apiErr)
				info.Billing = billing
				PostTextConsumeQuota(ctx, info, &usage, nil)
				want := tc.want * 12 / 10 // 分组 0.8 × 渠道 1.5，只应用一次。
				var gotUser model.User
				var gotToken model.Token
				require.NoError(t, model.DB.First(&gotUser, id).Error)
				require.NoError(t, model.DB.First(&gotToken, id).Error)
				assert.Equal(t, 1000000-want, gotUser.Quota)
				assert.Equal(t, 1000000-want, gotToken.RemainQuota)
				assert.Equal(t, want, gotUser.UsedQuota)
				assert.Equal(t, want, gotToken.UsedQuota)
				var gotChannel model.Channel
				require.NoError(t, model.DB.First(&gotChannel, id).Error)
				assert.Equal(t, int64(want), gotChannel.UsedQuota)
				var logs []model.Log
				require.NoError(t, model.LOG_DB.Where("token_id = ? AND type = ?", id, model.LogTypeConsume).Find(&logs).Error)
				require.Len(t, logs, 1)
				assert.Equal(t, want, logs[0].Quota)
				if index == 3 {
					var other map[string]any
					require.NoError(t, common.Unmarshal([]byte(logs[0].Other), &other))
					assert.Equal(t, float64(4000), other["cache_creation_tokens_12h"])
					assert.Equal(t, float64(7000), other["cache_write_tokens"])
					if expression {
						tokens, ok := other["billing_tokens"].(map[string]any)
						require.True(t, ok)
						assert.Equal(t, float64(12000), tokens["len"])
						assert.Equal(t, float64(1000), tokens["p"])
						assert.Equal(t, float64(3000), tokens["cc"])
						assert.Equal(t, float64(4000), tokens["cc12h"])
					}
				}
				require.NoError(t, billing.Settle(want))
				require.NoError(t, model.DB.First(&gotUser, id).Error)
				assert.Equal(t, 1000000-want, gotUser.Quota, "重复结算不能重复扣费")
				require.NoError(t, model.DB.First(&gotToken, id).Error)
				assert.Equal(t, 1000000-want, gotToken.RemainQuota)
			})
		}
	}
}
