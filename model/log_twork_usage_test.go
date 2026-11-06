package model

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsumeLogsExposeTworkUsageToOwner(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name   string
		usage  common.TworkUsage
		stream bool
	}{
		{"sync", common.TworkUsage{Turn: hash, Session: hash}, false},
		{"stream", common.TworkUsage{Turn: hash, Session: hash}, true},
		{"auxiliary", common.TworkUsage{Session: hash, Aux: "title"}, false},
		{"legacy", common.TworkUsage{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Set(common.TworkUsageContextKey, tc.usage)
			if tc.stream {
				c.SSEvent("response.output_text.delta", gin.H{"delta": "hello"})
				c.Writer.Flush()
			}
			RecordConsumeLog(c, 1, RecordConsumeLogParams{Quota: 123, ModelName: "test", PromptTokens: 10, CompletionTokens: 5, IsStream: tc.stream, Other: map[string]interface{}{"model_ratio": 2, "admin_info": map[string]any{"private": true}}})
			var stored Log
			require.NoError(t, LOG_DB.First(&stored).Error)
			assert.Equal(t, tc.usage.Turn, stored.TworkTurn)
			assert.Equal(t, 123, stored.Quota)
			assert.Equal(t, tc.stream, stored.IsStream)
			logs, total, err := GetUserLogs(1, LogTypeConsume, 0, 0, "", "", 0, 10, "", "", "")
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Len(t, logs, 1)
			assert.Equal(t, tc.usage.Turn, logs[0].TworkTurn)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			assert.EqualValues(t, 2, other["model_ratio"])
			assert.NotContains(t, other, "admin_info")
			if tc.usage.Session != "" {
				assert.Equal(t, tc.usage.Session, other["twork_session"])
			} else {
				assert.NotContains(t, other, "twork_session")
			}
			if tc.usage.Aux != "" {
				assert.Equal(t, tc.usage.Aux, other["twork_aux"])
			} else {
				assert.NotContains(t, other, "twork_aux")
			}
			_, total, err = GetUserLogs(2, LogTypeConsume, 0, 0, "", "", 0, 10, "", "", "")
			require.NoError(t, err)
			assert.Zero(t, total)
		})
	}
}
