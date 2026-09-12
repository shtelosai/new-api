package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmployeeChatSignedRoute(t *testing.T) {
	require.NoError(t, i18n.Init())
	const key = "employee-chat-test-signing-key-32bytes"
	const body = `{"model":"gpt-5","input":"你好"}`
	for _, tc := range []struct {
		name, mutate string
		want         int
	}{
		{"企业签名使用专用渠道且不写授权", "", 200},
		{"签名缺失不能使用专用渠道", "missing", 403},
		{"密钥未配置", "disabled", 403},
		{"签名错误", "signature", 403},
		{"签名过期", "expired", 403},
		{"未来签名", "future", 403},
		{"请求正文变更", "body", 403},
		{"付款账号变更", "payer", 403},
		{"渠道变更", "channel", 403},
		{"缺少管理员渠道后缀", "no_suffix", 403},
		{"重复签名", "duplicate", 403},
		{"模型下线", "model_disabled", 403},
		{"渠道下线", "channel_disabled", 403},
		{"模型不在渠道中", "model", 403},
		{"请求协议不匹配", "wire", 403},
		{"二级菜单不能使用企业签名绕过正式授权", "submenu", 403},
		{"付款分组不匹配", "group", 403},
		{"所有上游密钥禁用时不得进入转发", "keys_disabled", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDistributorTokenAffinityDB(t)
			t.Setenv("TWORK_EMPLOYEE_CHAT_BRIDGE_KEY", key)
			seedDistributorChannel(t, 4801, "gpt-5", 0)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"responses"}`).Error)
			if tc.mutate == "disabled" {
				t.Setenv("TWORK_EMPLOYEE_CHAT_BRIDGE_KEY", "")
			}
			if tc.mutate == "channel_disabled" {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("status", 2).Error)
			}
			if tc.mutate == "model_disabled" {
				require.NoError(t, model.DB.Create(&model.ChannelModelDisabled{ChannelId: 4801, Model: "gpt-5", Source: "manual"}).Error)
			}
			if tc.mutate == "model" {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("models", "another").Error)
			}
			if tc.mutate == "wire" {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"chat_completions"}`).Error)
			}
			if tc.mutate == "submenu" {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu"}`).Error)
			}
			if tc.mutate == "group" {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("group", "other").Error)
			}
			if tc.mutate == "keys_disabled" {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("channel_info", model.ChannelInfo{IsMultiKey: true, MultiKeySize: 1, MultiKeyStatusList: map[int]int{0: 2}}).Error)
			}
			result := requestRuntimeRoute(t, 991, "default", "/v1/responses", body, tc.want, func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "4801")
				c.Set("token_model_limit_enabled", true)
				c.Set("token_model_limit", map[string]bool{"compile-only": true})
				stamp := time.Now().Unix()
				if tc.mutate == "expired" {
					stamp -= 61
				}
				if tc.mutate == "future" {
					stamp += 61
				}
				signedBody := body
				if tc.mutate == "body" {
					signedBody += " "
				}
				mac := hmac.New(sha256.New, []byte(key))
				fmt.Fprintf(mac, "twork-employee-chat-v1\n%d\nPOST\n/v1/responses\n991\n4801\n%x", stamp, sha256.Sum256([]byte(signedBody)))
				proof := fmt.Sprintf("%d.%x", stamp, mac.Sum(nil))
				if tc.mutate == "signature" {
					proof += "00"
				}
				if tc.mutate != "missing" {
					c.Request.Header.Set("X-Twork-Employee-Authorization", proof)
				}
				if tc.mutate == "duplicate" {
					c.Request.Header.Add("X-Twork-Employee-Authorization", proof)
				}
				if tc.mutate == "payer" {
					c.Set("token_id", 992)
				}
				if tc.mutate == "channel" {
					c.Set("specific_channel_id", "4802")
				}
				if tc.mutate == "no_suffix" {
					delete(c.Keys, "specific_channel_id")
				}
			})
			if tc.want == 200 {
				assert.Equal(t, 4801, result.ChannelID)
			}
			var grants int64
			require.NoError(t, model.DB.Model(&model.TokenModelChannel{}).Count(&grants).Error)
			assert.Zero(t, grants)
		})
	}
}
