package service

import (
	"errors"
	"math"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

type RetryParam struct {
	Ctx                    *gin.Context
	TokenGroup             string
	ModelName              string
	RequestPath            string
	TokenId                int
	Retry                  *int
	ExcludedChannelIds     map[int]struct{}
	UseSoftFailureCooldown bool
	resetNextTry           bool
}

var ErrNoUntriedChannel = errors.New("no untried channel")

// ErrAllChannelsCooling distinguishes temporary cooldown exhaustion from an
// ordinary lack of eligible channels.
var ErrAllChannelsCooling = errors.New("all eligible channels are cooling down")

// AllChannelsCoolingError carries the earliest time at which selection can be retried.
type AllChannelsCoolingError struct {
	earliestExpiresAt time.Time
}

func (e *AllChannelsCoolingError) Error() string {
	return ErrAllChannelsCooling.Error()
}

func (e *AllChannelsCoolingError) Unwrap() error {
	return ErrAllChannelsCooling
}

func (e *AllChannelsCoolingError) RetryAfterSeconds() int {
	if e == nil {
		return 1
	}
	seconds := int(math.Ceil(time.Until(e.earliestExpiresAt).Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (p *RetryParam) GetRetry() int {
	if p.Retry == nil {
		return 0
	}
	return *p.Retry
}

func (p *RetryParam) SetRetry(retry int) {
	p.Retry = &retry
}

func (p *RetryParam) IncreaseRetry() {
	if p.resetNextTry {
		p.resetNextTry = false
		return
	}
	if p.Retry == nil {
		p.Retry = new(int)
	}
	*p.Retry++
}

func (p *RetryParam) ResetRetryNextTry() {
	p.resetNextTry = true
}

func (p *RetryParam) ExcludeChannel(channelId int) {
	if channelId <= 0 {
		return
	}
	if p.ExcludedChannelIds == nil {
		p.ExcludedChannelIds = make(map[int]struct{})
	}
	p.ExcludedChannelIds[channelId] = struct{}{}
}

func (p *RetryParam) HasExcludedChannels() bool {
	return len(p.ExcludedChannelIds) > 0
}

func getRandomSatisfiedChannelSkippingSoftCooldown(param *RetryParam, group string, retry int, excludedChannelIds map[int]struct{}) (*model.Channel, map[int]struct{}, time.Time, error) {
	if !operation_setting.IsSoftFailureCooldownEnabled() {
		channel, err := model.GetRandomSatisfiedChannelExcluding(group, param.ModelName, retry, param.RequestPath, param.TokenId, excludedChannelIds)
		return channel, excludedChannelIds, time.Time{}, err
	}

	localExcludedChannelIds := excludedChannelIds
	copiedExclusions := false
	var earliestExpiresAt time.Time
	for {
		channel, err := model.GetRandomSatisfiedChannelExcluding(group, param.ModelName, retry, param.RequestPath, param.TokenId, localExcludedChannelIds)
		if err != nil || channel == nil {
			return channel, localExcludedChannelIds, earliestExpiresAt, err
		}

		entry, cooling := GetChannelSoftCooldown(channel.Id, param.ModelName)
		if !cooling {
			return channel, localExcludedChannelIds, earliestExpiresAt, nil
		}
		if earliestExpiresAt.IsZero() || entry.ExpiresAt.Before(earliestExpiresAt) {
			earliestExpiresAt = entry.ExpiresAt
		}
		if !copiedExclusions {
			localExcludedChannelIds = make(map[int]struct{}, len(excludedChannelIds)+1)
			for channelID := range excludedChannelIds {
				localExcludedChannelIds[channelID] = struct{}{}
			}
			copiedExclusions = true
		}
		localExcludedChannelIds[channel.Id] = struct{}{}
	}
}

// CacheGetRandomSatisfiedChannel tries to get a random channel that satisfies the requirements.
// 尝试获取一个满足要求的随机渠道。
//
// For "auto" tokenGroup with cross-group Retry enabled:
// 对于启用了跨分组重试的 "auto" tokenGroup：
//
//   - Each group will exhaust all its priorities before moving to the next group.
//     每个分组会用完所有优先级后才会切换到下一个分组。
//
//   - Uses ContextKeyAutoGroupIndex to track current group index.
//     使用 ContextKeyAutoGroupIndex 跟踪当前分组索引。
//
//   - Uses ContextKeyAutoGroupRetryIndex to track the global Retry count when current group started.
//     使用 ContextKeyAutoGroupRetryIndex 跟踪当前分组开始时的全局重试次数。
//
//   - priorityRetry = Retry - startRetryIndex, represents the priority level within current group.
//     priorityRetry = Retry - startRetryIndex，表示当前分组内的优先级级别。
//
//   - When GetRandomSatisfiedChannel returns nil (priorities exhausted), moves to next group.
//     当 GetRandomSatisfiedChannel 返回 nil（优先级用完）时，切换到下一个分组。
//
// Example flow (2 groups, each with 2 priorities, RetryTimes=3):
// 示例流程（2个分组，每个有2个优先级，RetryTimes=3）：
//
//	Retry=0: GroupA, priority0 (startRetryIndex=0, priorityRetry=0)
//	         分组A, 优先级0
//
//	Retry=1: GroupA, priority1 (startRetryIndex=0, priorityRetry=1)
//	         分组A, 优先级1
//
//	Retry=2: GroupA exhausted → GroupB, priority0 (startRetryIndex=2, priorityRetry=0)
//	         分组A用完 → 分组B, 优先级0
//
//	Retry=3: GroupB, priority1 (startRetryIndex=2, priorityRetry=1)
//	         分组B, 优先级1
func CacheGetRandomSatisfiedChannel(param *RetryParam) (*model.Channel, string, error) {
	var channel *model.Channel
	var err error
	selectGroup := param.TokenGroup
	userGroup := common.GetContextKeyString(param.Ctx, constant.ContextKeyUserGroup)
	useSoftFailureCooldown := param.UseSoftFailureCooldown && operation_setting.IsSoftFailureCooldownEnabled()
	cooldownExcludedChannelIds := param.ExcludedChannelIds
	var earliestCooldownExpiry time.Time

	if param.TokenGroup == "auto" {
		if len(setting.GetAutoGroups()) == 0 {
			return nil, selectGroup, errors.New("auto groups is not enabled")
		}
		autoGroups := GetUserAutoGroup(userGroup)

		// startGroupIndex: the group index to start searching from
		// startGroupIndex: 开始搜索的分组索引
		startGroupIndex := 0
		crossGroupRetry := common.GetContextKeyBool(param.Ctx, constant.ContextKeyTokenCrossGroupRetry)

		if lastGroupIndex, exists := common.GetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex); exists {
			if idx, ok := lastGroupIndex.(int); ok {
				startGroupIndex = idx
			}
		}

		for i := startGroupIndex; i < len(autoGroups); i++ {
			autoGroup := autoGroups[i]
			// Calculate priorityRetry for current group
			// 计算当前分组的 priorityRetry
			priorityRetry := param.GetRetry()
			// If moved to a new group, reset priorityRetry and update startRetryIndex
			// 如果切换到新分组，重置 priorityRetry 并更新 startRetryIndex
			if i > startGroupIndex {
				priorityRetry = 0
			}
			logger.LogDebug(param.Ctx, "Auto selecting group: %s, priorityRetry: %d", autoGroup, priorityRetry)

			if useSoftFailureCooldown {
				var groupEarliestExpiry time.Time
				channel, cooldownExcludedChannelIds, groupEarliestExpiry, _ = getRandomSatisfiedChannelSkippingSoftCooldown(param, autoGroup, priorityRetry, cooldownExcludedChannelIds)
				if !groupEarliestExpiry.IsZero() && (earliestCooldownExpiry.IsZero() || groupEarliestExpiry.Before(earliestCooldownExpiry)) {
					earliestCooldownExpiry = groupEarliestExpiry
				}
			} else {
				channel, _ = model.GetRandomSatisfiedChannelExcluding(autoGroup, param.ModelName, priorityRetry, param.RequestPath, param.TokenId, param.ExcludedChannelIds)
			}
			if channel == nil {
				// Current group has no available channel for this model, try next group
				// 当前分组没有该模型的可用渠道，尝试下一个分组
				logger.LogDebug(param.Ctx, "No available channel in group %s for model %s at priorityRetry %d, trying next group", autoGroup, param.ModelName, priorityRetry)
				// 重置状态以尝试下一个分组
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex, i+1)
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupRetryIndex, 0)
				// Reset retry counter so outer loop can continue for next group
				// 重置重试计数器，以便外层循环可以为下一个分组继续
				if !param.HasExcludedChannels() {
					param.SetRetry(0)
				}
				continue
			}
			common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroup, autoGroup)
			selectGroup = autoGroup
			logger.LogDebug(param.Ctx, "Auto selected group: %s", autoGroup)

			// Prepare state for next retry
			// 为下一次重试准备状态
			if !param.HasExcludedChannels() && crossGroupRetry && priorityRetry >= common.RetryTimes {
				// Current group has exhausted all retries, prepare to switch to next group
				// This request still uses current group, but next retry will use next group
				// 当前分组已用完所有重试次数，准备切换到下一个分组
				// 本次请求仍使用当前分组，但下次重试将使用下一个分组
				logger.LogDebug(param.Ctx, "Current group %s retries exhausted (priorityRetry=%d >= RetryTimes=%d), preparing switch to next group for next retry", autoGroup, priorityRetry, common.RetryTimes)
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex, i+1)
				// Reset retry counter so outer loop can continue for next group
				// 重置重试计数器，以便外层循环可以为下一个分组继续
				param.SetRetry(0)
				param.ResetRetryNextTry()
			} else {
				// Stay in current group, save current state
				// 保持在当前分组，保存当前状态
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex, i)
			}
			break
		}
	} else {
		if useSoftFailureCooldown {
			channel, cooldownExcludedChannelIds, earliestCooldownExpiry, err = getRandomSatisfiedChannelSkippingSoftCooldown(param, param.TokenGroup, param.GetRetry(), cooldownExcludedChannelIds)
		} else {
			channel, err = model.GetRandomSatisfiedChannelExcluding(param.TokenGroup, param.ModelName, param.GetRetry(), param.RequestPath, param.TokenId, param.ExcludedChannelIds)
		}
		if err != nil {
			return nil, param.TokenGroup, err
		}
	}
	if channel == nil && !earliestCooldownExpiry.IsZero() {
		return nil, selectGroup, &AllChannelsCoolingError{earliestExpiresAt: earliestCooldownExpiry}
	}
	if channel == nil && param.HasExcludedChannels() {
		return nil, selectGroup, ErrNoUntriedChannel
	}
	return channel, selectGroup, nil
}
