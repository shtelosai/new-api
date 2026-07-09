package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldDisableChannelSkipsEmptyResponseStatusCodeConfig(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	origRanges := operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 500, End: 599}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = origRanges
	})

	emptyStreamErr := types.NewError(errors.New("upstream stream ended without any content"), types.ErrorCodeEmptyResponse)
	ordinaryErr := types.NewErrorWithStatusCode(errors.New("ordinary upstream failure"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)

	require.Equal(t, http.StatusInternalServerError, emptyStreamErr.StatusCode)
	assert.False(t, ShouldDisableChannel(emptyStreamErr))
	assert.True(t, ShouldDisableChannel(ordinaryErr))
}
