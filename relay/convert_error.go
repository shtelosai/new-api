package relay

import (
	"strings"

	"github.com/QuantumNous/new-api/types"
)

// convertRequestError 区分「渠道不支持这种协议」与请求本身转换失败：前者换个渠道就可能成功，后者换渠道也一样。
func convertRequestError(err error) *types.NewAPIError {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not implemented") {
		return types.NewError(err, types.ErrorCodeChannelUnsupportedProtocol)
	}
	return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
}
