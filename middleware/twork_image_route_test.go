package middleware

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTworkImageDeadlineBoundsAndCancelsUpstreamContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		valid  bool
	}{
		{"default", nil, true}, {"remaining", []string{"5"}, true},
		{"negative", []string{"-1"}, false}, {"zero", []string{"0"}, false},
		{"over total", []string{"280001"}, false}, {"not integer", []string{"abc"}, false},
		{"duplicate", []string{"5", "10"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
			if tc.values != nil {
				c.Request.Header["X-Twork-Image-Timeout-Ms"] = tc.values
			}
			cancel, err := tworkImageDeadline(c)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			defer cancel()
			require.Empty(t, c.Request.Header.Get("X-Twork-Image-Timeout-Ms"))
			deadline, ok := c.Request.Context().Deadline()
			require.True(t, ok)
			require.LessOrEqual(t, time.Until(deadline), 280*time.Second)
			if tc.name == "remaining" {
				select {
				case <-c.Request.Context().Done():
					require.ErrorIs(t, c.Request.Context().Err(), context.DeadlineExceeded)
				case <-time.After(time.Second):
					t.Fatal("剩余预算没有取消上游上下文")
				}
			}
		})
	}
}
