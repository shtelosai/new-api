package common

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

type failingSSEWriter struct {
	*httptest.ResponseRecorder
	calls, failAt int
	err           error
}

func (w *failingSSEWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, w.err
	}
	return w.ResponseRecorder.Write(p)
}

func TestCustomEventPropagatesWriteErrors(t *testing.T) {
	for _, failure := range []int{1, 2} {
		writer := &failingSSEWriter{ResponseRecorder: httptest.NewRecorder(), failAt: failure, err: errors.New("SSE write failed")}
		event := CustomEvent{Data: "data: hello"}
		assert.ErrorIs(t, event.Render(writer), writer.err)
	}
}
