package model

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const imageModelID = "gpt-image-2.6-flare"

func imageModelChannelFixture(t *testing.T, id int, modelID string) {
	t.Helper()
	setting := `{"twork_runtime":"image_model","twork_image_model":{"display_name":"GPT Image 2.6 Flare","prices_cents":{"1k":10,"2k":20,"4k":40},"sizes":["1:1","16:9"],"edit":true,"mask":false,"max_references":4,"sort_order":1}}`
	name := imageModelChannelName(modelID)
	c := Channel{Id: id, Status: common.ChannelStatusEnabled, Models: name, Group: "default", Key: "adapter-key", BaseURL: common.GetPointer("http://127.0.0.1:1/supergpt"), Setting: &setting}
	require.NoError(t, DB.Create(&c).Error)
	require.NoError(t, DB.Create(&Ability{Group: "default", Model: name, ChannelId: c.Id, Enabled: true, Priority: common.GetPointer(int64(200))}).Error)
}

func canvasRequest(id string) ImageJobRequest {
	return ImageJobRequest{ClientRequestID: id, ClientSessionID: "canvas", Prompt: "杯子", Size: "16:9", Resolution: "2k", Model: imageModelID}
}

func settleableImageJob(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, DB.Model(&ImageJob{}).Where("id = ?", id).Updates(map[string]any{"status": "settling", "result_hash": "hash", "bytes": 1, "output_format": "png", "actual_size": "64x64", "channel_id": 9200}).Error)
}

func TestLegacyImageJobHashIgnoresEmptyModel(t *testing.T) {
	r := ImageJobRequest{ClientRequestID: "r", ClientSessionID: "s", Prompt: "p", Size: "1:1", Resolution: "1k", Background: "opaque"}
	encoded, err := common.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "model")
	hash, err := r.Hash()
	require.NoError(t, err)
	// 线上任务的持久请求按这个哈希校验，变化会让进行中的对话任务全部失败。
	assert.Equal(t, "283950636c22a22ca65a1bb071980936", hash[:32])
}

func TestLegacyImageJobPriceAndChannelsIgnoreCanvasModels(t *testing.T) {
	a, _ := imageJobFixture(t)
	imageModelChannelFixture(t, 9200, imageModelID)
	channels, err := ImageJobChannels(context.Background(), a.Id, ImageJobRequest{Resolution: "1k", Size: "1:1", Background: "opaque"})
	require.NoError(t, err)
	require.NotEmpty(t, channels)
	for _, channel := range channels {
		assert.NotEqual(t, 9200, channel.Id)
	}
	r := ImageJobRequest{ClientRequestID: "legacy", ClientSessionID: "s", Prompt: "杯子"}
	require.NoError(t, r.Normalize())
	job, _, err := CreateImageJob(context.Background(), a.Id, r)
	require.NoError(t, err)
	assert.Equal(t, 150000, job.ReservedQuota)
	assert.Empty(t, job.ImageModel)
	assert.Equal(t, 150000, job.expectedQuota())
}

func TestCanvasImageJobOnlyUsesImageModelChannels(t *testing.T) {
	a, _ := imageJobFixture(t)
	imageModelChannelFixture(t, 9200, imageModelID)
	channels, err := ImageJobChannels(context.Background(), a.Id, canvasRequest("c"))
	require.NoError(t, err)
	require.Len(t, channels, 1)
	assert.Equal(t, 9200, channels[0].Id)
	r := canvasRequest("c2")
	r.Size = "9:16"
	channels, err = ImageJobChannels(context.Background(), a.Id, r)
	require.NoError(t, err)
	assert.Empty(t, channels, "不支持的比例不能降级到其他渠道")
	r = canvasRequest("c3")
	r.MaskURL, r.ImageURLs = "https://example.com/m.png", []string{"https://example.com/a.png"}
	channels, err = ImageJobChannels(context.Background(), a.Id, r)
	require.NoError(t, err)
	assert.Empty(t, channels)
}

