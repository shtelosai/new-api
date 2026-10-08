package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageJobServiceFixture(t *testing.T, handler http.HandlerFunc) (*ImageJobService, *model.Token, *httptest.Server) {
	t.Helper()
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "true")
	previousDB, previousLog := model.DB, model.LOG_DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "jobs.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.ImageJob{}, &model.ImageJobProjection{}, &model.ImageJobLogReceipt{}, &model.Token{}, &model.User{}, &model.Channel{}, &model.Ability{}, &model.TokenModelChannel{}, &model.ChannelModelDisabled{}, &model.Log{}))
	t.Cleanup(func() { model.DB = previousDB; model.LOG_DB = previousLog; require.NoError(t, sqlDB.Close()) })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	user := model.User{Id: 1, Username: "test-images", Status: common.UserStatusEnabled, Group: "default", Quota: 5000000}
	require.NoError(t, db.Create(&user).Error)
	token := &model.Token{Id: 2, UserId: 1, Key: "images-key", Status: common.TokenStatusEnabled, ExpiredTime: -1, Group: "default", RemainQuota: 1000000}
	require.NoError(t, db.Create(token).Error)
	for i, provider := range []string{"kie", "apimart"} {
		ch := model.Channel{Id: 3 + i, Status: common.ChannelStatusEnabled, Key: "private-adapter-key", BaseURL: common.GetPointer(server.URL + "/" + provider), Group: "default", Models: "twork-image-flare-async", Priority: common.GetPointer(int64(200 - i*100)), Setting: common.GetPointer(`{"twork_runtime":"image_async","twork_image_provider":"` + provider + `","twork_image_family":"flare"}`)}
		require.NoError(t, db.Create(&ch).Error)
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: ch.Models, ChannelId: ch.Id, Enabled: true}).Error)
		require.NoError(t, db.Create(&model.TokenModelChannel{TokenId: token.Id, ModelId: ch.Models, ChannelId: ch.Id}).Error)
	}
	s, err := NewImageJobService(t.TempDir(), server.Client())
	require.NoError(t, err)
	s.PollInterval = time.Nanosecond
	return s, token, server
}

func imageJobPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, width, height))))
	return b.Bytes()
}
func imageJobJSON(w http.ResponseWriter, status int, v any) {
	b, _ := common.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func TestSunburstFallbackAcrossFourRoutesRetainsOneTaskAndCharge(t *testing.T) {
	var paths, models []string
	var id string
	picture := imageJobPNG(t, 1024, 1024)
	s, token, server := imageJobServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/result") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(picture)
			return
		}
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		if id != "" {
			assert.Equal(t, id, body["job_id"])
		}
		id = body["job_id"].(string)
		paths = append(paths, r.URL.Path)
		models = append(models, body["model"].(string))
		if len(paths) < 4 {
			imageJobJSON(w, 202, map[string]any{"id": id, "status": "failed", "retryable": true, "error_code": "kie_submit_rejected"})
			return
		}
		imageJobJSON(w, 202, map[string]any{"id": id, "status": "succeeded", "provider_task_id": "accepted-once", "image_count": 1})
	})
	for _, table := range []any{&model.TokenModelChannel{}, &model.Ability{}, &model.Channel{}} {
		require.NoError(t, model.DB.Where("1 = 1").Delete(table).Error)
	}
	for i, route := range []string{"kie-sunburst", "apimart-sunburst", "kie-flare", "apimart-flare"} {
		parts := strings.Split(route, "-")
		name := "twork-image-" + parts[1] + "-async"
		channel := model.Channel{Id: i + 3, Status: common.ChannelStatusEnabled, Key: "adapter-key", Models: name, Group: "default", BaseURL: common.GetPointer(server.URL + "/" + route), Priority: common.GetPointer(int64(400 - i*100)), Setting: common.GetPointer(`{"twork_runtime":"image_async","twork_image_provider":"` + parts[0] + `","twork_image_family":"` + parts[1] + `"}`)}
		require.NoError(t, model.DB.Create(&channel).Error)
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: name, ChannelId: channel.Id, Enabled: true}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: token.Id, ModelId: name, ChannelId: channel.Id}).Error)
	}
	job, _, err := s.Create(context.Background(), token.Id, model.ImageJobRequest{ClientRequestID: "four-routes", ClientSessionID: "s", Prompt: "杯子", OutputFormat: "png"})
	require.NoError(t, err)
	for range 5 {
		require.NoError(t, s.RunOnce(context.Background()))
	}
	assert.Equal(t, []string{"/kie-sunburst/v1/image-tasks", "/apimart-sunburst/v1/image-tasks", "/kie-flare/v1/image-tasks", "/apimart-flare/v1/image-tasks"}, paths)
	assert.Equal(t, []string{"gpt-image-2.5-sunburst", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare", "gpt-image-2.5-flare"}, models)
	current, err := model.GetImageJob(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", current.Status)
	assert.Equal(t, 150000, current.ChargedQuota)
	var count int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", "imagejob_"+job.ID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestImageJobWorkerRestartRetrievesSameTaskAndChargesExactlyOnce(t *testing.T) {
	var posts atomic.Int32
	var jobID string
	picture := imageJobPNG(t, 1024, 1024)
	s, token, _ := imageJobServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer private-adapter-key", r.Header.Get("Authorization"))
		if r.Method == "POST" {
			posts.Add(1)
			var b map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &b))
			jobID = b["job_id"].(string)
			assert.Equal(t, "gpt-image-2.5-flare", b["model"])
			assert.Equal(t, "1k", b["resolution"])
			_, exists := b["output_format"]
			assert.False(t, exists)
			imageJobJSON(w, 202, map[string]any{"id": jobID, "status": "running", "provider_task_id": "upstream-1"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/result") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(picture)
			return
		}
		imageJobJSON(w, 200, map[string]any{"id": jobID, "status": "succeeded", "provider_task_id": "upstream-1", "image_count": 1, "actual_size": "1024x1024", "output_format": "png"})
	})
	request := model.ImageJobRequest{ClientRequestID: "restart", ClientSessionID: "session", Prompt: "蓝杯子"}
	job, _, err := s.Create(context.Background(), token.Id, request)
	require.NoError(t, err)
	require.NoError(t, s.RunOnce(context.Background()))
	current, err := model.GetImageJob(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", current.Status)
	// 重建服务对象模拟进程丢失全部内存，仅使用主库和持久卷恢复。
	resumed, err := NewImageJobService(s.DataDir, s.Client)
	require.NoError(t, err)
	resumed.PollInterval = time.Nanosecond
	require.NoError(t, resumed.RunOnce(context.Background()))
	require.NoError(t, resumed.RunOnce(context.Background()))
	current, err = model.GetImageJob(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", current.Status)
	assert.Equal(t, 150000, current.ChargedQuota)
	assert.Equal(t, int32(1), posts.Load())
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("remain_quota", 0).Error)
	replay, created, err := resumed.Create(context.Background(), token.Id, request)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, job.ID, replay.ID)
	b, mime, err := resumed.Result(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, picture, b)
	assert.Equal(t, "image/png", mime)
	_, _, err = resumed.Result(context.Background(), 999, job.ID)
	assert.ErrorIs(t, err, model.ErrImageJobNotFound)
	var count int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", "imagejob_"+job.ID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestImageJobFallbackRequiresExplicitNotAccepted(t *testing.T) {
	for _, tc := range []struct {
		name          string
		retryable     bool
		status        string
		expectedPosts int
	}{{"明确拒绝", true, "failed", 2}, {"受理未知", false, "unknown", 1}, {"受理后失败", true, "failed", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			var id string
			s, token, _ := imageJobServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts.Add(1)
					var b map[string]any
					require.NoError(t, common.DecodeJson(r.Body, &b))
					id = b["job_id"].(string)
				}
				providerID := ""
				if tc.name == "受理后失败" {
					providerID = "already-accepted"
				}
				result := map[string]any{"id": id, "status": tc.status, "retryable": tc.retryable, "error_code": "kie_submit_rejected", "provider_task_id": providerID}
				if strings.HasPrefix(r.URL.Path, "/apimart/") {
					result["status"] = "running"
					result["provider_task_id"] = "apimart-accepted"
				}
				code := 200
				if r.Method == "POST" {
					code = 202
				}
				imageJobJSON(w, code, result)
			})
			job, _, err := s.Create(context.Background(), token.Id, model.ImageJobRequest{ClientRequestID: "fallback", ClientSessionID: "s", Prompt: "杯子"})
			require.NoError(t, err)
			require.NoError(t, s.RunOnce(context.Background()))
			require.NoError(t, s.RunOnce(context.Background()))
			assert.Equal(t, int32(tc.expectedPosts), posts.Load())
			current, err := model.GetImageJob(context.Background(), token.Id, job.ID)
			require.NoError(t, err)
			if tc.name == "明确拒绝" {
				assert.Equal(t, 4, current.ChannelID)
				assert.Equal(t, "running", current.Status)
			} else if tc.name == "受理未知" {
				assert.Equal(t, "unknown", current.Status)
				assert.Equal(t, 150000, current.ReservedQuota)
			} else {
				assert.Equal(t, "failed", current.Status)
				assert.Zero(t, current.ReservedQuota)
			}
		})
	}
}

