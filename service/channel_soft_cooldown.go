package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/hot"
)

const (
	channelSoftCooldownCacheNamespace = "new-api:channel_soft_cooldown:v1"
	channelSoftCooldownCacheCapacity  = 10_000
	channelSoftCooldownCacheTimeout   = 200 * time.Millisecond
	ginKeySoftCooldownLogInfo         = "channel_soft_cooldown_log_info"

	SoftFailoverOutcomeSameRequestSuccess  = "same_request_success"
	SoftFailoverOutcomeDeferredAfterCommit = "deferred_after_commit"
	SoftFailoverOutcomeExhausted           = "exhausted"
	SoftFailoverOutcomeAllCooling          = "all_cooling"
)

var (
	channelSoftCooldownCacheOnce sync.Once
	channelSoftCooldownCache     *cachex.HybridCache[ChannelSoftCooldownEntry]

	channelSoftCooldownApplied = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "newapi_channel_soft_cooldown_applied_total",
			Help: "成功写入的渠道软故障冷却总数",
		},
		[]string{"channel_id", "error_class"},
	)
	channelSoftCooldownSkipped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "newapi_channel_soft_cooldown_skipped_total",
			Help: "选路时因软故障冷却跳过渠道的总数",
		},
		[]string{"channel_id"},
	)
	channelSoftFailover = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "newapi_channel_soft_failover_total",
			Help: "渠道软故障切换结果总数",
		},
		[]string{"outcome"},
	)
	channelSoftCooldownCacheErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "newapi_channel_soft_cooldown_cache_errors_total",
			Help: "渠道软故障冷却缓存操作错误总数",
		},
		[]string{"operation"},
	)
)

type channelSoftCooldownLogInfo struct {
	Applied                 bool
	Seconds                 int
	ReasonClass             string
	SkippedChannelIDs       []int
	CacheBackend            string
	CacheDegraded           bool
	AffinityCleared         bool
	FailoverOutcomeRecorded bool
}

type ChannelSoftCooldownEntry struct {
	ExpiresAt  time.Time `json:"expires_at"`
	StatusCode int       `json:"status_code"`
	ErrorClass string    `json:"error_class"`
}

func init() {
	prometheus.MustRegister(
		channelSoftCooldownApplied,
		channelSoftCooldownSkipped,
		channelSoftFailover,
		channelSoftCooldownCacheErrors,
	)
}

func channelSoftCooldownCacheBackend() string {
	if common.RedisEnabled && common.RDB != nil {
		return "redis"
	}
	return "local"
}

func getSoftCooldownLogInfo(c *gin.Context) (*channelSoftCooldownLogInfo, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.Get(ginKeySoftCooldownLogInfo)
	if !ok || value == nil {
		return nil, false
	}
	info, ok := value.(*channelSoftCooldownLogInfo)
	return info, ok && info != nil
}

func getOrCreateSoftCooldownLogInfo(c *gin.Context) *channelSoftCooldownLogInfo {
	if c == nil {
		return nil
	}
	if info, ok := getSoftCooldownLogInfo(c); ok {
		return info
	}
	info := &channelSoftCooldownLogInfo{
		SkippedChannelIDs: make([]int, 0),
		CacheBackend:      channelSoftCooldownCacheBackend(),
	}
	c.Set(ginKeySoftCooldownLogInfo, info)
	return info
}

func recordSoftCooldownCacheAccessForLog(c *gin.Context, degraded bool) {
	info := getOrCreateSoftCooldownLogInfo(c)
	if info == nil {
		return
	}
	info.CacheBackend = channelSoftCooldownCacheBackend()
	info.CacheDegraded = info.CacheDegraded || degraded
}

func softCooldownCacheCircuitOpen(c *gin.Context) bool {
	info, ok := getSoftCooldownLogInfo(c)
	return ok && info.CacheDegraded
}

func channelSoftCooldownCacheContext(c *gin.Context) (context.Context, context.CancelFunc) {
	requestContext := context.Background()
	if c != nil && c.Request != nil {
		requestContext = c.Request.Context()
	}
	return context.WithTimeout(requestContext, channelSoftCooldownCacheTimeout)
}

func channelSoftCooldownRequestCanceled(c *gin.Context, err error) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return errors.Is(err, c.Request.Context().Err())
}

func classifyChannelSoftCooldownError(statusCode int, errorType string) string {
	if statusCode == 429 {
		return "rate_limit"
	}
	if statusCode == 529 {
		return "overloaded"
	}

	switch strings.ToLower(strings.TrimSpace(errorType)) {
	case "rate_limit", "rate_limit_error", "rate_limit_exceeded", "resource_exhausted":
		return "rate_limit"
	case "overloaded", "overloaded_error", "system_cpu_overloaded", "system_memory_overloaded", "system_disk_overloaded", "model_capacity_exhausted":
		return "overloaded"
	}
	if statusCode >= 500 && statusCode <= 599 {
		return "server_error"
	}
	return "unknown"
}

func recordSoftCooldownAppliedForLog(c *gin.Context, seconds int, reasonClass string, cacheDegraded bool) {
	info := getOrCreateSoftCooldownLogInfo(c)
	if info == nil {
		return
	}
	info.Applied = info.Applied || !cacheDegraded
	info.Seconds = seconds
	info.ReasonClass = reasonClass
	info.CacheBackend = channelSoftCooldownCacheBackend()
	info.CacheDegraded = info.CacheDegraded || cacheDegraded
}

