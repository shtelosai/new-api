package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"image"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func imageJobFixture(t *testing.T) (*Token, *Token) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&ImageJob{}, &ImageJobProjection{}, &ImageJobLogReceipt{}))
	for _, table := range []string{"image_jobs", "image_job_projections", "image_job_log_receipts", "tokens", "users", "channels", "abilities", "token_model_channels", "channel_model_disabled", "logs"} {
		require.NoError(t, DB.Exec("DELETE FROM "+table).Error)
	}
	u := User{Id: 9123, Username: "image-test", Status: common.UserStatusEnabled, Group: "default", Quota: 3000000}
	require.NoError(t, DB.Create(&u).Error)
	a := &Token{Id: 9124, UserId: u.Id, Key: "image-token-a", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000, Group: "default"}
	b := &Token{Id: 9125, UserId: u.Id, Key: "image-token-b", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000, Group: "default"}
	require.NoError(t, DB.Create(a).Error)
	require.NoError(t, DB.Create(b).Error)
	for i, family := range []string{"flare", "sunburst"} {
		modelName := "twork-image-" + family + "-async"
		c := Channel{Id: 9126 + i, Status: common.ChannelStatusEnabled, Models: modelName, Group: "default", Key: "adapter-key", BaseURL: common.GetPointer("http://127.0.0.1:9999/" + family), Setting: common.GetPointer(`{"twork_runtime":"image_async","twork_image_provider":"kie","twork_image_family":"` + family + `"}`)}
		require.NoError(t, DB.Create(&c).Error)
		require.NoError(t, DB.Create(&Ability{Group: "default", Model: modelName, ChannelId: c.Id, Enabled: true, Priority: common.GetPointer(int64(200))}).Error)
		require.NoError(t, DB.Create(&TokenModelChannel{TokenId: a.Id, ModelId: modelName, ChannelId: c.Id}).Error)
	}
	return a, b
}

func TestImageJobViewKeepsLegacyFamilySlotAndReportsActualSunburst(t *testing.T) {
	view := (ImageJob{Resolution: "1k", ModelFamily: "sunburst"}).View()
	assert.Equal(t, "flare", view.ModelFamily)
	assert.Equal(t, "sunburst", view.UpstreamModelFamily)
	view = (ImageJob{Resolution: "4k", ModelFamily: "flare"}).View()
	assert.Equal(t, "sunburst", view.ModelFamily)
	assert.Equal(t, "flare", view.UpstreamModelFamily)
}

func TestImageJobReserveReplayConflictAndExactTokenOwnership(t *testing.T) {
	a, b := imageJobFixture(t)
	r := ImageJobRequest{ClientRequestID: "request-1", ClientSessionID: "session-1", Prompt: "一只蓝色杯子"}
	require.NoError(t, r.Normalize())
	j, created, err := CreateImageJob(context.Background(), a.Id, r)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, 150000, j.ReservedQuota)
	assert.Equal(t, 30, j.PriceCents)
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 850000, a.RemainQuota)
	assert.Zero(t, a.UsedQuota)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", a.Id).Update("remain_quota", 0).Error)
	replay, created, err := CreateImageJob(context.Background(), a.Id, r)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, j.ID, replay.ID)
	r.Prompt = "另一个请求"
	_, _, err = CreateImageJob(context.Background(), a.Id, r)
	assert.ErrorIs(t, err, ErrImageJobConflict)
	_, err = GetImageJob(context.Background(), b.Id, j.ID)
	assert.ErrorIs(t, err, ErrImageJobNotFound)
	_, _, err = CreateImageJob(context.Background(), b.Id, r)
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
}

