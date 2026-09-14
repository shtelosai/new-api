package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
)

const responsesRecoveryProbeInterval = 30 * time.Second
const responsesRecoveryProbeContextKey = "responses_recovery_probe"

var responsesRecoveryMutex sync.Mutex
var responsesRecoveryLeases = hot.NewHotCache[string, bool](hot.LRU, channelSoftCooldownCacheCapacity).WithTTL(responsesRecoveryProbeInterval).Build()

type responsesRecoveryProbe struct {
	Key   string
	Entry ChannelSoftCooldownEntry
}

// 冷却结束后每个渠道、模型最多每 30 秒放行一次真实请求试探。
func allowResponsesRecoveryProbe(c *gin.Context, key string, entry ChannelSoftCooldownEntry) bool {
	if current, ok := c.Get(responsesRecoveryProbeContextKey); ok {
		probe := current.(responsesRecoveryProbe)
		if probe.Key == key && probe.Entry.ExpiresAt.Equal(entry.ExpiresAt) {
			return true
		}
	}
	leaseKey := getChannelSoftCooldownCache().FullKey(key) + ":probe:" + fmt.Sprint(entry.ExpiresAt.UnixNano())
	var allowed bool
	var err error
	if common.RedisEnabled && common.RDB != nil {
		ctx, cancel := channelSoftCooldownCacheContext(c)
		defer cancel()
		allowed, err = common.RDB.SetNX(ctx, leaseKey, "1", responsesRecoveryProbeInterval).Result()
	} else {
		responsesRecoveryMutex.Lock()
		_, exists, _ := responsesRecoveryLeases.Get(leaseKey)
		allowed = !exists
		if allowed {
			responsesRecoveryLeases.Set(leaseKey, true)
		}
		responsesRecoveryMutex.Unlock()
	}
	if err != nil && channelSoftCooldownRequestCanceled(c, err) {
		return true
	}
	if err != nil {
		recordSoftCooldownCacheAccessForLog(c, true)
		channelSoftCooldownCacheErrors.WithLabelValues("probe").Inc()
		return true // 与现有冷却缓存一致：缓存故障不阻断模型服务。
	}
	if allowed {
		c.Set(responsesRecoveryProbeContextKey, responsesRecoveryProbe{Key: key, Entry: entry})
	}
	return allowed
}

// 成功只恢复本次试探对应的故障代次，不能覆盖其他并发请求报告的新故障。
func CompleteResponsesRecovery(c *gin.Context, channelID int, modelName string) {
	value, found := c.Get(responsesRecoveryProbeContextKey)
	if !found {
		return
	}
	probe := value.(responsesRecoveryProbe)
	if probe.Key != fmt.Sprintf("%d:%s", channelID, modelName) {
		return
	}
	ctx, cancel := channelSoftCooldownCacheContext(c)
	defer cancel()
	var err error
	if common.RedisEnabled && common.RDB != nil {
		const script = `local value = redis.call('GET', KEYS[1])
if value and cjson.decode(value).expires_at == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`
		err = common.RDB.Eval(ctx, script, []string{getChannelSoftCooldownCache().FullKey(probe.Key)}, probe.Entry.ExpiresAt.Format(time.RFC3339Nano)).Err()
	} else {
		responsesRecoveryMutex.Lock()
		current, ok, readErr := getChannelSoftCooldownCache().GetWithContext(ctx, probe.Key)
		err = readErr
		if err == nil && ok && current.ExpiresAt.Equal(probe.Entry.ExpiresAt) {
			err = getChannelSoftCooldownCache().SetWithTTLContext(ctx, probe.Key, ChannelSoftCooldownEntry{}, time.Nanosecond)
		}
		responsesRecoveryMutex.Unlock()
	}
	if err != nil && channelSoftCooldownRequestCanceled(c, err) {
		return
	}
	if err != nil {
		recordSoftCooldownCacheAccessForLog(c, true)
		channelSoftCooldownCacheErrors.WithLabelValues("recover").Inc()
	}
}
