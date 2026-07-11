package claude

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 复现线上问题：dashscope 风格通用错误体的 message 是字符串，
// 按 ClaudeResponse 反序列化必然失败（message 期望是对象）。
func TestClaudeResponseUnmarshalFailsOnGenericErrorBody(t *testing.T) {
	body := `{"code":"InvalidParameter","message":"Range of input length should be [1, 129024]","request_id":"abc-123"}`
	var claudeResponse dto.ClaudeResponse
	err := common.Unmarshal([]byte(body), &claudeResponse)
	require.Error(t, err, "前提确认：通用错误体应导致 ClaudeResponse 解析失败")
	assert.Contains(t, err.Error(), "ClaudeMediaMessage")
}

func TestTryParseClaudeGenericError(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantNil     bool
		wantMessage string
	}{
		{
			name:        "dashscope 风格 code+message",
			body:        `{"code":"InvalidParameter","message":"Range of input length should be [1, 129024]","request_id":"abc-123"}`,
			wantMessage: "Range of input length should be [1, 129024] (upstream code: InvalidParameter)",
		},
		{
			name:        "仅 message 无 code",
			body:        `{"message":"internal error"}`,
			wantMessage: "internal error",
		},
		{
			name:        "数字 code",
			body:        `{"code":429,"message":"throttled"}`,
			wantMessage: "throttled (upstream code: 429)",
		},
		{
			name:    "message 为空串不兜底",
			body:    `{"code":"X","message":""}`,
			wantNil: true,
		},
		{
			name:    "无 message 字段不兜底",
			body:    `{"code":"X"}`,
			wantNil: true,
		},
		{
			name:    "message 是对象（正常 Claude 响应形态）不兜底",
			body:    `{"type":"message_start","message":{"role":"assistant"}}`,
			wantNil: true,
		},
		{
			name:    "非法 JSON 不兜底",
			body:    `{"message":`,
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tryParseClaudeGenericError([]byte(tt.body))
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantMessage, got.Err.Error())
			assert.Equal(t, types.ErrorCode("upstream_error"), got.GetErrorCode())
		})
	}
}
