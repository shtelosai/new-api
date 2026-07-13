package model

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func ensureConversationHashTestColumn(t *testing.T) {
	t.Helper()
	if LOG_DB.Migrator().HasColumn("logs", "conversation_hash") {
		return
	}
	require.NoError(t, LOG_DB.Exec("ALTER TABLE logs ADD COLUMN conversation_hash varchar(32) DEFAULT ''").Error)
}

func newConversationHashLogContext(hash string) *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Set("username", "tester")
	ctx.Set("conversation_hash", hash)
	return ctx
}

func TestRecordConsumeLogPersistsConversationHash(t *testing.T) {
	truncateTables(t)
	ensureConversationHashTestColumn(t)
	ctx := newConversationHashLogContext("0123456789abcdef0123456789abcdef")

	RecordConsumeLog(ctx, 1, RecordConsumeLogParams{
		ChannelId: 128,
		ModelName: "auto",
		TokenName: "twork_1",
	})

	var row struct {
		ConversationHash string `gorm:"column:conversation_hash"`
	}
	require.NoError(t, LOG_DB.Table("logs").Select("conversation_hash").Take(&row).Error)
	require.Equal(t, "0123456789abcdef0123456789abcdef", row.ConversationHash)
}

func TestRecordErrorLogPersistsConversationHash(t *testing.T) {
	truncateTables(t)
	ensureConversationHashTestColumn(t)
	ctx := newConversationHashLogContext("fedcba9876543210fedcba9876543210")

	RecordErrorLog(ctx, 1, 128, "auto", "twork_1", "upstream failed", 1, 1, true, "auto", nil)

	var row struct {
		ConversationHash string `gorm:"column:conversation_hash"`
	}
	require.NoError(t, LOG_DB.Table("logs").Select("conversation_hash").Take(&row).Error)
	require.Equal(t, "fedcba9876543210fedcba9876543210", row.ConversationHash)
}

func TestFormatUserLogsHidesConversationHash(t *testing.T) {
	log := &Log{}
	field := reflect.ValueOf(log).Elem().FieldByName("ConversationHash")
	require.True(t, field.IsValid(), "Log 应提供 ConversationHash 字段")
	field.SetString("0123456789abcdef0123456789abcdef")

	formatUserLogs([]*Log{log}, 0)

	require.Empty(t, field.String())
}
