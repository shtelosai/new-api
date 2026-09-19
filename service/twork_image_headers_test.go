package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestTworkImageUpstreamCannotOverwriteVerifiedMetadata(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	common.SetContextKey(c, constant.ContextKeyTworkImageRoute, true)
	for _, header := range []string{"X-Twork-Image-Size", "x-twork-image-channel-id"} {
		assert.False(t, ShouldCopyUpstreamHeader(c, header, []string{"forged"}))
	}
	assert.True(t, ShouldCopyUpstreamHeader(c, "Content-Type", []string{"application/json"}))
}
