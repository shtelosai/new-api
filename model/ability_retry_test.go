package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedAbilityRetryChannel(t *testing.T, id int, modelName string, priority int64) {
	t.Helper()
	weight := uint(30)
	require.NoError(t, DB.Create(&Channel{
		Id:       id,
		Type:     1,
		Name:     "ability-retry-channel",
		Key:      "sk-test",
		Status:   common.ChannelStatusEnabled,
		Models:   modelName,
		Group:    "default",
		Priority: &priority,
		Weight:   &weight,
	}).Error)
	require.NoError(t, DB.Create(&Ability{
		Group:     "default",
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

func TestGetChannelExcludingUsesHighestRemainingPriority(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	modelName := "ability-retry-without-replacement"
	seedAbilityRetryChannel(t, 3241, modelName, 95)
	seedAbilityRetryChannel(t, 3242, modelName, 95)
	seedAbilityRetryChannel(t, 3243, modelName, 10)

	excluded := map[int]struct{}{3241: {}}
	channel, err := GetChannelExcluding("default", modelName, 1, "/v1/messages", excluded)
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3242, channel.Id)

	excluded[3242] = struct{}{}
	channel, err = GetChannelExcluding("default", modelName, 2, "/v1/messages", excluded)
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3243, channel.Id)

	excluded[3243] = struct{}{}
	channel, err = GetChannelExcluding("default", modelName, 3, "/v1/messages", excluded)
	require.NoError(t, err)
	assert.Nil(t, channel)
}

func TestGetChannelExcludingFiltersAdvancedRoutesBeforeSelectingRemainingPriority(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	modelName := "ability-retry-advanced-route"
	seedAbilityRetryChannel(t, 3251, modelName, 95)

	createAdvancedChannel := func(id int, priority int64, incomingPath string) {
		t.Helper()
		weight := uint(30)
		channel := &Channel{
			Id:       id,
			Type:     constant.ChannelTypeAdvancedCustom,
			Name:     "ability-retry-advanced-channel",
			Key:      "sk-test",
			Status:   common.ChannelStatusEnabled,
			Models:   modelName,
			Group:    "default",
			Priority: &priority,
			Weight:   &weight,
		}
		channel.SetOtherSettings(dto.ChannelOtherSettings{
			AdvancedCustom: &dto.AdvancedCustomConfig{
				Routes: []dto.AdvancedCustomRoute{{
					IncomingPath: incomingPath,
					UpstreamPath: incomingPath,
				}},
			},
		})
		require.NoError(t, DB.Create(channel).Error)
		require.NoError(t, DB.Create(&Ability{
			Group:     "default",
			Model:     modelName,
			ChannelId: id,
			Enabled:   true,
			Priority:  &priority,
			Weight:    weight,
		}).Error)
	}

	createAdvancedChannel(3252, 95, "/v1/responses")
	createAdvancedChannel(3253, 10, "/v1/messages")

	channel, err := GetChannelExcluding(
		"default",
		modelName,
		1,
		"/v1/messages",
		map[int]struct{}{3251: {}},
	)

	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3253, channel.Id)
}
