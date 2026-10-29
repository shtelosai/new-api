package openai

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPT61ChatWireParameters(t *testing.T) {
	for _, tc := range []struct {
		name, upstream, effort, wantModel, wantEffort string
		compatible, sampling                          bool
	}{
		{"Sol default", "gpt-6.1-sol", "", "gpt-6.1-sol", "", true, false},
		{"Sol none still drops sampling", "gpt-6.1-sol", "none", "gpt-6.1-sol", "none", true, false},
		{"Terra default", "gpt-6.1-terra", "", "gpt-6.1-terra", "", true, false},
		{"Terra none", "gpt-6.1-terra", "none", "gpt-6.1-terra", "none", true, true},
		{"Terra high", "gpt-6.1-terra", "high", "gpt-6.1-terra", "high", true, false},
		{"none suffix overrides effort", "gpt-6.1-terra-none", "high", "gpt-6.1-terra", "none", true, true},
		{"high suffix overrides none", "gpt-6.1-terra-high", "none", "gpt-6.1-terra", "high", true, false},
		{"max suffix", "gpt-6.1-terra-max", "", "gpt-6.1-terra", "max", true, false},
		{"dated Sol", "gpt-6.1-sol-2026-10-20", "none", "gpt-6.1-sol-2026-10-20", "none", true, false},
		{"dated Terra suffix", "gpt-6.1-terra-2026-10-20-none", "", "gpt-6.1-terra-2026-10-20", "none", true, true},
		{"invalid date", "gpt-6.1-terra-2026-02-30", "", "gpt-6.1-terra-2026-02-30", "", false, true},
		{"unknown named variant", "gpt-6.1-terra-codex", "high", "gpt-6.1-terra-codex", "high", false, true},
		{"unmapped supplier alias", "gpt-6.1-sol-self", "", "gpt-6.1-sol-self", "", false, true},
		{"future model", "gpt-6.2-terra", "high", "gpt-6.2-terra", "high", false, true},
		{"GPT6 unchanged", "gpt-6-astra", "", "gpt-6-astra", "", false, true},
		{"GPT4 unchanged", "gpt-4o", "", "gpt-4o", "", false, true},
		{"alias mapped away from GPT61", "qwen-turbo", "none", "qwen-turbo", "none", false, true},
	} {
		for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAzure} {
			t.Run(tc.name+"/"+common.Interface2String(channelType), func(t *testing.T) {
				request := &dto.GeneralOpenAIRequest{
					Model: tc.upstream, ReasoningEffort: tc.effort,
					MaxTokens:   common.GetPointer(uint(16)),
					Temperature: common.GetPointer(0.0), TopP: common.GetPointer(0.0), LogProbs: common.GetPointer(false), TopLogProbs: common.GetPointer(0),
					Messages: []dto.Message{{Role: "system", Content: "policy"}, {Role: "user", Content: "hello"}},
				}
				info := &relaycommon.RelayInfo{OriginModelName: "gpt-6.1-terra-public", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channelType, UpstreamModelName: tc.upstream}}
				converted, err := (&Adaptor{ChannelType: channelType}).ConvertOpenAIRequest(nil, info, request)
				require.NoError(t, err)
				encoded, err := common.Marshal(converted)
				require.NoError(t, err)
				var wire map[string]any
				require.NoError(t, common.Unmarshal(encoded, &wire))
				assert.Equal(t, tc.wantModel, wire["model"])
				if tc.wantEffort == "" {
					assert.NotContains(t, wire, "reasoning_effort")
				} else {
					assert.Equal(t, tc.wantEffort, wire["reasoning_effort"])
				}
				for _, key := range []string{"temperature", "top_p", "logprobs", "top_logprobs"} {
					if tc.sampling {
						assert.Contains(t, wire, key)
						if key == "logprobs" {
							assert.Equal(t, false, wire[key])
						} else {
							assert.Equal(t, float64(0), wire[key])
						}
					} else {
						assert.NotContains(t, wire, key)
					}
				}
				role := "system"
				limit := "max_tokens"
				if tc.compatible {
					role = "developer"
					limit = "max_completion_tokens"
					assert.NotContains(t, wire, "max_tokens")
				}
				assert.Equal(t, float64(16), wire[limit])
				assert.Equal(t, role, wire["messages"].([]any)[0].(map[string]any)["role"])
			})
		}
	}
}

func TestGPT61ChatDoesNotInventTokenLimit(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{Model: "gpt-6.1-sol"}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: request.Model}}
	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)
	require.NoError(t, err)
	result := converted.(*dto.GeneralOpenAIRequest)
	assert.Nil(t, result.MaxTokens)
	assert.Nil(t, result.MaxCompletionTokens)
}

func TestGPT61PatchKeepsLegacyModelRules(t *testing.T) {
	for _, model := range []string{"gpt-5.1", "o3-mini"} {
		t.Run(model, func(t *testing.T) {
			request := &dto.GeneralOpenAIRequest{Model: model, ReasoningEffort: "none", MaxTokens: common.GetPointer(uint(16)), Temperature: common.GetPointer(0.0), TopP: common.GetPointer(0.0), LogProbs: common.GetPointer(false), TopLogProbs: common.GetPointer(0)}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: model}}
			_, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)
			require.NoError(t, err)
			assert.Nil(t, request.Temperature)
			require.NotNil(t, request.MaxCompletionTokens)
			if model == "o3-mini" {
				assert.NotNil(t, request.TopP)
				assert.NotNil(t, request.LogProbs)
			} else {
				assert.Nil(t, request.TopP)
				assert.Nil(t, request.LogProbs)
			}
		})
	}
}

func TestGPT61ChatPreservesExplicitTokenLimit(t *testing.T) {
	for _, limit := range []uint{0, 32} {
		t.Run(common.Interface2String(limit), func(t *testing.T) {
			request := &dto.GeneralOpenAIRequest{Model: "gpt-6.1-terra", MaxTokens: common.GetPointer(uint(16)), MaxCompletionTokens: common.GetPointer(limit)}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: request.Model}}
			result, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)
			require.NoError(t, err)
			wire, err := common.Marshal(result)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.Unmarshal(wire, &body))
			assert.Equal(t, float64(limit), body["max_completion_tokens"])
			assert.NotContains(t, body, "max_tokens")
		})
	}
}
