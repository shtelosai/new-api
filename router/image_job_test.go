package router

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageJobHTTPFixture(t *testing.T, enabled bool) (*gin.Engine, *model.Token, *model.Token) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldDB, oldLog, oldRedis, oldBatch := model.DB, model.LOG_DB, common.RedisEnabled, common.BatchUpdateEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "image-http.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB = db
	model.LOG_DB = db
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, db.AutoMigrate(&model.ImageJob{}, &model.ImageJobProjection{}, &model.ImageJobLogReceipt{}, &model.Token{}, &model.User{}, &model.Channel{}, &model.Ability{}, &model.TokenModelChannel{}, &model.ChannelModelDisabled{}, &model.Log{}))
	t.Cleanup(func() {
		model.DB = oldDB
		model.LOG_DB = oldLog
		common.RedisEnabled = oldRedis
		common.BatchUpdateEnabled = oldBatch
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "images", Status: common.UserStatusEnabled, Group: "default", Quota: 3000000}).Error)
	a := &model.Token{Id: 2, UserId: 1, Key: "http-image-key-a", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 150000, Group: "default"}
	b := &model.Token{Id: 3, UserId: 1, Key: "http-image-key-b", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 150000, Group: "default"}
	require.NoError(t, db.Create(a).Error)
	require.NoError(t, db.Create(b).Error)
	ch := model.Channel{Id: 4, Status: common.ChannelStatusEnabled, Key: "private-key", BaseURL: common.GetPointer("http://127.0.0.1:1/kie"), Models: "twork-image-flare-async", Group: "default", Setting: common.GetPointer(`{"twork_runtime":"image_async","twork_image_provider":"kie","twork_image_family":"flare"}`)}
	require.NoError(t, db.Create(&ch).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: ch.Models, ChannelId: ch.Id, Enabled: true}).Error)
	require.NoError(t, db.Create(&model.TokenModelChannel{TokenId: a.Id, ModelId: ch.Models, ChannelId: ch.Id}).Error)
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "false")
	if enabled {
		t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "true")
	}
	t.Setenv("TWORK_IMAGE_TASKS_DATA_DIR", t.TempDir())
	stop := service.StartImageJobWorker()
	t.Cleanup(stop)
	engine := gin.New()
	SetImageJobRouter(engine)
	return engine, a, b
}
func imageJobHTTP(t *testing.T, engine *gin.Engine, key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	r.Header.Set("Authorization", "Bearer sk-"+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, r)
	return w
}

func TestImageJobHTTPDisabledOnlyDisablesNewRoutes(t *testing.T) {
	engine, a, _ := imageJobHTTPFixture(t, false)
	w := imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/capabilities", nil)
	require.Equal(t, 200, w.Code)
	assert.JSONEq(t, `{"async":false,"protocol_version":1,"prices_cents":{"1k":30,"2k":40,"4k":80},"resolutions":[],"sizes":[],"supports":{"edit":false,"mask":false,"transparent":false},"max_reference_images":16}`, w.Body.String())
	w = imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", map[string]any{"prompt": "cup"})
	assert.Equal(t, 503, w.Code)
}

func TestImageJobHTTPTokenBoundaryExhaustedReadAndCreateReplay(t *testing.T) {
	engine, a, b := imageJobHTTPFixture(t, true)
	request := map[string]any{"client_request_id": "create-1", "client_session_id": "session-1", "prompt": "蓝色杯子"}
	w := imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	require.Equal(t, 202, w.Code, w.Body.String())
	var job model.ImageJobView
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &job))
	assert.Equal(t, 30, job.PriceCents)
	require.NotEmpty(t, job.ID)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", a.Id).Update("status", common.TokenStatusExhausted).Error)
	w = imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	assert.Equal(t, 202, w.Code, w.Body.String())
	w = imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/"+job.ID, nil)
	assert.Equal(t, 200, w.Code)
	w = imageJobHTTP(t, engine, b.Key, "GET", "/v1/image-tasks/"+job.ID, nil)
	assert.Equal(t, 404, w.Code)
	w = imageJobHTTP(t, engine, b.Key, "GET", "/v1/image-tasks/"+job.ID+"/result", nil)
	assert.Equal(t, 404, w.Code)
	w = imageJobHTTP(t, engine, b.Key, "GET", "/v1/image-tasks?client_session_id=session-1", nil)
	assert.JSONEq(t, `{"items":[]}`, w.Body.String())
	w = imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks?client_session_id=another", nil)
	assert.JSONEq(t, `{"items":[]}`, w.Body.String())
	request["prompt"] = "别的图"
	w = imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	assert.Equal(t, 409, w.Code)
	for _, status := range []int{common.TokenStatusDisabled, common.TokenStatusExpired} {
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", a.Id).Update("status", status).Error)
		w = imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/"+job.ID, nil)
		assert.Equal(t, 401, w.Code)
	}
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", a.Id).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": time.Now().Unix() - 1}).Error)
	w = imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/"+job.ID, nil)
	assert.Equal(t, 401, w.Code)
	_, err := model.GetImageJob(context.Background(), a.Id, job.ID)
	require.NoError(t, err)
}

