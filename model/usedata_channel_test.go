package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestGetChannelConsumptionDataAggregatesAndSortsChannels(t *testing.T) {
	truncateTables(t)

	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	require.NoError(t, DB.Create(&[]Channel{
		{Id: 1, Name: "shared"},
		{Id: 2, Name: "shared"},
	}).Error)
	require.NoError(t, DB.Create(&[]QuotaData{
		{ChannelID: 1, ModelName: "gpt-a", CreatedAt: 1000, Quota: 100},
		{ChannelID: 1, ModelName: "gpt-b", CreatedAt: 1100, Quota: 50},
		{ChannelID: 2, ModelName: "gpt-c", CreatedAt: 1200, Quota: 150},
		{ChannelID: 3, ModelName: "deleted-channel", CreatedAt: 1300, Quota: 25},
		{ChannelID: 4, ModelName: "zero-total", CreatedAt: 1400, Quota: 0},
		{ChannelID: 0, ModelName: "unassigned", CreatedAt: 1500, Quota: 999},
		{ChannelID: 1, ModelName: "outside-range", CreatedAt: 3000, Quota: 1000},
	}).Error)

	rows, err := GetChannelConsumptionData(900, 2000)

	require.NoError(t, err)
	require.Equal(t, []ChannelConsumptionData{
		{ChannelID: 1, ChannelName: "shared", Quota: 150},
		{ChannelID: 2, ChannelName: "shared", Quota: 150},
		{ChannelID: 3, ChannelName: "channel-3", Quota: 25},
	}, rows)
}

func TestGetChannelConsumptionDataReturnsEmptySlice(t *testing.T) {
	truncateTables(t)

	rows, err := GetChannelConsumptionData(1000, 2000)

	require.NoError(t, err)
	require.Empty(t, rows)
	require.NotNil(t, rows)
}
