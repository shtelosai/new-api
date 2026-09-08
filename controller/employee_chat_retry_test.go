package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestEmployeeChatSpecificChannelNeverRetriesChannelErrors(t *testing.T) {
	c := newRetryTestContext()
	c.Set("specific_channel_id", "168")
	err := types.InitOpenAIError(types.ErrorCodeChannelNoAvailableKey, http.StatusServiceUnavailable)
	require.False(t, shouldRetry(c, err, 3))
}
