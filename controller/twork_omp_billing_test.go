package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOMPFailedStreamKeepsPartialUsageAndSettlesOnlyOnce(t *testing.T) {
	calls := 0
	router, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failed\"},\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":60}}}}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	// OMP 只能使用明确标记的 omp 渠道。
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("models = ?", name).Update("setting", `{"twork_runtime":"omp","twork_wire_api":"responses","pass_through_body_enabled":true}`).Error)
	model.InitChannelCache()
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.User{}))
	common.LogConsumeEnabled = true
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+name+`","input":"hello","stream":true}`))
	for key, value := range map[string]string{"Content-Type": "application/json", "X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v4", "X-Twork-Agent-Runtime": "omp", "X-Twork-Omp-Version": "18.10.1", "X-Twork-Wire-Api": "responses", "X-Twork-Route-Mode": "model"} {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, 1, calls, "输出后的失败不能重试并重复结算")
	assert.Contains(t, recorder.Body.String(), "partial")
	assert.Contains(t, recorder.Body.String(), `"type":"error"`)
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("model_name = ? AND type = ?", name, model.LogTypeConsume).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 100, logs[0].PromptTokens)
	assert.Equal(t, 20, logs[0].CompletionTokens)
	assert.Contains(t, logs[0].Content, "已输出用量")
}
