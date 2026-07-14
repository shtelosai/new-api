package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/samber/hot"
)

const (
	channelSoftCooldownCacheNamespace = "new-api:channel_soft_cooldown:v1"
	channelSoftCooldownCacheCapacity  = 10_000
)

var (
	channelSoftCooldownCacheOnce sync.Once
	channelSoftCooldownCache     *cachex.HybridCache[ChannelSoftCooldownEntry]
)

type ChannelSoftCooldownEntry struct {
	ExpiresAt  time.Time `json:"expires_at"`
	StatusCode int       `json:"status_code"`
	ErrorClass string    `json:"error_class"`
}

func getChannelSoftCooldownCache() *cachex.HybridCache[ChannelSoftCooldownEntry] {
	channelSoftCooldownCacheOnce.Do(func() {
		defaultTTL := time.Duration(operation_setting.GetSoftFailureCooldownSeconds()) * time.Second
		channelSoftCooldownCache = cachex.NewHybridCache[ChannelSoftCooldownEntry](cachex.HybridCacheConfig[ChannelSoftCooldownEntry]{
			Namespace: cachex.Namespace(channelSoftCooldownCacheNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[ChannelSoftCooldownEntry]{},
			Memory: func() *hot.HotCache[string, ChannelSoftCooldownEntry] {
				return hot.NewHotCache[string, ChannelSoftCooldownEntry](hot.LRU, channelSoftCooldownCacheCapacity).
					WithTTL(defaultTTL).
					WithJanitor().
					Build()
			},
		})
	})
	return channelSoftCooldownCache
}

func RecordChannelSoftCooldown(channelID int, modelName string, statusCode int, errorClass string) {
	if !operation_setting.IsSoftFailureCooldownEnabled() {
		return
	}

	ttl := time.Duration(operation_setting.GetSoftFailureCooldownSeconds()) * time.Second
	now := time.Now()
	entry := ChannelSoftCooldownEntry{
		ExpiresAt:  now.Add(ttl),
		StatusCode: statusCode,
		ErrorClass: errorClass,
	}
	key := fmt.Sprintf("%d:%s", channelID, modelName)
	if err := getChannelSoftCooldownCache().SetWithTTL(key, entry, ttl); err != nil {
		common.SysError(fmt.Sprintf("channel soft cooldown cache set failed: err=%v", err))
	}
}

func GetChannelSoftCooldown(channelID int, modelName string) (entry ChannelSoftCooldownEntry, cooling bool) {
	if !operation_setting.IsSoftFailureCooldownEnabled() {
		return ChannelSoftCooldownEntry{}, false
	}

	key := fmt.Sprintf("%d:%s", channelID, modelName)
	entry, found, err := getChannelSoftCooldownCache().Get(key)
	if err != nil {
		common.SysError(fmt.Sprintf("channel soft cooldown cache get failed: err=%v", err))
		return ChannelSoftCooldownEntry{}, false
	}
	if !found || !entry.ExpiresAt.After(time.Now()) {
		return ChannelSoftCooldownEntry{}, false
	}
	return entry, true
}
