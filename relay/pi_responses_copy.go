package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/QuantumNous/new-api/dto"
	"github.com/jinzhu/copier"
)

// copyPiResponsesRequest 保持请求副本隔离，原始 JSON 按字节块复制，避免逐字节反射。
func copyPiResponsesRequest(src *dto.OpenAIResponsesRequest) (*dto.OpenAIResponsesRequest, error) {
	if src == nil {
		return nil, fmt.Errorf("copy source cannot be nil")
	}
	var dst dto.OpenAIResponsesRequest
	err := copier.CopyWithOption(&dst, src, copier.Option{DeepCopy: true, IgnoreEmpty: true, Converters: []copier.TypeConverter{{SrcType: json.RawMessage{}, DstType: json.RawMessage{}, Fn: func(src interface{}) (interface{}, error) {
		return json.RawMessage(bytes.Clone(src.(json.RawMessage))), nil
	}}}})
	if err != nil {
		return nil, err
	}
	return &dst, nil
}
