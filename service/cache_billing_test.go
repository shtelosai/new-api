package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCalculateTextQuotaSummaryBillsCacheCreationInputTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "cache-creation-input",
		PriceData: types.PriceData{
			ModelRatio:         1,
			CompletionRatio:    1,
			CacheRatio:         0.1,
			CacheCreationRatio: 1.25,
			GroupRatioInfo:     types.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	t.Run("creation tokens billed at creation ratio", func(t *testing.T) {
		var usage dto.Usage
		require.NoError(t, common.Unmarshal([]byte(`{"prompt_tokens":1510,"completion_tokens":20,"total_tokens":1530,"prompt_tokens_details":{"cached_tokens":1200,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":300}}}`), &usage))
		summary := calculateTextQuotaSummary(ctx, relayInfo, &usage)

		assert.Equal(t, 1510, summary.PromptTokens)
		assert.Equal(t, 1200, summary.CacheTokens)
		assert.Equal(t, 300, summary.CacheCreationTokens)
		assert.Zero(t, summary.CacheCreationTokens5m)
		assert.Zero(t, summary.CacheCreationTokens1h)
		// (1510-1200-300) + 1200*0.1 + 300*1.25 + 20 = 525
		assert.Equal(t, 525, summary.Quota)
	})

	t.Run("missing creation field stays in prompt price", func(t *testing.T) {
		var usage dto.Usage
		require.NoError(t, common.Unmarshal([]byte(`{"prompt_tokens":1510,"completion_tokens":20,"total_tokens":1530,"prompt_tokens_details":{"cached_tokens":1200}}`), &usage))
		summary := calculateTextQuotaSummary(ctx, relayInfo, &usage)

		assert.Zero(t, summary.CacheCreationTokens)
		// (1510-1200) + 1200*0.1 + 20 = 450
		assert.Equal(t, 450, summary.Quota)
	})

	t.Run("cache write and creation scalar take the larger count", func(t *testing.T) {
		var usage dto.Usage
		require.NoError(t, common.Unmarshal([]byte(`{"prompt_tokens":1510,"completion_tokens":20,"total_tokens":1530,"prompt_tokens_details":{"cached_tokens":1200,"cache_write_tokens":100,"cache_creation_input_tokens":300}}`), &usage))
		summary := calculateTextQuotaSummary(ctx, relayInfo, &usage)

		assert.Equal(t, 300, summary.CacheCreationTokens)
		assert.Zero(t, summary.CacheCreationTokens5m)
		assert.Zero(t, summary.CacheCreationTokens1h)
		assert.Equal(t, 525, summary.Quota)
	})
}

// OpenAI 12h cache writes bill at twice the write price and keep their own
// count in the consume log, with ratio and expression pricing alike.
func TestBuildTieredTokenParams_ClaudeUnsplitCacheWrites(t *testing.T) {
	expr := `tier("base", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6)`
	for _, tc := range []struct {
		name             string
		split5m, split1h int
		wantCC, wantCC1h float64
	}{
		{"total without split", 0, 0, 10000, 0},
		{"split missing part of total", 0, 4000, 6000, 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &dto.Usage{
				PromptTokens:                100,
				CompletionTokens:            50,
				UsageSemantic:               "anthropic",
				PromptTokensDetails:         dto.InputTokenDetails{CachedCreationTokens: 10000},
				ClaudeCacheCreation5mTokens: tc.split5m,
				ClaudeCacheCreation1hTokens: tc.split1h,
			}
			params := BuildTieredTokenParams(usage, true, billingexpr.UsedVars(expr))
			assert.Equal(t, tc.wantCC, params.CC)
			assert.Equal(t, tc.wantCC1h, params.CC1h)
			assert.Equal(t, float64(100+10000), params.Len, "len counts every cache write")
		})
	}
}

func TestBuildTieredTokenParams_UnpricedLongTTLCacheWrites(t *testing.T) {
	// The OpenAI prompt caching guide example: 12000 input tokens with 4000
	// cache reads and 7000 writes, 3000 of them 30m and 4000 12h.
	openai := dto.Usage{PromptTokens: 12000, PromptTokensDetails: dto.InputTokenDetails{
		CachedTokens: 4000, CacheWriteTokens: 7000,
		CacheWriteTokensDetails: &dto.CacheWriteTokenDetails{TTL30mTokens: 3000, TTL12hTokens: 4000},
	}}
	unsplit := openai
	unsplit.PromptTokensDetails.CacheWriteTokensDetails = nil
	claude := func(split5m, split1h int) dto.Usage {
		return dto.Usage{PromptTokens: 100, UsageSemantic: "anthropic",
			ClaudeCacheCreation5mTokens: split5m, ClaudeCacheCreation1hTokens: split1h,
			PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: split5m + split1h}}
	}
	for _, tc := range []struct {
		name               string
		usage              dto.Usage
		expr               string
		p, cc, cc1h, cc12h float64
		cost               float64
	}{
		{"openai 30m and 12h priced", openai, `tier("base", p * 10 + cr * 1 + cc * 12.5 + cc12h * 25)`, 1000, 3000, 0, 4000, 151500},
		{"openai 12h joins cc at twice its count", openai, `tier("base", p * 10 + cr * 1 + cc * 12.5)`, 1000, 11000, 0, 0, 151500},
		{"openai 30m stays in p", openai, `tier("base", p * 10 + cr * 1 + cc12h * 25)`, 4000, 0, 0, 4000, 144000},
		{"openai 12h bills twice in p", openai, `tier("base", p * 10 + cr * 1)`, 12000, 0, 0, 0, 124000},
		{"openai writes without a ttl split", unsplit, `tier("base", p * 10 + cr * 1 + cc * 12.5 + cc12h * 25)`, 1000, 7000, 0, 0, 101500},
		{"claude 1h joins cc at 1.6x", claude(0, 10000), `tier("base", p * 3 + c * 15 + cr * 0.3 + cc * 3.75)`, 100, 16000, 0, 0, 60300},
		{"claude 5m bills as input", claude(10000, 0), `tier("base", p * 3 + c * 15)`, 10100, 0, 0, 0, 30300},
		{"claude 5m bills as input beside priced 1h", claude(2000, 10000), `tier("base", p * 3 + cc1h * 6)`, 2100, 0, 10000, 0, 66300},
		{"claude 1h bills as input at 1.6x", claude(2000, 10000), `tier("base", p * 3)`, 18100, 0, 0, 0, 54300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isClaude := tc.usage.UsageSemantic == "anthropic"
			params := BuildTieredTokenParams(&tc.usage, isClaude, billingexpr.UsedVars(tc.expr))
			assert.Equal(t, tc.p, params.P)
			assert.Equal(t, tc.cc, params.CC)
			assert.Equal(t, tc.cc1h, params.CC1h)
			assert.Equal(t, tc.cc12h, params.CC12h)
			wantLen := float64(tc.usage.PromptTokens)
			if isClaude {
				wantLen += float64(tc.usage.PromptTokensDetails.CacheCreationTokensTotal())
			}
			assert.Equal(t, wantLen, params.Len, "len keeps the unweighted counts")
			cost, _, err := billingexpr.RunExpr(tc.expr, params)
			require.NoError(t, err)
			assert.Equal(t, tc.cost, cost)
		})
	}
}
