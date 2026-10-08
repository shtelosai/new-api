package openai

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkImageResolutionIsForwardedOnlyOnItsDedicatedRoute(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		context, _ := gin.CreateTestContext(nil)
		common.SetContextKey(context, constant.ContextKeyTworkImageRoute, enabled)
		var request dto.ImageRequest
		require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-image-2.5-sunburst","prompt":"杯子","size":"3840x3840","resolution":"4k"}`), &request))
		converted, err := (&Adaptor{}).ConvertImageRequest(context, &relaycommon.RelayInfo{}, request)
		require.NoError(t, err)
		raw, err := common.Marshal(converted)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, common.Unmarshal(raw, &body))
		if enabled {
			assert.Equal(t, "4k", body["resolution"])
		} else {
			assert.NotContains(t, body, "resolution")
		}
		assert.Equal(t, "3840x3840", body["size"])
	}
}
