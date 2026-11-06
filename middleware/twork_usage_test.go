package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureTworkUsage(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, turn, session, aux string
		want                     common.TworkUsage
	}{
		{name: "legacy"},
		{"reply", hash, hash, "", common.TworkUsage{Turn: hash, Session: hash}},
		{"turn wins", hash, hash, "title", common.TworkUsage{Turn: hash, Session: hash}},
		{"invalid turn ignored", "invalid", hash, "memory", common.TworkUsage{Session: hash, Aux: "memory"}},
		{"uppercase", strings.ToUpper(hash), strings.ToUpper(hash), "TITLE", common.TworkUsage{}},
		{"short", hash[:31], hash[:31], "unknown", common.TworkUsage{}},
		{"long", hash + "0", hash + "0", "title,memory", common.TworkUsage{}},
		{"nonhex", strings.Repeat("g", 32), strings.Repeat("g", 32), "", common.TworkUsage{}},
		{"whitespace", " " + hash, hash + " ", " title", common.TworkUsage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			c.Request.Header.Set("X-Twork-Turn", tc.turn)
			c.Request.Header.Set("X-Twork-Session", tc.session)
			c.Request.Header.Set("X-Twork-Aux", tc.aux)
			CaptureTworkUsage(c)
			assert.Equal(t, tc.want, common.GetTworkUsage(c))
			assert.False(t, c.IsAborted())
			assert.Equal(t, tc.session, c.GetHeader("X-Twork-Session"), "logging must not change affinity inputs")
		})
	}
	for _, aux := range []string{"title", "composer_prediction", "memory", "prompt_enhance", "role_prompt", "one_shot", "kb_import", "avatar", "minutes", "group_chat", "lingshi"} {
		t.Run(aux, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/images/edits", nil)
			c.Request.Header.Set("X-Twork-Session", hash)
			c.Request.Header.Set("X-Twork-Aux", aux)
			CaptureTworkUsage(c)
			require.Equal(t, common.TworkUsage{Session: hash, Aux: aux}, common.GetTworkUsage(c))
		})
	}
	t.Run("duplicate values", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		for key, value := range map[string]string{"X-Twork-Turn": hash, "X-Twork-Session": hash, "X-Twork-Aux": "title"} {
			c.Request.Header.Add(key, value)
			c.Request.Header.Add(key, value)
		}
		CaptureTworkUsage(c)
		assert.Equal(t, common.TworkUsage{}, common.GetTworkUsage(c))
	})
}
