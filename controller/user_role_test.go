package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateUserValidatesRoleBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator int
		role     int
		allowed  bool
	}{
		{name: "拒绝负角色", operator: common.RoleAdminUser, role: -1},
		{name: "拒绝中间角色", operator: common.RoleAdminUser, role: 5},
		{name: "根管理员也拒绝非法角色", operator: common.RoleRootUser, role: 99},
		{name: "允许创建普通用户", operator: common.RoleAdminUser, role: common.RoleCommonUser, allowed: true},
		{name: "允许根管理员创建管理员", operator: common.RoleRootUser, role: common.RoleAdminUser, allowed: true},
		{name: "拒绝创建同级管理员", operator: common.RoleAdminUser, role: common.RoleAdminUser},
		{name: "拒绝创建更高角色", operator: common.RoleAdminUser, role: common.RoleRootUser},
		{name: "拒绝根管理员创建同级", operator: common.RoleRootUser, role: common.RoleRootUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Log{}))
			originalQuota := common.QuotaForNewUser
			common.QuotaForNewUser = 0
			t.Cleanup(func() { common.QuotaForNewUser = originalQuota })
			body, err := common.Marshal(map[string]any{
				"username": "role-test-user", "password": "TestPassword123", "role": tc.role,
			})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/user/", bytes.NewReader(body))
			ctx.Set("role", tc.operator)

			CreateUser(ctx)

			var response struct {
				Success bool `json:"success"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, tc.allowed, response.Success, recorder.Body.String())
			var users []model.User
			require.NoError(t, db.Find(&users).Error)
			if tc.allowed {
				require.Len(t, users, 1)
				assert.Equal(t, tc.role, users[0].Role)
			} else {
				assert.Empty(t, users, "拒绝请求不能写入用户")
			}
		})
	}
}
