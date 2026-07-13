package controller

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
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
