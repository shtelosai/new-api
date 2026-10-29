package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGPT61ChannelProbeTokenLimit(t *testing.T) {
	for _, name := range []string{"gpt-6.1-sol", "gpt-6.1-terra", "gpt-6.1-terra-2026-10-20-high"} {
		for _, endpoint := range []string{"", string(constant.EndpointTypeOpenAI)} {
			for _, stream := range []bool{false, true} {
				t.Run(name+"/"+endpoint+"/"+common.Interface2String(stream), func(t *testing.T) {
					request := buildTestRequest(name, endpoint, &model.Channel{Type: constant.ChannelTypeOpenAI}, stream)
					chat, ok := request.(*dto.GeneralOpenAIRequest)
					require.True(t, ok)
					require.NotNil(t, chat.MaxCompletionTokens)
					assert.Equal(t, uint(16), *chat.MaxCompletionTokens)
					assert.Nil(t, chat.MaxTokens)
				})
			}
		}
	}
}

func TestGPT61ChannelProbeKeepsProtocolBoundary(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure} {
		t.Run(common.Interface2String(channelType), func(t *testing.T) {
			request := buildTestRequest("gpt-6.1-terra", string(constant.EndpointTypeOpenAI), &model.Channel{Type: channelType}, true)
			chat, ok := request.(*dto.GeneralOpenAIRequest)
			require.True(t, ok)
			require.NotNil(t, chat.MaxCompletionTokens)
			assert.Nil(t, chat.MaxTokens)
			native := buildTestRequest("gpt-6.1-terra", string(constant.EndpointTypeOpenAIResponse), &model.Channel{Type: channelType}, true)
			assert.IsType(t, &dto.OpenAIResponsesRequest{}, native)
		})
	}
	request := buildTestRequest("gpt-6.1-terra", string(constant.EndpointTypeAnthropic), &model.Channel{Type: constant.ChannelTypeAnthropic}, false)
	chat, ok := request.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	assert.Nil(t, chat.MaxCompletionTokens)
	require.NotNil(t, chat.MaxTokens)
}
