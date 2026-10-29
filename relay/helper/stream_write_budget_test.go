package helper

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func TestStreamWriteBudget(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var budget StreamWriteBudget
	steps := []struct {
		name  string
		at    time.Duration
		bytes int
		want  time.Duration
	}{
		{"small event", 0, 8 << 10, 31 * time.Second},
		{"image event gets the time a slow client needs", 0, 3600 << 10, 481 * time.Second},
		{"next write keeps the image budget", 60 * time.Second, 8 << 10, 482 * time.Second},
		{"budget starts over once it ran out", 600 * time.Second, 8 << 10, 631 * time.Second},
	}
	for _, step := range steps {
		assert.Equal(t, start.Add(step.want), budget.Deadline(start.Add(step.at), step.bytes), step.name)
	}
}

func TestSetEventStreamHeadersBoundsStreamWritesByBudget(t *testing.T) {
	recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	SetEventStreamHeaders(c)

	before := time.Now()
	require.NoError(t, StringData(c, strings.Repeat("A", 3600<<10)))

	require.NotEmpty(t, recorder.deadlines)
	assert.False(t, recorder.deadlines[0].Before(before.Add(streamWriteTimeout+450*time.Second)))
	assert.Contains(t, recorder.Body.String(), "data: AAAA")
}