func TestImageJobResolutionValidationUsesNativeTableAndFullDecode(t *testing.T) {
	r := model.ImageJobRequest{Size: "1:1", Resolution: "4k", Background: "opaque"}
	actual, format, err := ValidateImageJobResult(imageJobPNG(t, 2880, 2880), r, "apimart")
	require.NoError(t, err)
	assert.Equal(t, "2880x2880", actual)
	assert.Equal(t, "png", format)
	_, _, err = ValidateImageJobResult(imageJobPNG(t, 2048, 2048), r, "apimart")
	assert.EqualError(t, err, "image_resolution_mismatch")
	b := imageJobPNG(t, 1024, 1024)
	_, _, err = ValidateImageJobResult(b[:40], model.ImageJobRequest{Size: "1:1", Resolution: "1k"}, "kie")
	assert.EqualError(t, err, "image_result_invalid")
	actual, _, err = ValidateImageJobResult(imageJobPNG(t, 1254, 1254), model.ImageJobRequest{Size: "1:1", Resolution: "1k"}, "kie")
	require.NoError(t, err)
	assert.Equal(t, "1254x1254", actual)
}

func TestImageJobLogProjectionRetriesAcrossDatabasesWithoutDoubleLog(t *testing.T) {
	s, token, _ := imageJobServiceFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	job, _, err := s.Create(context.Background(), token.Id, model.ImageJobRequest{ClientRequestID: "log-retry", ClientSessionID: "s", Prompt: "杯子"})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.ImageJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "settling", "channel_id": 3, "result_hash": "h", "bytes": 1, "actual_size": "1024x1024", "output_format": "png"}).Error)
	require.NoError(t, model.SettleImageJob(context.Background(), job.ID, true, ""))
	logDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "logs.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&model.Log{}, &model.ImageJobLogReceipt{}))
	previous := model.LOG_DB
	model.LOG_DB = logDB
	t.Cleanup(func() { model.LOG_DB = previous; db, _ := logDB.DB(); db.Close() })
	require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register("fail_image_log", func(tx *gorm.DB) {
		if tx.Statement.Table == "logs" {
			tx.AddError(errors.New("log storage unavailable"))
		}
	}))
	require.Error(t, model.ProjectImageJobLogs(context.Background()))
	var receipts int64
	require.NoError(t, logDB.Model(&model.ImageJobLogReceipt{}).Count(&receipts).Error)
	assert.Zero(t, receipts)
	require.NoError(t, logDB.Callback().Create().Remove("fail_image_log"))
	require.NoError(t, model.ProjectImageJobLogs(context.Background()))
	// 模拟跨库日志已提交，但主库 outbox 标记未落盘后重启。
	require.NoError(t, model.DB.Model(&model.ImageJobProjection{}).Where("job_id = ?", job.ID).Update("completed_at", 0).Error)
	require.NoError(t, model.ProjectImageJobLogs(context.Background()))
	var logs []model.Log
	require.NoError(t, logDB.Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 150000, logs[0].Quota)
}

