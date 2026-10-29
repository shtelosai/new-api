package helper

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const streamMinClientRate = 8 << 10

// StreamWriteBudget 按累计写入量给予最低读取速率的排空时间，另加 30 秒余量。
// 写入由调用方串行化；停止读取的客户端仍会在有限预算耗尽后超时。
type StreamWriteBudget struct{ drainBy time.Time }

func (b *StreamWriteBudget) Deadline(now time.Time, n int) time.Time {
	if b.drainBy.Before(now) {
		b.drainBy = now
	}
	b.drainBy = b.drainBy.Add(time.Duration(n) * time.Second / streamMinClientRate)
	return b.drainBy.Add(streamWriteTimeout)
}

type streamResponseWriter struct {
	gin.ResponseWriter
	budget StreamWriteBudget
}

func (w *streamResponseWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.budget.Deadline(time.Now(), len(p)))
	return w.ResponseWriter.Write(p)
}

func (w *streamResponseWriter) WriteString(s string) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.budget.Deadline(time.Now(), len(s)))
	return w.ResponseWriter.WriteString(s)
}

func (w *streamResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
