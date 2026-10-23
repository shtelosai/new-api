package relay

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
)

func TestConvertRequestErrorSeparatesUnsupportedProtocolFromBadRequest(t *testing.T) {
	unsupported := convertRequestError(errors.New("not implemented"))
	assert.Equal(t, types.ErrorCodeChannelUnsupportedProtocol, unsupported.GetErrorCode())
	assert.True(t, types.IsChannelError(unsupported), "换个渠道就可能成功")

	bad := convertRequestError(errors.New("messages must not be empty"))
	assert.Equal(t, types.ErrorCodeConvertRequestFailed, bad.GetErrorCode())
	assert.True(t, types.IsSkipRetryError(bad), "请求本身的问题换渠道也一样")
}
