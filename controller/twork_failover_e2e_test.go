package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 端到端验收（2026-10-23 换渠道规则）：真实网关代码 + 本地库里的 3 个渠道（各自可控的假上游）+ Twork 的 OMP 宿主。
// 只在显式提供 Twork 工作树时运行：TWORK_FAILOVER_E2E=1 TWORK_OMP_WORKTREE=<路径>。
func TestTworkFailoverRulesEndToEnd(t *testing.T) {
	root := os.Getenv("TWORK_OMP_WORKTREE")
	if root == "" || os.Getenv("TWORK_FAILOVER_E2E") != "1" {
		t.Skip("需显式启用：TWORK_FAILOVER_E2E=1 TWORK_OMP_WORKTREE=<twork 工作树>")
	}
	require.NoError(t, i18n.Init())
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 60
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	service.InitTokenEncoders()

	const modelName = "e2e-model"
	const tokenID = 8900
	channels := setupRelaySoftCooldownChannels(t, modelName, 3, 3, 5)
	require.NoError(t, model.DB.AutoMigrate(&model.TokenModelChannel{}, &model.ChannelModelHealth{}))
	ban := 1
	for _, ch := range channels {
		require.NoError(t, model.DB.Model(ch).Updates(map[string]any{
			"type": constant.ChannelTypeOpenAI, "auto_ban": ban,
			"setting": `{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`,
		}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: tokenID, ModelId: modelName, ChannelId: ch.Id}).Error)
	}
	healthSetting := operation_setting.GetChannelHealthSetting()
	healthSetting.SoftFailureCooldownSeconds = 2
	healthSetting.ModelLevelAutoDisableEnabled = true
	originalAutoDisable := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = originalAutoDisable })
	model.InitChannelCache()

	// 每个渠道的假上游：ok 正常回复「来自渠道N」，fail502 / fail404 回对应错误。
	var lock sync.Mutex
	modes := map[string]string{}
	calls := map[string]int{}
	clientRequests := 0
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	httpClient := service.GetHttpClient()
	previousTransport := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = previousTransport })
	httpClient.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, req.Body)
		id := strings.TrimSuffix(strings.TrimPrefix(req.URL.Host, "channel-"), ".test")
		lock.Lock()
		calls[id]++
		mode := modes[id]
		lock.Unlock()
		switch mode {
		case "fail502":
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"The upstream service could not complete the request.","type":"upstream_error"}}`)), Request: req}, nil
		case "fail404":
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Model is not available for this group","type":"new_api_error","code":"model_not_found"}}`)), Request: req}, nil
		}
		chunk := func(delta, finish string) string {
			finishJSON := "null"
			if finish != "" {
				finishJSON = `"` + finish + `"`
			}
			return fmt.Sprintf("data: {\"id\":\"e2e\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"%s\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%s}]}\n\n", modelName, delta, finishJSON)
		}
		body := chunk(fmt.Sprintf(`{"role":"assistant","content":"来自渠道%s"}`, id), "") + chunk(`{}`, "stop") +
			"data: {\"id\":\"e2e\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"" + modelName + "\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	control := router.Group("/e2e")
	control.POST("/mode", func(c *gin.Context) {
		lock.Lock()
		modes[c.Query("channel")] = c.Query("mode")
		lock.Unlock()
		c.Status(http.StatusNoContent)
	})
	control.POST("/manual-lock", func(c *gin.Context) {
		var id int
		_, _ = fmt.Sscan(c.Query("channel"), &id)
		require.NoError(t, model.DB.Create(&model.ChannelModelDisabled{ChannelId: id, Model: modelName, Source: model.DisabledSourceManual, Reason: "人工锁定"}).Error)
		model.InitChannelCache()
		c.Status(http.StatusNoContent)
	})
	control.GET("/state", func(c *gin.Context) {
		lock.Lock()
		snapshot := map[string]int{}
		for k, v := range calls {
			snapshot[k] = v
		}
		requests := clientRequests
		lock.Unlock()
		var disabled []model.ChannelModelDisabled
		require.NoError(t, model.DB.Where("model = ?", modelName).Find(&disabled).Error)
		disabledBy := map[string]string{}
		for _, row := range disabled {
			disabledBy[fmt.Sprint(row.ChannelId)] = row.Source
		}
		cooling := map[string]bool{}
		for _, ch := range channels {
			_, cooling[fmt.Sprint(ch.Id)] = service.GetChannelSoftCooldown(nil, ch.Id, modelName)
		}
		c.JSON(http.StatusOK, gin.H{"calls": snapshot, "clientRequests": requests, "disabled": disabledBy, "cooling": cooling})
	})
	api := router.Group("/v1")
	api.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer failover-e2e-local" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		lock.Lock()
		clientRequests++
		lock.Unlock()
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, tokenID)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	api.Use(middleware.Distribute())
	api.POST("/chat/completions", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bun", "run", filepath.Join(root, "scripts/omp-failover-e2e.ts"))
	command.Dir = root
	command.Env = append(os.Environ(), "TWORK_E2E_GATEWAY="+server.URL)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	t.Log(output.String())
	require.NoError(t, err)
	require.Contains(t, output.String(), "FAILOVER E2E PASS")
}