func TestImageJobSettlementIsAtomicIdempotentAndPricedByResolution(t *testing.T) {
	for _, tc := range []struct {
		resolution   string
		quota, cents int
	}{{"1k", 150000, 30}, {"2k", 200000, 40}, {"4k", 400000, 80}} {
		t.Run(tc.resolution, func(t *testing.T) {
			a, _ := imageJobFixture(t)
			r := ImageJobRequest{ClientRequestID: "price", ClientSessionID: "s", Prompt: "杯子", Resolution: tc.resolution}
			require.NoError(t, r.Normalize())
			j, _, err := CreateImageJob(context.Background(), a.Id, r)
			require.NoError(t, err)
			require.NoError(t, DB.Model(&ImageJob{}).Where("id = ?", j.ID).Updates(map[string]any{"status": "settling", "channel_id": 9126, "result_hash": "digest", "actual_size": "1024x1024", "output_format": "png", "bytes": 123}).Error)
			require.NoError(t, SettleImageJob(context.Background(), j.ID, true, ""))
			require.NoError(t, SettleImageJob(context.Background(), j.ID, true, ""))
			require.NoError(t, SettleImageJob(context.Background(), j.ID, false, "late_failure"))
			j, err = GetImageJob(context.Background(), a.Id, j.ID)
			require.NoError(t, err)
			assert.Equal(t, "succeeded", j.Status)
			assert.Equal(t, tc.quota, j.ChargedQuota)
			assert.Zero(t, j.ReservedQuota)
			assert.Equal(t, tc.cents, j.PriceCents)
			require.NoError(t, DB.First(a, a.Id).Error)
			assert.Equal(t, 1000000-tc.quota, a.RemainQuota)
			assert.Equal(t, tc.quota, a.UsedQuota)
			var u User
			require.NoError(t, DB.First(&u, a.UserId).Error)
			assert.Equal(t, 3000000-tc.quota, u.Quota)
			assert.Equal(t, tc.quota, u.UsedQuota)
			assert.Equal(t, 1, u.RequestCount)
			require.NoError(t, ProjectImageJobLogs(context.Background()))
			require.NoError(t, ProjectImageJobLogs(context.Background()))
			var logs []Log
			require.NoError(t, LOG_DB.Where("request_id = ?", "imagejob_"+j.ID).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, tc.quota, logs[0].Quota)
		})
	}
}

func TestImageJobFailedSettlementReleasesOnceAndLeaseHasSingleOwner(t *testing.T) {
	a, _ := imageJobFixture(t)
	r := ImageJobRequest{ClientRequestID: "fail", ClientSessionID: "s", Prompt: "杯子"}
	require.NoError(t, r.Normalize())
	j, _, err := CreateImageJob(context.Background(), a.Id, r)
	require.NoError(t, err)
	now := time.Now().Unix()
	var owners int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			claimed, e := ClaimImageJob(context.Background(), owner, now, 60)
			require.NoError(t, e)
			if claimed != nil {
				mu.Lock()
				owners++
				mu.Unlock()
			}
		}(owner)
	}
	wg.Wait()
	assert.Equal(t, 1, owners)
	revived, err := ClaimImageJob(context.Background(), "worker-restarted", now+61, 60)
	require.NoError(t, err)
	require.NotNil(t, revived)
	assert.Equal(t, j.ID, revived.ID)
	require.NoError(t, SettleImageJob(context.Background(), j.ID, false, "provider_rejected"))
	require.NoError(t, SettleImageJob(context.Background(), j.ID, false, "provider_rejected"))
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 1000000, a.RemainQuota)
	assert.Zero(t, a.UsedQuota)
	var u User
	require.NoError(t, DB.First(&u, a.UserId).Error)
	assert.Equal(t, 3000000, u.Quota)
}

func TestImageJobRuntimeNeverEntersLegacySelection(t *testing.T) {
	a, _ := imageJobFixture(t)
	channels, err := ImageJobChannels(context.Background(), a.Id, ImageJobRequest{Resolution: "1k", Size: "1:1", Background: "opaque"})
	require.NoError(t, err)
	require.Len(t, channels, 2)
	for _, channel := range channels {
		assert.False(t, channel.AllowsLegacyRuntime())
	}
	ids, err := legacyChannelIDs("default", "twork-image-flare-async")
	require.NoError(t, err)
	assert.Empty(t, ids)
	for _, channel := range channels {
		require.NoError(t, DB.Create(&ChannelModelDisabled{ChannelId: channel.Id, Model: channel.Models, Source: "manual"}).Error)
	}
	channels, err = ImageJobChannels(context.Background(), a.Id, ImageJobRequest{Resolution: "1k", Size: "1:1", Background: "opaque"})
	require.NoError(t, err)
	assert.Empty(t, channels)
}