func TestCanvasImageJobChargesConfiguredPriceAndRefundsOnFailure(t *testing.T) {
	a, _ := imageJobFixture(t)
	imageModelChannelFixture(t, 9200, imageModelID)
	ctx := context.Background()
	r := canvasRequest("charged")
	require.NoError(t, r.Normalize())
	job, created, err := CreateImageJob(ctx, a.Id, r)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, 20, job.PriceCents)
	assert.Equal(t, 100000, job.ReservedQuota)
	assert.Equal(t, imageModelID, job.ImageModel)
	settleableImageJob(t, job.ID)
	require.NoError(t, SettleImageJob(ctx, job.ID, true, ""))
	require.NoError(t, DB.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, 100000, job.ChargedQuota)
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 900000, a.RemainQuota)
	assert.Equal(t, 100000, a.UsedQuota)

	failed := canvasRequest("refund")
	require.NoError(t, failed.Normalize())
	job, _, err = CreateImageJob(ctx, a.Id, failed)
	require.NoError(t, err)
	require.NoError(t, SettleImageJob(ctx, job.ID, false, "image_failed"))
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 900000, a.RemainQuota)
}

func TestCanvasImageJobPriceChangeDoesNotBreakReservedJobs(t *testing.T) {
	a, _ := imageJobFixture(t)
	imageModelChannelFixture(t, 9200, imageModelID)
	ctx := context.Background()
	r := canvasRequest("reprice")
	require.NoError(t, r.Normalize())
	job, _, err := CreateImageJob(ctx, a.Id, r)
	require.NoError(t, err)
	setting := `{"twork_runtime":"image_model","twork_image_model":{"display_name":"x","prices_cents":{"2k":99},"sizes":["16:9"]}}`
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 9200).Update("setting", setting).Error)
	settleableImageJob(t, job.ID)
	require.NoError(t, SettleImageJob(ctx, job.ID, true, ""))
	require.NoError(t, DB.First(job, "id = ?", job.ID).Error)
	assert.Equal(t, 100000, job.ChargedQuota)
}

func TestLegacyImageJobListHidesCanvasJobs(t *testing.T) {
	a, _ := imageJobFixture(t)
	imageModelChannelFixture(t, 9200, imageModelID)
	ctx := context.Background()
	legacy := ImageJobRequest{ClientRequestID: "legacy", ClientSessionID: "canvas", Prompt: "杯子"}
	require.NoError(t, legacy.Normalize())
	_, _, err := CreateImageJob(ctx, a.Id, legacy)
	require.NoError(t, err)
	r := canvasRequest("hidden")
	require.NoError(t, r.Normalize())
	_, _, err = CreateImageJob(ctx, a.Id, r)
	require.NoError(t, err)
	jobs, err := ListImageJobs(ctx, a.Id, "", "")
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "legacy", jobs[0].ClientRequestID)
}

func TestListImageModelsReturnsCatalogForAnyToken(t *testing.T) {
	_, b := imageJobFixture(t)
	imageModelChannelFixture(t, 9200, imageModelID)
	imageModelChannelFixture(t, 9201, imageModelID)
	models, err := ListImageModels(context.Background(), b.Id)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, imageModelID, models[0].ID)
	assert.Equal(t, "GPT Image 2.6 Flare", models[0].Name)
	assert.Equal(t, []string{"1k", "2k", "4k"}, models[0].Resolutions)
	assert.Equal(t, 40, models[0].PricesCents["4k"])
}

func TestImageModelConfigRejectsInvalidPrices(t *testing.T) {
	setting := `{"twork_image_model":{"display_name":"x","prices_cents":{"1k":0},"sizes":["1:1"]}}`
	_, err := (&Channel{Setting: &setting}).ImageModelConfig()
	assert.Error(t, err)
	setting = `{"twork_image_model":{"display_name":"x","prices_cents":{"8k":10},"sizes":["1:1"]}}`
	_, err = (&Channel{Setting: &setting}).ImageModelConfig()
	assert.Error(t, err)
}
