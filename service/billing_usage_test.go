package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service/relayconvert"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesConvertedUsagePreservesCacheDiscountAndChannelRatio(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ratio float64
		quota int
	}{
		{name: "默认倍率", ratio: 1, quota: 90},
		{name: "定制倍率", ratio: 1.5, quota: 135},
		{name: "免费渠道", ratio: 0, quota: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			info := &relaycommon.RelayInfo{
				RelayFormat:     types.RelayFormatOpenAI,
				OriginModelName: "gpt-4o",
				StartTime:       time.Now(),
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelRatio: &tc.ratio},
				PriceData: types.PriceData{
					ModelRatio: 1, CompletionRatio: 2, CacheRatio: 0.25,
					GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
				},
			}
			converted, err := relayconvert.ConvertResponse(ctx, info, types.RelayFormatOpenAI, &dto.OpenAIResponsesResponse{
				ID: "resp_cache", Model: "gpt-4o", Status: []byte(`"completed"`),
				Usage: &dto.Usage{
					InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
					InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 40},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, converted.Usage)
			require.NotNil(t, converted.Usage.BillingUsage)
			snapshot := dto.CloneBillingUsage(converted.Usage.BillingUsage)

			summary := calculateTextQuotaSummary(ctx, info, effectiveBillingUsage(converted.Usage))

			assert.Equal(t, 40, summary.CacheTokens)
			// (60 普通输入 + 40 × 0.25 缓存输入 + 10 × 2 输出) × 渠道倍率。
			assert.Equal(t, tc.quota, summary.Quota)
			assert.Equal(t, snapshot, converted.Usage.BillingUsage, "结算不能修改上游用量快照")
		})
	}
}

func TestOpenAIBillingUsageFillsMissingDetailsWithoutOverwritingCanonicalValues(t *testing.T) {
	billingUsage := dto.NewOpenAIResponsesBillingUsage(&dto.Usage{
		InputTokens: 100, OutputTokens: 10, PromptCacheHitTokens: 55,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8, TextTokens: 12},
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens: 40, CachedCreationTokens: 5, CacheWriteTokens: 6,
			TextTokens: 60, ImageTokens: 7, AudioTokens: 9,
		},
	})
	snapshot := dto.CloneBillingUsage(billingUsage)

	usage := effectiveBillingUsage(&dto.Usage{BillingUsage: billingUsage})

	assert.Equal(t, dto.InputTokenDetails{
		CachedTokens: 8, CachedCreationTokens: 5, CacheWriteTokens: 6,
		TextTokens: 12, ImageTokens: 7, AudioTokens: 9,
	}, usage.PromptTokensDetails)
	assert.Equal(t, snapshot, billingUsage)
}

func TestOpenAIBillingUsageCacheFallbackDoesNotInventUsage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details *dto.InputTokenDetails
		hit     int
		want    int
	}{
		{name: "兼容缓存字段", hit: 35, want: 35},
		{name: "Responses明细优先", details: &dto.InputTokenDetails{CachedTokens: 40}, hit: 35, want: 40},
		{name: "缺少缓存明细"},
		{name: "空缓存明细", details: &dto.InputTokenDetails{}},
		{name: "负缓存不补入", details: &dto.InputTokenDetails{CachedTokens: -1}, hit: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := effectiveBillingUsage(&dto.Usage{BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{
				PromptTokens: 100, CompletionTokens: 10,
				InputTokensDetails: tc.details, PromptCacheHitTokens: tc.hit,
			})})
			assert.Equal(t, tc.want, usage.PromptTokensDetails.CachedTokens)
		})
	}
}