func TestImageJobReservationRollsBackBothBalancesWhenInsertFails(t *testing.T) {
	a, _ := imageJobFixture(t)
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register("reject_image_job", func(tx *gorm.DB) {
		if tx.Statement.Table == "image_jobs" {
			tx.AddError(errors.New("disk full"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, DB.Callback().Create().Remove("reject_image_job")) })
	_, _, err := CreateImageJob(context.Background(), a.Id, ImageJobRequest{ClientRequestID: "rollback", ClientSessionID: "s", Prompt: "杯子"})
	require.Error(t, err)
	require.NoError(t, DB.First(a, a.Id).Error)
	assert.Equal(t, 1000000, a.RemainQuota)
	var user User
	require.NoError(t, DB.First(&user, a.UserId).Error)
	assert.Equal(t, 3000000, user.Quota)
	var count int64
	require.NoError(t, DB.Model(&ImageJob{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestImageJobLeaseRejectsStaleSettlementAndCapabilityFiltering(t *testing.T) {
	a, _ := imageJobFixture(t)
	request := ImageJobRequest{ClientRequestID: "lease", ClientSessionID: "s", Prompt: "杯子"}
	require.NoError(t, request.Normalize())
	job, _, err := CreateImageJob(context.Background(), a.Id, request)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&ImageJob{}).Where("id = ?", job.ID).Updates(map[string]any{"lease_owner": "current-worker", "lease_until": time.Now().Unix() + 60}).Error)
	assert.ErrorIs(t, SettleImageJob(context.Background(), job.ID, false, "failure", "stale-worker"), ErrImageJobLease)
	current, err := GetImageJob(context.Background(), a.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, 150000, current.ReservedQuota)
	for _, r := range []ImageJobRequest{{Resolution: "1k", Size: "1:1", OutputFormat: "webp"}, {Resolution: "1k", Size: "5:4"}, {Resolution: "1k", Size: "1:1", MaskURL: "https://example.test/mask.png"}, {Resolution: "1k", Size: "1:1", Quality: "high"}} {
		channels, err := ImageJobChannels(context.Background(), a.Id, r)
		require.NoError(t, err)
		assert.Empty(t, channels)
	}
	channels, err := ImageJobChannels(context.Background(), a.Id, ImageJobRequest{Resolution: "1k", Size: "1:1", Background: "transparent"})
	require.NoError(t, err)
	require.Len(t, channels, 2)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", a.Id).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "another-model"}).Error)
	channels, err = ImageJobChannels(context.Background(), a.Id, request)
	require.NoError(t, err)
	assert.Empty(t, channels)
}

func TestImageJobRuntimeRejectsAmbiguousProviderAndFamilySettings(t *testing.T) {
	for _, settings := range []string{`{"twork_runtime":"image_async","twork_image_provider":"kie","twork_image_provider":"apimart","twork_image_family":"flare"}`, `{"twork_runtime":"image_async","Twork_Image_Provider":"kie","twork_image_family":"flare"}`, `{"twork_runtime":"image_async","twork_image_provider":"Kie","twork_image_family":"flare"}`, `{"twork_runtime":"image_async","twork_image_provider":"kie","twork_image_family":null}`, `{"twork_runtime":"image_async","twork_image_provider":"kie","twork_image_family":"sunburst","twork_wire_api":"responses"}`} {
		ch := Channel{Setting: &settings}
		_, err := ch.TworkRuntime()
		assert.Error(t, err)
		assert.False(t, ch.AllowsLegacyRuntime())
	}
}

func TestImageJobListReturnsNewestSessionTasksWithOwnerScopedCursor(t *testing.T) {
	a, b := imageJobFixture(t)
	for i := 0; i < 3; i++ {
		require.NoError(t, DB.Create(&ImageJob{ID: ImageJobID(a.Id, fmt.Sprint(i)), TokenID: a.Id, ClientRequestID: fmt.Sprint(i), ClientSessionID: "s", Status: "failed", CreatedAt: int64(100 + i)}).Error)
	}
	require.NoError(t, DB.Create(&ImageJob{ID: ImageJobID(a.Id, "other"), TokenID: a.Id, ClientRequestID: "other", ClientSessionID: "different", Status: "failed", CreatedAt: 200}).Error)
	jobs, err := ListImageJobs(context.Background(), a.Id, "s", "")
	require.NoError(t, err)
	require.Len(t, jobs, 3)
	assert.Equal(t, int64(102), jobs[0].CreatedAt)
	next, err := ListImageJobs(context.Background(), a.Id, "s", jobs[0].ID)
	require.NoError(t, err)
	require.Len(t, next, 2)
	assert.Equal(t, int64(101), next[0].CreatedAt)
	_, err = ListImageJobs(context.Background(), b.Id, "s", jobs[0].ID)
	assert.ErrorIs(t, err, ErrImageJobNotFound)
}

func TestImageJobReferenceDecodedLimitRejectsOneExtraByte(t *testing.T) {
	var encodedImage bytes.Buffer
	require.NoError(t, png.Encode(&encodedImage, image.NewNRGBA(image.Rect(0, 0, 64, 64))))
	for _, tc := range []struct {
		name  string
		limit int
		mask  bool
	}{{"reference", 20 << 20, false}, {"mask", 4 << 20, true}} {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.limit+1)
			copy(data, encodedImage.Bytes())
			// 这些边界的 +1 字节仍有相同的 Base64 EncodedLen，必须检查真实解码长度。
			assert.Equal(t, base64.StdEncoding.EncodedLen(tc.limit), base64.StdEncoding.EncodedLen(tc.limit+1))
			for _, extra := range []int{0, 1} {
				input := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data[:tc.limit+extra])
				r := ImageJobRequest{ClientRequestID: "limit", ClientSessionID: "s", Prompt: "杯子"}
				if tc.mask {
					r.ImageURLs = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(encodedImage.Bytes())}
					r.MaskURL = input
				} else {
					r.ImageURLs = []string{input}
				}
				err := r.Normalize()
				if extra == 0 {
					require.NoError(t, err)
				} else {
					assert.EqualError(t, err, "参考图 Base64 无效或超过大小限制")
				}
			}
		})
	}
}

