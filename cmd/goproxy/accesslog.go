package main

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// accessLogWriter records the response status and size while keeping the
// optional ResponseWriter behavior streaming handlers rely on.
type accessLogWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *accessLogWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessLogWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// ReadFrom preserves the underlying optimized copy path (sendfile).
func (w *accessLogWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.n += n
		return n, err
	}
	n, err := io.Copy(struct{ io.Writer }{w}, r)
	return n, err
}

func (w *accessLogWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *accessLogWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withAccessLog logs one line per request at Debug level. The level filter
// in newLogger hides it unless DEBUG=1, so the journal stays quiet.
func withAccessLog(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		aw := &accessLogWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(aw, r)
		log.Debug("access",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.RequestURI(),
			"status", aw.status, "bytes", aw.n,
			"took", time.Since(start).Round(time.Millisecond))
	})
}
