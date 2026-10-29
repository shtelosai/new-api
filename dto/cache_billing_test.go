package dto

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCacheBillingDetailsBoundAndClone(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		total, alias, legacy, ttl, want, wantTTL int
	}{
		{"aliases take max", 10, 30, 20, 25, 30, 25},
		{"negative counts", -1, -2, -3, -4, 0, 0},
		{"12h capped at total", 10, 0, 0, 50, 10, 10},
		{"split alone cannot invent total", 0, 0, 0, 50, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := InputTokenDetails{CacheWriteTokens: tc.total, CacheCreationInputTokens: tc.alias, CachedCreationTokens: tc.legacy, CacheWriteTokensDetails: &CacheWriteTokenDetails{TTL12hTokens: tc.ttl}}
			assert.Equal(t, tc.want, d.CacheCreationTokensTotal())
			assert.Equal(t, tc.wantTTL, d.CacheWrite12hTokens())
		})
	}
	usage := &Usage{PromptTokensDetails: InputTokenDetails{CacheWriteTokens: 7, CacheWriteTokensDetails: &CacheWriteTokenDetails{TTL12hTokens: 4}}}
	copy := CloneBillingUsage(NewOpenAIChatBillingUsage(usage))
	require.NotNil(t, copy)
	usage.PromptTokensDetails.CacheWriteTokensDetails.TTL12hTokens = 6
	assert.Equal(t, 4, copy.OpenAIUsage.PromptTokensDetails.CacheWrite12hTokens())
	later := InputTokenDetails{CacheWriteTokens: 9, CacheWriteTokensDetails: &CacheWriteTokenDetails{TTL30mTokens: 5}}
	later.InheritCacheWrites(copy.OpenAIUsage.PromptTokensDetails)
	assert.Equal(t, 9, later.CacheCreationTokensTotal(), "后续累计正值覆盖旧值而非相加")
	assert.Equal(t, 4, later.CacheWrite12hTokens(), "后续缺省 TTL 不能抹掉旧值")
	copy.OpenAIUsage.PromptTokensDetails.CacheWriteTokensDetails.TTL12hTokens = 1
	assert.Equal(t, 4, later.CacheWrite12hTokens(), "合并后的 TTL 不共享旧指针")
	var req GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-test","prompt_cache_options":{"ttl":"12h"}}`), &req))
	wire, err := common.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"prompt_cache_options":{"ttl":"12h"}`)
}
