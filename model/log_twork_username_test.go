package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetAllLogsAddsCurrentTworkUsernameFromSoftDeletedToken(t *testing.T) {
	truncateTables(t)
	token := &Token{
		Id:            37,
		UserId:        1,
		Key:           "token-37",
		Name:          "twork_37",
		TworkUsername: "alice-current",
	}
	require.NoError(t, DB.Create(token).Error)
	require.NoError(t, DB.Delete(token).Error)
	require.NoError(t, LOG_DB.Create(&Log{
		UserId:    1,
		Type:      LogTypeConsume,
		TokenId:   token.Id,
		TokenName: token.Name,
	}).Error)

	logs, total, err := GetAllLogs(
		LogTypeUnknown, 0, 0, "", "", "", 0, 20, 0, "", "", "",
	)

	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, logs, 1)
	require.Equal(t, "alice-current", logs[0].TworkUsername)
}

func TestGetAllLogsKeepsWorkingWhenTworkUsernameLookupFails(t *testing.T) {
	truncateTables(t)
	require.NoError(t, LOG_DB.Create(&Log{
		UserId:    1,
		Type:      LogTypeConsume,
		TokenId:   999,
		TokenName: "twork_999",
	}).Error)

	originalDB := DB
	brokenDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	DB = brokenDB
	t.Cleanup(func() { DB = originalDB })

	logs, total, err := GetAllLogs(
		LogTypeUnknown, 0, 0, "", "", "", 0, 20, 0, "", "", "",
	)

	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, logs, 1)
	require.Empty(t, logs[0].TworkUsername)
}
