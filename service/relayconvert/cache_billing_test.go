package relayconvert

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCacheBillingSurvivesProtocolConversions(t *testing.T) {
	var chat dto.OpenAITextResponse
	require.NoError(t, common.Unmarshal([]byte(`{"id":"cache","model":"test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":4000,"cache_creation_input_tokens":7000,"cache_write_tokens_details":{"ttl_30m_tokens":3000,"ttl_12h_tokens":4000}}}}`), &chat))
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAIResponses} {
		t.Run(string(format), func(t *testing.T) {
			converted, err := ConvertResponse(nil, &relaycommon.RelayInfo{}, format, &chat)
			require.NoError(t, err)
			if format == types.RelayFormatOpenAIResponses {
				response := converted.Value.(*dto.OpenAIResponsesResponse)
				require.NotNil(t, response.Usage.InputTokensDetails)
				assert.Equal(t, 4000, response.Usage.InputTokensDetails.CacheWrite12hTokens())
			}
			back, err := ConvertResponse(nil, &relaycommon.RelayInfo{}, types.RelayFormatOpenAI, converted.Value)
			require.NoError(t, err)
			require.NotNil(t, back.Usage.BillingUsage)
			details := back.Usage.BillingUsage.OpenAIUsage.PromptTokensDetails
			assert.Equal(t, 7000, details.CacheCreationTokensTotal())
			assert.Equal(t, 4000, details.CacheWrite12hTokens())
		})
	}
}

func TestCacheBillingRequestTTLForwarding(t *testing.T) {
	var chat dto.GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"test","messages":[{"role":"user","content":"hello"}],"prompt_cache_options":{"ttl":"12h"}}`), &chat))
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "test"}}
	response, err := ConvertRequest(nil, info, types.RelayFormatOpenAIResponses, &chat)
	require.NoError(t, err)
	back, err := ConvertRequest(nil, info, types.RelayFormatOpenAI, response.Value)
	require.NoError(t, err)
	for _, value := range []any{response.Value, back.Value} {
		wire, err := common.Marshal(value)
		require.NoError(t, err)
		assert.Contains(t, string(wire), `"prompt_cache_options":{"ttl":"12h"}`)
	}
}
