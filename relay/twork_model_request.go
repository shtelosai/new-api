package relay

import (
	"reflect"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

// 模型级请求按本次渠道重新生成；原生 Gemini 等渠道继续使用原来的协议转换器。
func buildTworkModelUpstreamRequest(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor) ([]byte, error) {
	data, err := relaycommon.BuildTworkModelRequest(c, info)
	if err != nil {
		return nil, err
	}
	var converted any
	var before []byte
	if info.ChannelSetting.TworkWireAPI == "chat_completions" {
		var request dto.GeneralOpenAIRequest
		if err = common.Unmarshal(data, &request); err != nil {
			return nil, err
		}
		before, err = common.Marshal(request)
		if err != nil {
			return nil, err
		}
		converted, err = adaptor.ConvertOpenAIRequest(c, info, &request)
	} else {
		var request dto.OpenAIResponsesRequest
		if err = common.Unmarshal(data, &request); err != nil {
			return nil, err
		}
		before, err = common.Marshal(request)
		if err != nil {
			return nil, err
		}
		converted, err = adaptor.ConvertOpenAIResponsesRequest(c, info, request)
	}
	if err != nil {
		return nil, err
	}
	relaycommon.AppendRequestConversionFromRequest(info, converted)
	convertedData, err := common.Marshal(converted)
	if err != nil {
		return nil, err
	}
	if info.ApiType == constant.APITypeOpenAI {
		data, err = mergeTworkOpenAIConversion(data, before, convertedData)
		if err != nil {
			return nil, err
		}
	} else {
		data = convertedData
	}
	data, err = relaycommon.RemoveDisabledFields(data, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
	if err != nil {
		return nil, err
	}
	return relaycommon.ApplyParamOverrideWithRelayInfo(data, info)
}

// OpenAI 适配器仍处理模型后缀等既有规则；只合并它实际修改的字段，保留原始扩展参数。
func mergeTworkOpenAIConversion(raw, before, after []byte) ([]byte, error) {
	var body, previous, converted map[string]any
	for _, entry := range []struct {
		data   []byte
		target *map[string]any
	}{{raw, &body}, {before, &previous}, {after, &converted}} {
		if err := common.Unmarshal(entry.data, entry.target); err != nil {
			return nil, err
		}
	}
	for key := range previous {
		if _, present := converted[key]; !present {
			delete(body, key)
		}
	}
	for key, value := range converted {
		if !reflect.DeepEqual(previous[key], value) {
			body[key] = value
		}
	}
	return common.Marshal(body)
}
