package controller

import (
	"errors"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExplicitRuntimeRouteNeverRetriesChannelErrors(t *testing.T) {
	c := newRetryTestContext()
	c.Set("twork_explicit_channel_route", true)
	c.Set("specific_channel_id", "4301")
	for _, code := range []types.ErrorCode{types.ErrorCodeChannelNoAvailableKey, types.ErrorCodeChannelModelMappedError, types.ErrorCodeGetChannelFailed} {
		assert.False(t, shouldRetry(c, types.NewError(errors.New("upstream failed"), code), 3))
	}
}

func TestExplicitRuntimeRouteKeepsSelectedChannelAfterRelayInitialization(t *testing.T) {
	c := newRetryTestContext()
	c.Set("twork_explicit_channel_route", true)
	c.Set("channel_id", 4301)
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
	channel, err := getChannel(c, info, nil)
	require.Nil(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 4301, channel.Id)
}
