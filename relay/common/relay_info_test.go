package common

import (
	"net/http"
	"net/http/httptest"
	"testing"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoGetFinalRequestRelayFormatPrefersExplicitFinal(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		RequestConversionChain:  []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		FinalRequestRelayFormat: types.RelayFormatOpenAIResponses,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToConversionChain(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToRelayFormat(t *testing.T) {
	info := &RelayInfo{
		RelayFormat: types.RelayFormatGemini,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatGemini), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatNilReceiver(t *testing.T) {
	var info *RelayInfo
	require.Equal(t, types.RelayFormat(""), info.GetFinalRequestRelayFormat())
}

func TestChannelMetaGetChannelRatioDefaultsAndAllowsZero(t *testing.T) {
	var nilMeta *ChannelMeta
	negative := -0.5
	zero := 0.0
	positive := 1.5

	require.Equal(t, 1.0, nilMeta.GetChannelRatio())
	require.Equal(t, 1.0, (&ChannelMeta{}).GetChannelRatio())
	require.Equal(t, 1.0, (&ChannelMeta{ChannelRatio: &negative}).GetChannelRatio())
	require.Equal(t, 0.0, (&ChannelMeta{ChannelRatio: &zero}).GetChannelRatio())
	require.Equal(t, 1.5, (&ChannelMeta{ChannelRatio: &positive}).GetChannelRatio())
}

func TestInitChannelMetaReadsChannelRatioFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name      string
		input     float64
		wantRatio float64
	}{
		{name: "positive", input: 1.5, wantRatio: 1.5},
		{name: "free", input: 0, wantRatio: 0},
		{name: "negative fallback", input: -0.5, wantRatio: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			rootcommon.SetContextKey(ctx, constant.ContextKeyChannelRatio, tc.input)

			info := &RelayInfo{}
			info.InitChannelMeta(ctx)

			require.NotNil(t, info.ChannelMeta)
			require.Equal(t, tc.wantRatio, info.ChannelMeta.GetChannelRatio())
		})
	}
}
