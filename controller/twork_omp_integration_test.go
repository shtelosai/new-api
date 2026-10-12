package controller

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 此验收只在显式提供已修改的 Twork 工作树时运行；上游读取当前账号的真实授权。
func TestOMPRealSDKGateway(t *testing.T) {
	root := os.Getenv("TWORK_OMP_WORKTREE")
	if root == "" || os.Getenv("TWORK_OMP_REAL") != "1" {
		t.Skip("需显式启用当前账号真实模型验收")
	}
	require.NoError(t, i18n.Init())
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 60
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	service.InitTokenEncoders()
	names := []string{"claude-haiku-5-5", "qwen3.8-flash", "glm-5.3-flash"}
	wires := []string{"anthropic-messages", "responses", "chat-completions"}
	channels := setupRelaySoftCooldownChannels(t, strings.Join(names, ","), 3, 0, 1)
	require.NoError(t, model.DB.AutoMigrate(&model.TokenModelChannel{}, &model.User{}, &model.Log{}))
	require.NoError(t, model.DB.Where("1 = 1").Delete(&model.Ability{}).Error)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"claude-haiku-5-5":0,"qwen3.8-flash":0,"glm-5.3-flash":0}`))
	for i, ch := range channels {
		kind := constant.ChannelTypeOpenAI
		if i == 0 {
			kind = constant.ChannelTypeAnthropic
		}
		require.NoError(t, model.DB.Model(ch).Updates(map[string]any{"type": kind, "models": names[i], "setting": `{"twork_runtime":"omp","twork_wire_api":"` + wires[i] + `"}`}).Error)
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: names[i], ChannelId: ch.Id, Enabled: true, Priority: ch.Priority, Weight: *ch.Weight}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 8900, ModelId: names[i], ChannelId: ch.Id}).Error)
	}
	common.LogConsumeEnabled = true
	model.InitChannelCache()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer omp-local-acceptance" && c.GetHeader("x-api-key") != "omp-local-acceptance" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, 8900)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	router.Use(middleware.Distribute())
	router.POST("/v1/messages", func(c *gin.Context) { Relay(c, types.RelayFormatClaude) })
	router.POST("/v1/responses", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIResponses) })
	router.POST("/v1/chat/completions", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	server := httptest.NewServer(router)
	defer server.Close()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previous := client.Transport
	t.Cleanup(func() { client.Transport = previous })
	var proxyURL *url.URL
	var lock sync.RWMutex
	calls := map[string]int{}
	client.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		lock.RLock()
		target := proxyURL
		lock.RUnlock()
		if target == nil {
			return nil, io.ErrClosedPipe
		}
		cloned := req.Clone(req.Context())
		cloned.URL.Scheme = target.Scheme
		cloned.URL.Host = target.Host
		cloned.Host = target.Host
		lock.Lock()
		calls[cloned.URL.Path]++
		lock.Unlock()
		return http.DefaultTransport.RoundTrip(cloned)
	})
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bun", "run", filepath.Join(root, "scripts/omp-m2-probe.ts"))
	command.Dir = root
	command.Env = append(os.Environ(), "TWORK_OMP_TEST_GATEWAY="+server.URL)
	command.ExtraFiles = []*os.File{writer}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	require.NoError(t, command.Start())
	writer.Close()
	var bootstrap struct {
		Upstream string `json:"upstream"`
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 4096))
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(raw, &bootstrap), "%s", output.String())
	target, err := url.Parse(bootstrap.Upstream)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", target.Hostname())
	lock.Lock()
	proxyURL = target
	lock.Unlock()
	err = command.Wait()
	t.Log(output.String())
	require.NoError(t, err)
	lock.RLock()
	defer lock.RUnlock()
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		assert.Equal(t, 2, calls[path], path)
	}
}
