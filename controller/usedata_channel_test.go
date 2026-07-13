package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type channelConsumptionResponse struct {
	Success bool                           `json:"success"`
	Message string                         `json:"message"`
	Data    []model.ChannelConsumptionData `json:"data"`
}

func setupChannelConsumptionControllerTestDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.QuotaData{}))
	require.NoError(t, db.Create(&model.Channel{Id: 1, Name: "east"}).Error)
	require.NoError(t, db.Create(&[]model.QuotaData{
		{ChannelID: 1, ModelName: "gpt-a", CreatedAt: 1100, Quota: 100},
		{ChannelID: 1, ModelName: "gpt-b", CreatedAt: 1200, Quota: 50},
	}).Error)
}

func TestGetChannelConsumptionReturnsAggregatedData(t *testing.T) {
	setupChannelConsumptionControllerTestDB(t)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/channels?start_timestamp=1000&end_timestamp=2000", nil)

	GetChannelConsumption(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload channelConsumptionResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success, payload.Message)
	require.Equal(t, []model.ChannelConsumptionData{
		{ChannelID: 1, ChannelName: "east", Quota: 150},
	}, payload.Data)
}

func TestGetChannelConsumptionRejectsInvalidTimeRanges(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		message string
	}{
		{name: "missing start", query: "end_timestamp=2000", message: "invalid start_timestamp"},
		{name: "non-positive start", query: "start_timestamp=0&end_timestamp=2000", message: "invalid start_timestamp"},
		{name: "invalid end", query: "start_timestamp=1000&end_timestamp=bad", message: "invalid end_timestamp"},
		{name: "reversed range", query: "start_timestamp=2000&end_timestamp=1000", message: "invalid time range"},
		{name: "over 31 days", query: "start_timestamp=1&end_timestamp=2678402", message: "time range cannot exceed 31 days"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/channels?"+test.query, nil)

			GetChannelConsumption(ctx)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload channelConsumptionResponse
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			require.False(t, payload.Success)
			require.Equal(t, test.message, payload.Message)
		})
	}
}

func TestGetChannelConsumptionReturnsDatabaseErrors(t *testing.T) {
	setupChannelConsumptionControllerTestDB(t)
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/channels?start_timestamp=1000&end_timestamp=2000", nil)

	GetChannelConsumption(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload channelConsumptionResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.False(t, payload.Success)
	require.NotEmpty(t, payload.Message)
}
