package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
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
		TworkOrgName:  "研发中心",
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
	assert.Equal(t, "alice-current", logs[0].TworkUsername)
	assert.Equal(t, "研发中心", logs[0].TworkOrgName)
}

func TestGetAllLogsKeepsEmptyTworkUsernameAndOrgName(t *testing.T) {
	truncateTables(t)
	token := &Token{
		Id:     38,
		UserId: 1,
		Key:    "token-38",
		Name:   "twork_38",
	}
	require.NoError(t, DB.Create(token).Error)
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
	assert.Empty(t, logs[0].TworkUsername)
	assert.Empty(t, logs[0].TworkOrgName)
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
	assert.Empty(t, logs[0].TworkUsername)
	assert.Empty(t, logs[0].TworkOrgName)
}
