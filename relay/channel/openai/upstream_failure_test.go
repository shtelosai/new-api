package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasFinishReasonToleratesWhitespaceAndIgnoresNull(t *testing.T) {
	assert.True(t, hasFinishReason(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	assert.True(t, hasFinishReason(`{"choices": [{"delta": {}, "finish_reason": "stop"}]}`))
	assert.False(t, hasFinishReason(`{"choices":[{"delta":{"content":"x"},"finish_reason":null}]}`))
	assert.False(t, hasFinishReason(`{"choices":[{"delta":{"content":"x"}}]}`))
	assert.False(t, hasFinishReason(`{"choices":[{"delta":{"content":"x"},"finish_reason":""}]}`), "空字符串不算结束")
}

func TestStreamChunkHasOutputIgnoresRoleOnlyAndUsageOnlyChunks(t *testing.T) {
	assert.False(t, streamChunkHasOutput(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`))
	assert.False(t, streamChunkHasOutput(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	assert.True(t, streamChunkHasOutput(`{"choices":[{"index":0,"delta":{"content":"你好"}}]}`))
	assert.True(t, streamChunkHasOutput(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"f","arguments":""}}]}}]}`))
	assert.True(t, streamChunkHasOutput(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
}

func TestStreamChunkUsageReadsReportedUsageAnywhereInStream(t *testing.T) {
	assert.Nil(t, streamChunkUsage(`{"choices":[{"delta":{"content":"x"}}]}`))
	assert.Nil(t, streamChunkUsage(`{"choices":[{"delta":{"content":"x"}}],"usage":null}`))
	usage := streamChunkUsage(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	if assert.NotNil(t, usage) {
		assert.Equal(t, 3, usage.CompletionTokens)
	}
}
