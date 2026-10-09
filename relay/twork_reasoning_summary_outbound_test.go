package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 在真实 HTTP 客户端边界捕获请求，立即停止，不连接上游或进入响应结算。
type tworkReasoningSummaryRoundTripper struct {
	request *http.Request
	body    []byte
	calls   int
}

func (capture *tworkReasoningSummaryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	capture.calls++
	capture.request = request
	var err error
	capture.body, err = io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	return nil, errors.New("已捕获合成出站请求，停止响应与计费")
}

func TestTworkReasoningSummaryReachesResponsesUpstreamWithoutChangingFixedOrLegacy(t *testing.T) {
	client := service.GetHttpClient()
	if client == nil {
		service.InitHttpClient()
		client = service.GetHttpClient()
	}
	require.NotNil(t, client)
	oldTransport, oldTimeout, oldDebug := client.Transport, client.Timeout, common.DebugEnabled
	oldCache := common.GetDiskCacheConfig()
	common.DebugEnabled = false
	common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: false})
	client.Timeout = 0
	t.Cleanup(func() {
		client.Transport, client.Timeout = oldTransport, oldTimeout
		common.DebugEnabled = oldDebug
		common.SetDiskCacheConfig(oldCache)
	})

	raw := []byte(`{
		"model":"fixture-model","stream":true,"store":false,"prompt_cache_key":"fixture-cache",
		"reasoning":{"effort":"high","summary":"auto"},"custom_extension":{"note":"原扩展🙂","enabled":true},
		"tools":[{"type":"function","name":"read","description":"读合成样本","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]},"strict":true}],
		"input":[
			{"role":"system","content":"规则"},
			{"role":"developer","content":"输出保持中文"},
			{"role":"user","content":"原任务\n🙂"},
			{"type":"reasoning","id":"rs_fixture","encrypted_content":"fixture-only-opaque","content":null,"status":null,"summary":[{"type":"summary_text","text":"  摘要\n逐字🙂  "},{"type":"summary_text","text":"二段\u2028全文"}]},
			{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"read","arguments":"{ \"path\": \"./摘要🙂.txt\", \"offset\": 0 }"},
			{"type":"function_call_output","call_id":"call_fixture","output":"工具结果\n\\路径\t🙂"},
			{"type":"reasoning","id":"rs_opaque_fixture","encrypted_content":"fixture-only-opaque","summary":[]},
			{"role":"assistant","content":"既有回复"}
		]
	}`)
	var original map[string]any
	require.NoError(t, common.Unmarshal(raw, &original))
	publicPrefix := []any{
		map[string]any{"role": "system", "content": "规则"},
		map[string]any{"role": "developer", "content": "输出保持中文"},
		map[string]any{"role": "user", "content": "原任务\n🙂"},
	}
	publicTail := []any{
		map[string]any{"type": "function_call", "call_id": "call_fixture", "name": "read", "arguments": `{ "path": "./摘要🙂.txt", "offset": 0 }`},
		map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": "工具结果\n\\路径\t🙂"},
		map[string]any{"role": "assistant", "content": "既有回复"},
	}
	newHistory := append(append(append([]any{}, publicPrefix...),
		map[string]any{"role": "assistant", "content": "  摘要\n逐字🙂  "},
		map[string]any{"role": "assistant", "content": "二段\u2028全文"}), publicTail...)
	legacyHistory := append(append([]any{}, publicPrefix...), publicTail...)
	expectedHeaders := http.Header{
		"Accept":        []string{"application/json"},
		"Authorization": []string{"Bearer fixture-only-key"},
		"Content-Type":  []string{"application/json"},
	}

	for _, arm := range []struct {
		name, version string
		modelRoute    bool
		wantHistory   []any
	}{
		{"model_pi_one", "1.0.0", true, newHistory},
		{"model_pi_one_one", "1.1.0", true, newHistory},
		{"fixed_passthrough", "1.0.0", false, nil},
		{"model_legacy", "0.85.1", true, legacyHistory},
	} {
		t.Run(arm.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("Accept", "application/json")
			c.Request.Header.Set("X-Twork-Client-Version", "4.0.0")
			c.Request.Header.Set("X-Twork-Client-Capabilities", "model-routes-v3")
			c.Request.Header.Set("X-Twork-Agent-Runtime", "pi")
			c.Request.Header.Set("X-Twork-Wire-Api", "responses")
			c.Request.Header.Set("X-Twork-Pi-Compatibility-Version", arm.version)
			common.SetContextKey(c, constant.ContextKeyTworkPiCompatibilityVersion, arm.version)
			if arm.modelRoute {
				c.Request.Header.Set("X-Twork-Route-Mode", "model")
				common.SetContextKey(c, constant.ContextKeyTworkRouteModel, "fixture-model")
			} else {
				c.Request.Header.Set("X-Twork-Channel-Id", "168")
				common.SetContextKey(c, constant.ContextKeyTworkExplicitChannelRoute, true)
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "168")
			}
			// 复用已鉴权/已选路后的上下文形状，不接真实鉴权、数据库或渠道配置。
			common.SetContextKey(c, constant.ContextKeyChannelId, 168)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://reasoning-summary.invalid")
			common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture-only-key")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "fixture-model")
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{TworkRuntime: "pi", TworkWireAPI: "responses", PassThroughBodyEnabled: true})
			common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{})
			common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, map[string]any{})
			c.Set("model_mapping", "")
			c.Set("channel_organization", "")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			var requestDTO dto.OpenAIResponsesRequest
			require.NoError(t, common.Unmarshal(raw, &requestDTO))
			info := &relaycommon.RelayInfo{
				OriginModelName: "fixture-model", RequestURLPath: "/v1/responses", IsStream: true, DisablePing: true,
				RelayMode: relayconstant.RelayModeResponses, RelayFormat: types.RelayFormatOpenAIResponses, Request: &requestDTO,
			}
			capture := &tworkReasoningSummaryRoundTripper{}
			client.Transport = capture
			stopped := ResponsesHelper(c, info)
			require.NotNil(t, stopped)
			require.Equal(t, types.ErrorCodeDoRequestFailed, stopped.GetErrorCode())
			require.Equal(t, 1, capture.calls)
			require.NotNil(t, capture.request)
			assert.Equal(t, "https://reasoning-summary.invalid/v1/responses", capture.request.URL.String())
			assert.Equal(t, http.MethodPost, capture.request.Method)
			assert.True(t, capture.request.Context() == ctx)
			assert.Equal(t, expectedHeaders, capture.request.Header)
			assert.Equal(t, arm.modelRoute, relaycommon.IsTworkModelRoute(c))
			var actual, expected map[string]any
			require.NoError(t, common.Unmarshal(capture.body, &actual))
			require.NoError(t, common.Unmarshal(raw, &expected))
			if arm.modelRoute {
				expected["input"] = arm.wantHistory
			} else {
				assert.Equal(t, raw, capture.body)
			}
			assert.Equal(t, expected, actual)
			assert.Equal(t, original["tools"], actual["tools"])
		})
	}
}
