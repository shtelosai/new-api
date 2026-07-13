package middleware

import (
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const claudeCodeSessionHeader = "X-Claude-Code-Session-Id"

var claudeCodeSessionSuffixPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

func captureConversationHash(c *gin.Context, requestBody []byte) {
	sessionID := ""
	if c != nil && c.Request != nil {
		sessionID = strings.TrimSpace(c.Request.Header.Get(claudeCodeSessionHeader))
	}
	if sessionID == "" {
		sessionID = extractClaudeCodeSessionID(requestBody)
	}
	if sessionID == "" {
		return
	}

	hash := common.GenerateHMAC("new-api:conversation:v1:" + sessionID)
	if len(hash) < 32 {
		return
	}
	common.SetContextKey(c, constant.ContextKeyConversationHash, hash[:32])
}

func extractClaudeCodeSessionID(requestBody []byte) string {
	userID := strings.TrimSpace(gjson.GetBytes(requestBody, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if matches := claudeCodeSessionSuffixPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return matches[1]
	}
	if strings.HasPrefix(userID, "{") {
		return strings.TrimSpace(gjson.Get(userID, "session_id").String())
	}
	return ""
}