func TestImageJobKieRejectsDownscaledOrWrongRatioWithoutAssumingExactPixels(t *testing.T) {
	for _, tc := range []struct {
		resolution, size string
		width, height    int
		valid            bool
	}{{"1k", "1:1", 1024, 1024, true}, {"1k", "1:1", 1254, 1254, true}, {"1k", "1:1", 512, 512, false}, {"2k", "1:1", 1024, 1024, false}, {"2k", "1:1", 2048, 2048, true}, {"4k", "1:1", 2048, 2048, false}, {"4k", "1:1", 2880, 2880, true}, {"1k", "9:16", 1024, 1024, false}} {
		_, _, err := ValidateImageJobResult(imageJobPNG(t, tc.width, tc.height), model.ImageJobRequest{Resolution: tc.resolution, Size: tc.size}, "kie")
		if tc.valid {
			assert.NoError(t, err)
		} else {
			assert.EqualError(t, err, "image_resolution_mismatch")
		}
	}
}

func TestImageJobSettlingWaitsForDurableResultAndExpirationNeverRegenerates(t *testing.T) {
	s, token, _ := imageJobServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("恢复结算和过期领取不应联系上游")
		w.WriteHeader(500)
	})
	request := model.ImageJobRequest{ClientRequestID: "durable", ClientSessionID: "s", Prompt: "杯子"}
	job, _, err := s.Create(context.Background(), token.Id, request)
	require.NoError(t, err)
	picture := imageJobPNG(t, 1024, 1024)
	hash := fmt.Sprintf("%x", sha256.Sum256(picture))
	require.NoError(t, model.DB.Model(&model.ImageJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": "settling", "channel_id": 3, "result_hash": hash, "bytes": len(picture), "actual_size": "1024x1024", "output_format": "png"}).Error)
	require.Error(t, s.RunOnce(context.Background()))
	current, err := model.GetImageJob(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Zero(t, current.ChargedQuota)
	assert.Equal(t, 150000, current.ReservedQuota)
	require.NoError(t, s.writeImmutable("results", job.ID, picture))
	require.NoError(t, s.RunOnce(context.Background()))
	current, err = model.GetImageJob(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", current.Status)
	require.NoError(t, model.DB.Model(&model.ImageJob{}).Where("id = ?", job.ID).Update("expires_at", time.Now().Unix()-1).Error)
	s.lastCleanup.Store(0)
	require.NoError(t, s.cleanup(context.Background()))
	_, _, err = s.Result(context.Background(), token.Id, job.ID)
	assert.EqualError(t, err, "image_result_expired")
	_, err = os.Stat(filepath.Join(s.DataDir, "results", job.ID))
	assert.True(t, os.IsNotExist(err))
	replay, created, err := s.Create(context.Background(), token.Id, request)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, job.ID, replay.ID)
	assert.False(t, replay.View().CanDownload)
}

func TestImageJobUnknownExpirationNeverReleasesReservation(t *testing.T) {
	var posts atomic.Int32
	var jobID string
	s, token, _ := imageJobServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			var payload map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &payload))
			jobID = payload["job_id"].(string)
		}
		imageJobJSON(w, 200, map[string]any{"id": jobID, "status": "unknown", "result_expired": true})
	})
	job, _, err := s.Create(context.Background(), token.Id, model.ImageJobRequest{ClientRequestID: "expired-unknown", ClientSessionID: "s", Prompt: "杯子"})
	require.NoError(t, err)
	for range 3 {
		require.NoError(t, s.RunOnce(context.Background()))
	}
	current, err := model.GetImageJob(context.Background(), token.Id, job.ID)
	require.NoError(t, err)
	assert.Equal(t, "unknown", current.Status)
	assert.Equal(t, 150000, current.ReservedQuota)
	assert.Zero(t, current.ChargedQuota)
	require.NoError(t, model.DB.First(token, token.Id).Error)
	assert.Equal(t, 850000, token.RemainQuota)
	assert.Equal(t, int32(1), posts.Load())
}