func TestImageJobLeasePicksUnpolledTasksBeforeOlderRunningTasks(t *testing.T) {
	a, _ := imageJobFixture(t)
	now := time.Now().Unix()
	oldest := ImageJob{ID: ImageJobID(a.Id, "old"), TokenID: a.Id, ClientRequestID: "old", ClientSessionID: "s", Status: "running", CreatedAt: now - 100, NextPollAt: now}
	newest := ImageJob{ID: ImageJobID(a.Id, "new"), TokenID: a.Id, ClientRequestID: "new", ClientSessionID: "s", Status: "queued", CreatedAt: now - 1, NextPollAt: 0}
	require.NoError(t, DB.Create(&oldest).Error)
	require.NoError(t, DB.Create(&newest).Error)
	claimed, err := ClaimImageJob(context.Background(), "fair-worker", now, 60)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, newest.ID, claimed.ID)
}

func TestImageJobSunburstPreferredAtEveryResolutionWithAuthorizedFlareFallback(t *testing.T) {
	for _, resolution := range []string{"1k", "2k", "4k"} {
		t.Run(resolution, func(t *testing.T) {
			token, _ := imageJobFixture(t)
			request := ImageJobRequest{ClientRequestID: "sunburst-policy", ClientSessionID: "s", Prompt: "杯子", Resolution: resolution, OutputFormat: "png"}
			require.NoError(t, request.Normalize())
			channels, err := ImageJobChannels(context.Background(), token.Id, request)
			require.NoError(t, err)
			require.Len(t, channels, 2)
			assert.Equal(t, 9127, channels[0].Id)
			assert.Equal(t, 9126, channels[1].Id)
			require.NoError(t, DB.Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "twork-image-flare-async"}).Error)
			channels, err = ImageJobChannels(context.Background(), token.Id, request)
			require.NoError(t, err)
			require.Len(t, channels, 1)
			assert.Equal(t, 9126, channels[0].Id)
			require.NoError(t, DB.Where("token_id = ? AND channel_id = ?", token.Id, 9126).Delete(&TokenModelChannel{}).Error)
			channels, err = ImageJobChannels(context.Background(), token.Id, request)
			require.NoError(t, err)
			assert.Empty(t, channels)
		})
	}
}
