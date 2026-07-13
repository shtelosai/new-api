package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func parseConversationHash(t *testing.T, body string, sessionHeader string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if sessionHeader != "" {
		ctx.Request.Header.Set("X-Claude-Code-Session-Id", sessionHeader)
	}

	_, err := getModelFromJSONBody(ctx)
	require.NoError(t, err)
	return ctx.GetString("conversation_hash")
}

func TestGetModelFromJSONBodyCapturesStableConversationHash(t *testing.T) {
	first := parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"{\"device_id\":\"device-a\",\"session_id\":\"session-a\"}"}}`, "")
	second := parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"{\"device_id\":\"device-b\",\"session_id\":\"session-a\"}"}}`, "")
	different := parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"{\"session_id\":\"session-b\"}"}}`, "")

	require.Len(t, first, 32)
	require.Equal(t, first, second)
	require.NotEqual(t, first, different)
}

func TestGetModelFromJSONBodyConversationHashUsesHeaderAndLegacySuffix(t *testing.T) {
	headerOnly := parseConversationHash(t, `{"model":"auto"}`, "header-session")
	headerWins := parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"{\"session_id\":\"payload-session\"}"}}`, "header-session")
	legacy := parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"device_account_session_abc-123"}}`, "")
	legacyEquivalent := parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"{\"session_id\":\"abc-123\"}"}}`, "")

	require.Equal(t, headerOnly, headerWins)
	require.Equal(t, legacyEquivalent, legacy)
}

func TestGetModelFromJSONBodyConversationHashRejectsBareUserID(t *testing.T) {
	require.Empty(t, parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"same-user-across-chats"}}`, ""))
	require.Empty(t, parseConversationHash(t, `{"model":"auto","metadata":{"user_id":"{invalid-json"}}`, ""))
}
