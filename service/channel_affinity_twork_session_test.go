package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTworkSessionAffinityContext(path, model, session string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(fmt.Sprintf(`{"model":"%s"}`, model)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Twork-Session", session)
	return c
}

// 兜底规则：没有专门规则的模型（DeepSeek、通义、Auto 等）也按会话粘在同一渠道，各协议一致。
func TestTworkSessionAffinityKeepsSessionOnSameChannelForEveryProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	session := fmt.Sprintf("session-%d", time.Now().UnixNano())
	for i, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		model := fmt.Sprintf("deepseek-v4-flash-%d", i)
		first := newTworkSessionAffinityContext(path, model, session)
		_, found := GetPreferredChannelByAffinity(first, model, "default")
		require.False(t, found, path)
		RecordChannelAffinity(first, 8800+i)
		t.Cleanup(func() { ClearCurrentChannelAffinityCache(first) })

		again := newTworkSessionAffinityContext(path, model, session)
		channelID, found := GetPreferredChannelByAffinity(again, model, "default")
		assert.True(t, found, path)
		assert.Equal(t, 8800+i, channelID, path)

		// 同一会话换了模型、或者换了会话，都重新按优先级挑。
		_, found = GetPreferredChannelByAffinity(newTworkSessionAffinityContext(path, model+"-other", session), model+"-other", "default")
		assert.False(t, found, path)
		_, found = GetPreferredChannelByAffinity(newTworkSessionAffinityContext(path, model, session+"-other"), model, "default")
		assert.False(t, found, path)
	}
}
