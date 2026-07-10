package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newClearModelDisabledContext(t *testing.T, query url.Values) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/model-disabled?"+query.Encode(), nil)
	ctx.Set("id", 1)
	ctx.Set("username", "root")
	ctx.Set("role", 100)
	return ctx, recorder
}

// 解除禁用端点：删除任意 source（含 manual）+ trim 恢复键 + 幂等 + 参数校验
func TestClearChannelModelDisabledHandler(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelDisabled{}, &model.ChannelModelHealth{}, &model.Log{}))

	require.NoError(t, model.UpsertChannelModelDisabled(7, "gpt-x", model.DisabledSourceManual, "人工禁用"))

	// 带首尾空格的 model 参数应被 trim 后命中禁用行
	ctx, recorder := newClearModelDisabledContext(t, url.Values{
		"channel_id": {"7"},
		"model":      {"  gpt-x  "},
	})
	ClearChannelModelDisabled(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	assert.Contains(t, body, `"success":true`)
	assert.Contains(t, body, `"changed":true`)
	assert.Contains(t, body, `"previous_source":"manual"`)

	disabled, err := model.IsChannelModelDisabled(7, "gpt-x")
	require.NoError(t, err)
	assert.False(t, disabled, "manual 行应被显式解除动作删除")

	// 幂等：再次解除返回成功但 changed=false
	ctx, recorder = newClearModelDisabledContext(t, url.Values{
		"channel_id": {"7"},
		"model":      {"gpt-x"},
	})
	ClearChannelModelDisabled(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"changed":false`)

	// 参数校验：非法渠道 ID / 空模型名
	ctx, recorder = newClearModelDisabledContext(t, url.Values{"channel_id": {"abc"}, "model": {"m"}})
	ClearChannelModelDisabled(ctx)
	assert.Contains(t, recorder.Body.String(), `"success":false`)

	ctx, recorder = newClearModelDisabledContext(t, url.Values{"channel_id": {"7"}, "model": {"   "}})
	ClearChannelModelDisabled(ctx)
	assert.Contains(t, recorder.Body.String(), `"success":false`)
}

// 解除禁用是可追责的管理动作：必须写精确审计（含 channel/model/previous_source）
func TestClearChannelModelDisabledWritesAuditLog(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelDisabled{}, &model.ChannelModelHealth{}, &model.Log{}))
	model.LOG_DB = db

	require.NoError(t, model.UpsertChannelModelDisabled(8, "claude-y", model.DisabledSourceRelay, "relay fail"))

	ctx, recorder := newClearModelDisabledContext(t, url.Values{
		"channel_id": {"8"},
		"model":      {"claude-y"},
	})
	ClearChannelModelDisabled(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)

	var logs []model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeManage).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Contains(t, logs[0].Other, "channel.model_disabled_clear")
	assert.Contains(t, logs[0].Other, "claude-y")
	assert.Contains(t, logs[0].Other, `"previous_source":"relay"`)
}
