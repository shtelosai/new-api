package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkUsageSyncRelayAndUserLogAPI(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ name, turn, aux string }{
		{"reply", hash, ""},
		{"auxiliary", "", "title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				body := `{"id":"ok","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.User{}))
			common.LogConsumeEnabled = true
			request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+name+`","input":"hello","stream":false}`))
			for key, value := range map[string]string{"Content-Type": "application/json", "X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3", "X-Twork-Route-Mode": "model", "X-Twork-Agent-Runtime": "pi", "X-Twork-Wire-Api": "responses", "X-Twork-Turn": tc.turn, "X-Twork-Session": hash, "X-Twork-Aux": tc.aux} {
				request.Header.Set(key, value)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, request)
			require.Equal(t, 200, w.Code, w.Body.String())
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, tc.turn, logs[0].TworkTurn)
			assert.Equal(t, 10, logs[0].PromptTokens)
			assert.Equal(t, 5, logs[0].CompletionTokens)
			assert.False(t, logs[0].IsStream)
			// Exercise the user-facing HTTP serialization and its admin-field filtering.
			api := gin.New()
			api.GET("/api/log/self", func(c *gin.Context) { c.Set("id", logs[0].UserId); GetUserLogs(c) })
			w = httptest.NewRecorder()
			api.ServeHTTP(w, httptest.NewRequest("GET", "/api/log/self?type=2", nil))
			require.Equal(t, 200, w.Code)
			var result struct {
				Success bool `json:"success"`
				Data    struct {
					Items []model.Log `json:"items"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
			require.True(t, result.Success, w.Body.String())
			require.Len(t, result.Data.Items, 1)
			assert.Equal(t, tc.turn, result.Data.Items[0].TworkTurn)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(result.Data.Items[0].Other, &other))
			assert.Equal(t, hash, other["twork_session"])
			if tc.aux != "" {
				assert.Equal(t, tc.aux, other["twork_aux"])
			} else {
				assert.NotContains(t, other, "twork_aux")
			}
			assert.NotContains(t, other, "admin_info")
		})
	}
}
