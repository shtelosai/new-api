package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 生产令牌桶缓存首个 Redis client，整个测试进程共用一个本地服务，支持 -count 重复验证。
var modelLimitTestUsers atomic.Int64
var modelLimitTestRedis *miniredis.Miniredis
var modelLimitTestClient *redis.Client

func TestMain(m *testing.M) {
	server, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	modelLimitTestRedis = server
	modelLimitTestClient = redis.NewClient(&redis.Options{Addr: server.Addr()})
	result := m.Run()
	_ = modelLimitTestClient.Close()
	server.Close()
	os.Exit(result)
}

func TestModelRateLimitConfiguredGroupIsolation(t *testing.T) {
	oldEnabled, oldMinutes := setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitDurationMinutes
	oldTotal, oldSuccess := setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount
	oldGroups, oldRedis, oldClient := setting.ModelRequestRateLimitGroup, common.RedisEnabled, common.RDB
	t.Cleanup(func() {
		setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitDurationMinutes = oldEnabled, oldMinutes
		setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount = oldTotal, oldSuccess
		setting.ModelRequestRateLimitGroup, common.RedisEnabled, common.RDB = oldGroups, oldRedis, oldClient
	})
	setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitDurationMinutes = true, 60
	common.RDB = modelLimitTestClient
	inMemoryRateLimiter.Init(0)
	for _, backend := range []string{"memory", "redis"} {
		for _, limit := range []string{"total", "success"} {
			t.Run(backend+"/"+limit, func(t *testing.T) {
				common.RedisEnabled = backend == "redis"
				require.NoError(t, modelLimitTestClient.FlushDB(context.Background()).Err())
				modelLimitTestRedis.SetTime(time.Unix(1800000000, 0))
				counts := [2]int{1, 1000}
				if limit == "success" {
					counts = [2]int{0, 1}
				}
				setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount = counts[0], counts[1]
				highCounts := [2]int{3, 1000}
				if limit == "success" {
					highCounts = [2]int{0, 3}
				}
				setting.ModelRequestRateLimitGroup = map[string][2]int{"a": highCounts, "b": counts, "fallback": counts}
				userOffset := int(modelLimitTestUsers.Add(10)) + 830000
				router := gin.New()
				router.GET("/:group/:user", func(c *gin.Context) {
					user, err := strconv.Atoi(c.Param("user"))
					require.NoError(t, err)
					c.Set("id", user+userOffset)
					if c.Param("group") != "fallback" {
						common.SetContextKey(c, constant.ContextKeyTokenGroup, c.Param("group"))
					}
					common.SetContextKey(c, constant.ContextKeyUserGroup, "fallback")
				}, ModelRequestRateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })
				// 另一个用户、独立分组不互相挤占；无独立配置的 c/d 必须共用全局计数。
				steps := []struct {
					path string
					want int
				}{
					{"/a/1", 200}, {"/b/1", 200},
					{"/a/1", 200}, {"/a/1", 200}, {"/a/1", 429}, {"/b/1", 429},
					{"/b/2", 200}, {"/b/2", 429},
					{"/c/1", 200}, {"/d/1", 429},
					{"/fallback/1", 200}, {"/fallback/1", 429},
				}
				for _, step := range steps {
					path := step.path
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
					assert.Equal(t, step.want, recorder.Code, "%s", path)
				}
				setting.ModelRequestRateLimitEnabled = false
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/a/1", nil))
				assert.Equal(t, http.StatusOK, recorder.Code, "停用限流后不应继续拦截")
				setting.ModelRequestRateLimitEnabled = true
			})
		}
	}
}
