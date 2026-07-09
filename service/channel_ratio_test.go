package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func float64Ptr(value float64) *float64 {
	return &value
}

func TestChannelGetRatioDefaultsAndAllowsFreeChannel(t *testing.T) {
	positive := 1.5
	zero := 0.0
	negative := -0.5

	assert.Equal(t, 1.0, (&model.Channel{}).GetRatio())
	assert.Equal(t, positive, (&model.Channel{Ratio: &positive}).GetRatio())
	assert.Equal(t, 0.0, (&model.Channel{Ratio: &zero}).GetRatio())
	assert.Equal(t, 1.0, (&model.Channel{Ratio: &negative}).GetRatio())
}

func TestGenerateTextOtherInfoChannelRatioVisibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	now := time.Now()

	cases := []struct {
		name        string
		channelMeta *relaycommon.ChannelMeta
		wantValue   any
	}{
		{
			name: "positive non-default ratio is logged",
			channelMeta: &relaycommon.ChannelMeta{
				ChannelRatio: float64Ptr(1.5),
			},
			wantValue: 1.5,
		},
		{
			name: "default ratio is omitted",
			channelMeta: &relaycommon.ChannelMeta{
				ChannelRatio: float64Ptr(1),
			},
		},
		{
			name: "free channel ratio is omitted",
			channelMeta: &relaycommon.ChannelMeta{
				ChannelRatio: float64Ptr(0),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relayInfo := &relaycommon.RelayInfo{
				StartTime:         now,
				FirstResponseTime: now.Add(150 * time.Millisecond),
				ChannelMeta:       tc.channelMeta,
			}

			other := GenerateTextOtherInfo(ctx, relayInfo, 2, 3, 4, 0, 1, 0, 0)
			if tc.wantValue == nil {
				assert.NotContains(t, other, "channel_ratio")
				return
			}
			require.Contains(t, other, "channel_ratio")
			assert.Equal(t, tc.wantValue, other["channel_ratio"])
		})
	}
}

func TestCalculateAudioQuotaAppliesChannelRatioForPriceBilling(t *testing.T) {
	quota, clamp := calculateAudioQuota(QuotaInfo{
		UsePrice:     true,
		ModelPrice:   2,
		GroupRatio:   3,
		ChannelRatio: 1.5,
	})

	require.Nil(t, clamp)
	assert.Equal(t, int(2*3*1.5*common.QuotaPerUnit), quota)
}

func TestCalculateAudioQuotaAllowsFreeChannelRatio(t *testing.T) {
	quota, clamp := calculateAudioQuota(QuotaInfo{
		InputDetails: TokenDetails{
			TextTokens: 100,
		},
		ModelName:    "free-channel-audio",
		ModelRatio:   2,
		GroupRatio:   3,
		ChannelRatio: 0,
	})

	require.Nil(t, clamp)
	assert.Equal(t, 0, quota)
}
