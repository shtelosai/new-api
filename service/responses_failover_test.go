package service

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newResponsesFailoverContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyTworkRoutePolicy, model.TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"})
	return c
}

func TestResponsesFailoverScopeAndClassification(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	for _, tc := range []struct {
		name string
		err  *types.NewAPIError
		soft bool
	}{
		{"503", types.WithOpenAIError(types.OpenAIError{Type: "server_error", Message: "upstream service error"}, 503), true},
		{"timeout", types.NewOpenAIError(fmt.Errorf("upstream timeout"), types.ErrorCodeDoRequestFailed, 504), true},
		{"connection", types.NewOpenAIError(fmt.Errorf("connection refused"), types.ErrorCodeDoRequestFailed, 500), true},
		{"limit", types.WithOpenAIError(types.OpenAIError{Type: "rate_limit_error"}, 429), true},
		{"auth", types.WithOpenAIError(types.OpenAIError{Type: "authentication_error"}, 502), false},
		{"quota", types.WithOpenAIError(types.OpenAIError{Code: "insufficient_quota"}, 429), false},
		{"input", types.WithOpenAIError(types.OpenAIError{Type: "invalid_request_error"}, 502), false},
		{"local", types.NewError(fmt.Errorf("invalid price"), types.ErrorCodeModelPriceError), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.soft, IsRelaySoftFailure(newResponsesFailoverContext(), tc.err))
		})
	}
	for _, boundary := range []string{"fixed", "token-fixed", "legacy", "compact", "disabled", "cancel"} {
		t.Run(boundary, func(t *testing.T) {
			c := newResponsesFailoverContext()
			switch boundary {
			case "fixed":
				common.SetContextKey(c, constant.ContextKeyTworkExplicitChannelRoute, true)
			case "token-fixed":
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, 170)
			case "legacy":
				c.Set(string(constant.ContextKeyTworkRoutePolicy), model.TworkRoutePolicy{})
			case "compact":
				c.Request.URL.Path = "/v1/responses/compact"
			case "disabled":
				operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = false
				defer func() { operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = true }()
			case "cancel":
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			if boundary == "cancel" {
				assert.False(t, IsRelaySoftFailure(c, types.NewOpenAIError(context.Canceled, types.ErrorCodeDoRequestFailed, 500)))
			} else {
				assert.False(t, TworkResponsesFailoverEnabled(c))
			}
		})
	}
}

func TestResponsesRecoveryProbeAndNewerFailures(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			setupChannelSoftCooldownTest(t, true, 30)
			if backend == "redis" {
				if os.Getenv("NEWAPI_TEST_LOCAL_REDIS") != "1" {
					t.Skip("设置 NEWAPI_TEST_LOCAL_REDIS=1 启动隔离的本地 Redis 验证")
				}
				binary, err := exec.LookPath("redis-server")
				require.NoError(t, err)
				dir, err := os.MkdirTemp("/tmp", "newapi-redis-")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
				socket := filepath.Join(dir, "redis.sock")
				cmd := exec.Command(binary, "--port", "0", "--unixsocket", socket, "--save", "", "--appendonly", "no")
				require.NoError(t, cmd.Start())
				t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
				client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket})
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				require.Eventually(t, func() bool { return client.Ping(context.Background()).Err() == nil }, 3*time.Second, 10*time.Millisecond)
				useRedisClientForSoftCooldownTest(t, client)
			}
			const id = 99017
			name := "gpt-probe-" + backend
			key := fmt.Sprintf("%d:%s", id, name)
			cleanupChannelSoftCooldownKey(t, id, name)
			expired := ChannelSoftCooldownEntry{ExpiresAt: time.Now().Add(-time.Second), StatusCode: 503, ProbeAfterExpiry: true}
			require.NoError(t, getChannelSoftCooldownCache().SetWithTTL(key, expired, time.Hour))
			first := newResponsesFailoverContext()
			_, cooling := GetChannelSoftCooldown(first, id, name)
			require.False(t, cooling, "首个请求可以试探")
			_, cooling = GetChannelSoftCooldown(newResponsesFailoverContext(), id, name)
			assert.True(t, cooling, "其他请求继续避开试探中的渠道")
			_, cooling = GetChannelSoftCooldown(newResponsesFailoverContext(), id, name+"-other")
			assert.False(t, cooling)
			CompleteResponsesRecovery(first, id, name)
			_, cooling = GetChannelSoftCooldown(newResponsesFailoverContext(), id, name)
			assert.False(t, cooling, "成功试探后恢复")

			expired.ExpiresAt = expired.ExpiresAt.Add(-time.Second)
			require.NoError(t, getChannelSoftCooldownCache().SetWithTTL(key, expired, time.Hour))
			oldProbe := newResponsesFailoverContext()
			_, cooling = GetChannelSoftCooldown(oldProbe, id, name)
			require.False(t, cooling)
			RecordChannelSoftCooldown(newResponsesFailoverContext(), id, name, 503, "server_error")
			CompleteResponsesRecovery(oldProbe, id, name)
			_, cooling = GetChannelSoftCooldown(newResponsesFailoverContext(), id, name)
			assert.True(t, cooling, "旧成功不能抹掉新故障")
		})
	}
}
