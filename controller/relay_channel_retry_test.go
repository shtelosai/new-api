package controller

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetChannelPreservesLastUpstreamErrorWhenCandidatesExhausted(t *testing.T) {
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })

	ctx := newRetryTestContext()
	lastError := types.NewOpenAIError(
		errors.New("upstream stream ended with zero usage and no output"),
		types.ErrorCodeEmptyResponse,
		http.StatusInternalServerError,
	)
	relayInfo := &relaycommon.RelayInfo{
		TokenGroup:      "controller-retry-exhausted",
		UsingGroup:      "controller-retry-exhausted",
		UserGroup:       "controller-retry-exhausted",
		OriginModelName: "controller-retry-model",
		ChannelMeta:     &relaycommon.ChannelMeta{},
		LastError:       lastError,
	}
	retryParam := &service.RetryParam{
		Ctx:         ctx,
		TokenGroup:  relayInfo.TokenGroup,
		ModelName:   relayInfo.OriginModelName,
		RequestPath: "/v1/messages",
		Retry:       common.GetPointer(1),
	}
	retryParam.ExcludeChannel(99)

	channel, gotError := getChannel(ctx, relayInfo, retryParam)

	require.Nil(t, channel)
	require.Same(t, lastError, gotError)
}

func TestGetChannelPreservesLastUpstreamErrorWhenRemainingCandidatesCooling(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ChannelModelDisabled{}))
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalRedisEnabled := common.RedisEnabled
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalHealthSetting := *healthSetting
	model.DB = db
	model.LOG_DB = db
	common.MemoryCacheEnabled = true
	common.RedisEnabled = false
	healthSetting.SoftFailureCooldownEnabled = true
	healthSetting.SoftFailureCooldownSeconds = 30
	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.RedisEnabled = originalRedisEnabled
		*healthSetting = originalHealthSetting
	})

	modelName := "controller-retry-cooling"
	priority := int64(10)
	weight := uint(10)
	require.NoError(t, db.Create(&model.Channel{
		Id: 9401, Type: 1, Name: "cooling-channel", Key: "sk-test", Status: common.ChannelStatusEnabled,
		Models: modelName, Group: "default", Priority: &priority, Weight: &weight,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "default", Model: modelName, ChannelId: 9401, Enabled: true, Priority: &priority, Weight: weight,
	}).Error)
	model.InitChannelCache()
	service.RecordChannelSoftCooldown(nil, 9401, modelName, 529, "overloaded_error")

	ctx := newRetryTestContext()
	lastError := types.NewOpenAIError(
		errors.New("upstream overloaded before retry selection"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	relayInfo := &relaycommon.RelayInfo{
		TokenGroup:      "default",
		UsingGroup:      "default",
		UserGroup:       "default",
		OriginModelName: modelName,
		ChannelMeta:     &relaycommon.ChannelMeta{},
		LastError:       lastError,
	}
	retryParam := &service.RetryParam{
		Ctx:                    ctx,
		TokenGroup:             "default",
		ModelName:              modelName,
		RequestPath:            "/v1/messages",
		Retry:                  common.GetPointer(1),
		UseSoftFailureCooldown: true,
	}

	channel, gotError := getChannel(ctx, relayInfo, retryParam)

	require.Nil(t, channel)
	require.Same(t, lastError, gotError)
}