func TestImageJobHTTPRejectsClientPriceModelCountAndUnknownFields(t *testing.T) {
	engine, a, _ := imageJobHTTPFixture(t, true)
	for _, field := range []string{"n", "model", "channel_id", "price_cents", "callback_url", "background_extra"} {
		request := map[string]any{"client_request_id": "invalid", "client_session_id": "s", "prompt": "杯子", field: 1}
		w := imageJobHTTP(t, engine, a.Key, http.MethodPost, "/v1/image-tasks", request)
		assert.Equal(t, 400, w.Code, field)
	}
	w := imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/capabilities", nil)
	require.Equal(t, 200, w.Code)
	var cap map[string]any
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &cap))
	assert.Equal(t, true, cap["async"])
	assert.Equal(t, []any{"1k", "2k", "4k"}, cap["resolutions"])
	assert.Equal(t, false, cap["supports"].(map[string]any)["mask"])
	var count int64
	require.NoError(t, model.DB.Model(&model.ImageJob{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestImageJobClosingCreationKeepsExistingTasksAndIdempotentReplay(t *testing.T) {
	engine, a, _ := imageJobHTTPFixture(t, true)
	request := map[string]any{"client_request_id": "before-pause", "client_session_id": "s", "prompt": "杯子"}
	w := imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	require.Equal(t, 202, w.Code)
	var job model.ImageJobView
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &job))
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "false")
	w = imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/"+job.ID, nil)
	assert.Equal(t, 200, w.Code)
	w = imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	assert.Equal(t, 202, w.Code)
	request["client_request_id"] = "after-pause"
	w = imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	assert.Equal(t, 503, w.Code)
	w = imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks/capabilities", nil)
	require.Equal(t, 200, w.Code)
	var cap map[string]any
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &cap))
	assert.Equal(t, false, cap["async"])
}

func TestImageJobReplayOnlyHeaderCannotCreateButCanRecoverExisting(t *testing.T) {
	engine, a, _ := imageJobHTTPFixture(t, true)
	request := map[string]any{"client_request_id": "replay-only", "client_session_id": "s", "prompt": "杯子"}
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	replay := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/image-tasks", bytes.NewReader(encoded))
		r.Header.Set("Authorization", "Bearer sk-"+a.Key)
		r.Header.Set("X-Twork-Image-Replay-Only", "true")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}
	assert.Equal(t, 503, replay().Code)
	w := imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", request)
	require.Equal(t, 202, w.Code)
	assert.Equal(t, 202, replay().Code)
	var count int64
	require.NoError(t, model.DB.Model(&model.ImageJob{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestImageJobAccountingPrepareRemainsAvailableWithCreationPaused(t *testing.T) {
	engine, a, _ := imageJobHTTPFixture(t, true)
	t.Setenv("TWORK_IMAGE_TASKS_ENABLED", "false")
	for i := 0; i < 2; i++ {
		w := imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks/accounting/prepare", nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.JSONEq(t, `{"prepared":true}`, w.Body.String())
	}
	var jobs int64
	require.NoError(t, model.DB.Model(&model.ImageJob{}).Count(&jobs).Error)
	assert.Zero(t, jobs)
	var token model.Token
	require.NoError(t, model.DB.First(&token, a.Id).Error)
	assert.Equal(t, 150000, token.RemainQuota)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", a.Id).Update("status", common.TokenStatusDisabled).Error)
	w := imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks/accounting/prepare", nil)
	assert.Equal(t, 401, w.Code)
}

func TestImageJobListHonorsRequestedLimitAndCursor(t *testing.T) {
	engine, a, _ := imageJobHTTPFixture(t, true)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", a.Id).Update("remain_quota", 1000000).Error)
	for _, id := range []string{"page-a", "page-b", "page-c"} {
		response := imageJobHTTP(t, engine, a.Key, "POST", "/v1/image-tasks", map[string]any{"client_request_id": id, "client_session_id": "s", "prompt": "杯子"})
		require.Equal(t, 202, response.Code)
	}
	seen := map[string]bool{}
	cursor := ""
	for index := 0; index < 3; index++ {
		response := imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks?limit=1&cursor="+cursor, nil)
		require.Equal(t, 200, response.Code)
		var page struct {
			Items []model.ImageJobView `json:"items"`
			Next  string               `json:"next_cursor"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &page))
		require.Len(t, page.Items, 1)
		assert.False(t, seen[page.Items[0].ID])
		seen[page.Items[0].ID] = true
		cursor = page.Next
		if index < 2 {
			require.NotEmpty(t, cursor)
		} else {
			assert.Empty(t, cursor)
		}
	}
	for _, limit := range []string{"0", "101", "abc"} {
		response := imageJobHTTP(t, engine, a.Key, "GET", "/v1/image-tasks?limit="+limit, nil)
		assert.Equal(t, 400, response.Code)
	}
}
