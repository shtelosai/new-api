package middleware

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
)

func TestDistributeProtocolBoundaryOnVersionAndPinnedRoutes(t *testing.T) {
	require.NoError(t, i18n.Init())
	// 使用真实启动初始化列名；数据库固定为测试临时文件，禁止读取外部 DSN。
	t.Setenv("SQL_DSN", "local")
	t.Setenv("LOG_SQL_DSN", "")
	t.Setenv("SKIP_AUTO_MIGRATE", "true")
	previousDB, previousPath := model.DB, common.SQLitePath
	common.SQLitePath = filepath.Join(t.TempDir(), "startup.db")
	require.NoError(t, model.InitDB())
	startupDB, err := model.DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = startupDB.Close(); model.DB = previousDB; common.SQLitePath = previousPath })
	for _, name := range []string{"auto", "summarization-model", "qwen3.8-flash", "claude-sonnet-5"} {
		for _, version := range []string{"", "3.9.9", "4.0.0", "4.1.0", "10.0.0"} {
			for _, cached := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/cache=%v", name, version, cached), func(t *testing.T) {
					setupDistributorTokenAffinityDB(t)
					common.MemoryCacheEnabled = cached
					seedDistributorChannel(t, 4501, name, 100)
					seedDistributorChannel(t, 4502, name, 10)
					// 把不匹配渠道设为更高优先级，证明过滤发生在选优先级之前。
					blocked, allowed := 14, 1
					if name == "claude-sonnet-5" {
						blocked, allowed = 1, 14
					}
					require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4501).Update("type", blocked).Error)
					require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4502).Update("type", allowed).Error)
					model.InitChannelCache()
					setup := func(c *gin.Context) {
						if version != "" {
							c.Request.Header.Set("X-Twork-Client-Version", version)
						}
					}
					current := version != "" && version != "3.9.9"
					want := 4501
					if current {
						want = 4502
					}
					got := requestRuntimeRoute(t, 0, "default", "/v1/messages", `{"model":"`+name+`"}`, 200, setup)
					require.Equal(t, want, got.ChannelID)
					status := 200
					if current {
						status = 403
					}
					requestRuntimeRoute(t, 0, "default", "/v1/messages", `{"model":"`+name+`"}`, status, func(c *gin.Context) {
						setup(c)
						common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "4501")
					})
				})
			}
		}
	}
}
