package model

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// TokenModelChannel 存储每个 token 对每个模型的选定渠道
// 由 tbackend 同步写入，无记录 = 不限制渠道（向后兼容）
type TokenModelChannel struct {
	Id        int    `json:"id" gorm:"primaryKey"`
	TokenId   int    `json:"token_id" gorm:"not null;uniqueIndex:uq_token_model_channel"`
	ModelId   string `json:"model_id" gorm:"type:varchar(255);not null;uniqueIndex:uq_token_model_channel;index:idx_token_model"`
	ChannelId int    `json:"channel_id" gorm:"not null;uniqueIndex:uq_token_model_channel"`
}

// --- 缓存层 ---
// 使用原子指针 + map 缓存，定期刷新（30秒）
// 最终一致性：配置变更后最多 30 秒生效

var tokenModelChannelCachePtr atomic.Pointer[map[string][]int]
var tokenModelChannelCacheOnce sync.Once

func init() {
	empty := make(map[string][]int)
	tokenModelChannelCachePtr.Store(&empty)
}

func tokenModelChannelCacheKey(tokenId int, modelId string) string {
	return fmt.Sprintf("%d:%s", tokenId, modelId)
}

// InitTokenModelChannelCache 启动缓存定期刷新
func InitTokenModelChannelCache() {
	tokenModelChannelCacheOnce.Do(func() {
		go func() {
			for {
				refreshTokenModelChannelCache()
				time.Sleep(30 * time.Second)
			}
		}()
	})
}

// refreshTokenModelChannelCache 全量刷新缓存（原子替换，无并发窗口）
func refreshTokenModelChannelCache() {
	var records []TokenModelChannel
	err := DB.Find(&records).Error
	if err != nil {
		common.SysError(fmt.Sprintf("failed to refresh token_model_channels cache: %v", err))
		return
	}

	// 构建新缓存
	newCache := make(map[string][]int)
	for _, r := range records {
		key := tokenModelChannelCacheKey(r.TokenId, r.ModelId)
		newCache[key] = append(newCache[key], r.ChannelId)
	}

	// 原子替换指针，读写无竞争
	tokenModelChannelCachePtr.Store(&newCache)
}

// LoadTokenModelChannels 查询 token+model 的选定渠道ID列表
// 返回 nil = 不限制
func LoadTokenModelChannels(tokenId int, modelId string) []int {
	if tokenId <= 0 {
		return nil
	}
	cache := tokenModelChannelCachePtr.Load()
	if cache == nil {
		return nil
	}
	key := tokenModelChannelCacheKey(tokenId, modelId)
	channelIds, ok := (*cache)[key]
	if !ok {
		return nil
	}
	return channelIds
}

// IsChannelAllowedForToken 判断渠道是否满足 token_model_channels 白名单。
// 空白名单表示不限制；匹配 key 使用调用方传入的原始 model 名。
func IsChannelAllowedForToken(tokenId int, modelName string, channelId int) bool {
	if channelId <= 0 {
		return false
	}
	allowed := LoadTokenModelChannels(tokenId, modelName)
	if len(allowed) == 0 {
		return true
	}
	for _, id := range allowed {
		if id == channelId {
			return true
		}
	}
	return false
}

// FilterChannelsByToken 根据 token_model_channels 过滤渠道列表
// 如果该 token+model 有选定渠道，只保留选定的；否则不过滤。
// 注意：该过滤只在内存缓存选路路径生效；MemoryCacheEnabled=false 时 DB 直查路径不过滤。
func FilterChannelsByToken(channelIds []int, tokenId int, modelId string) []int {
	allowed := LoadTokenModelChannels(tokenId, modelId)
	if len(allowed) == 0 {
		return channelIds // 不限制
	}

	allowedSet := make(map[int]bool, len(allowed))
	for _, id := range allowed {
		allowedSet[id] = true
	}

	filtered := make([]int, 0, len(channelIds))
	for _, id := range channelIds {
		if allowedSet[id] {
			filtered = append(filtered, id)
		}
	}
	return filtered
}