func recordSoftCooldownSkippedChannelForLog(c *gin.Context, channelID int) {
	if channelID <= 0 {
		return
	}
	info := getOrCreateSoftCooldownLogInfo(c)
	if info == nil {
		return
	}
	for _, recordedChannelID := range info.SkippedChannelIDs {
		if recordedChannelID == channelID {
			return
		}
	}
	info.SkippedChannelIDs = append(info.SkippedChannelIDs, channelID)
}

func RecordAffinityClearedForLog(c *gin.Context) {
	info := getOrCreateSoftCooldownLogInfo(c)
	if info != nil {
		info.AffinityCleared = true
	}
}

func AppendChannelSoftCooldownAdminInfo(c *gin.Context, adminInfo map[string]interface{}) {
	if c == nil || adminInfo == nil || !operation_setting.IsSoftFailureCooldownEnabled() {
		return
	}
	info, hasInfo := getSoftCooldownLogInfo(c)
	if !hasInfo {
		return
	}
	if !info.Applied && len(info.SkippedChannelIDs) == 0 && !info.CacheDegraded && !info.AffinityCleared {
		return
	}
	streamCommitted := common.GetContextKeyBool(c, constant.ContextKeyClaudeStreamCommitted)
	cacheBackend := info.CacheBackend
	if cacheBackend == "" {
		cacheBackend = channelSoftCooldownCacheBackend()
	}
	adminInfo["soft_cooldown"] = map[string]interface{}{
		"applied":                   info.Applied,
		"seconds":                   info.Seconds,
		"reason_class":              info.ReasonClass,
		"skipped_channel_ids":       append([]int{}, info.SkippedChannelIDs...),
		"cache_backend":             cacheBackend,
		"cache_degraded":            info.CacheDegraded,
		"stream_semantic_committed": streamCommitted,
		"affinity_cleared":          info.AffinityCleared,
	}
}

func RecordChannelSoftFailoverOutcome(c *gin.Context, outcome string) {
	if !operation_setting.IsSoftFailureCooldownEnabled() {
		return
	}
	switch outcome {
	case SoftFailoverOutcomeSameRequestSuccess,
		SoftFailoverOutcomeDeferredAfterCommit,
		SoftFailoverOutcomeExhausted,
		SoftFailoverOutcomeAllCooling:
	default:
		return
	}
	info, ok := getSoftCooldownLogInfo(c)
	if !ok || (!info.Applied && len(info.SkippedChannelIDs) == 0 && !info.AffinityCleared) || info.FailoverOutcomeRecorded {
		return
	}
	info.FailoverOutcomeRecorded = true
	channelSoftFailover.WithLabelValues(outcome).Inc()
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

func RecordChannelSoftCooldown(c *gin.Context, channelID int, modelName string, statusCode int, errorClass string) {
	if !operation_setting.IsSoftFailureCooldownEnabled() {
		return
	}
	if softCooldownCacheCircuitOpen(c) {
		return
	}

	seconds := operation_setting.GetSoftFailureCooldownSeconds()
	ttl := time.Duration(seconds) * time.Second
	now := time.Now()
	errorClass = classifyChannelSoftCooldownError(statusCode, errorClass)
	entry := ChannelSoftCooldownEntry{
		ExpiresAt:  now.Add(ttl),
		StatusCode: statusCode,
		ErrorClass: errorClass,
	}
	key := fmt.Sprintf("%d:%s", channelID, modelName)
	ctx, cancel := channelSoftCooldownCacheContext(c)
	defer cancel()
	err := getChannelSoftCooldownCache().SetWithTTLContext(ctx, key, entry, ttl)
	if err != nil && channelSoftCooldownRequestCanceled(c, err) {
		return
	}
	recordSoftCooldownAppliedForLog(c, seconds, errorClass, err != nil)
	if err != nil {
		channelSoftCooldownCacheErrors.WithLabelValues("set").Inc()
		common.SysError(fmt.Sprintf("channel soft cooldown cache set failed: err=%v", err))
		return
	}
	channelSoftCooldownApplied.WithLabelValues(strconv.Itoa(channelID), errorClass).Inc()
}

func GetChannelSoftCooldown(c *gin.Context, channelID int, modelName string) (entry ChannelSoftCooldownEntry, cooling bool) {
	if !operation_setting.IsSoftFailureCooldownEnabled() {
		return ChannelSoftCooldownEntry{}, false
	}
	if softCooldownCacheCircuitOpen(c) {
		return ChannelSoftCooldownEntry{}, false
	}

	key := fmt.Sprintf("%d:%s", channelID, modelName)
	ctx, cancel := channelSoftCooldownCacheContext(c)
	defer cancel()
	entry, found, err := getChannelSoftCooldownCache().GetWithContext(ctx, key)
	if err != nil && channelSoftCooldownRequestCanceled(c, err) {
		return ChannelSoftCooldownEntry{}, false
	}
	recordSoftCooldownCacheAccessForLog(c, err != nil)
	if err != nil {
		channelSoftCooldownCacheErrors.WithLabelValues("get").Inc()
		common.SysError(fmt.Sprintf("channel soft cooldown cache get failed: err=%v", err))
		return ChannelSoftCooldownEntry{}, false
	}
	if !found || !entry.ExpiresAt.After(time.Now()) {
		return ChannelSoftCooldownEntry{}, false
	}
	return entry, true
}
