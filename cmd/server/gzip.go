package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipPool reuses gzip.Writer allocations across requests.
var gzipPool = sync.Pool{
	New: func() interface{} {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	},
}

// gzipResponseWriter routes the response body through a gzip.Writer.
// Content-Encoding is set on the underlying writer by gzipMiddleware before
// the handler runs, so this wrapper only needs to redirect Write.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.gz.Write(b)
}

// clientAcceptsGzip reports whether the request's Accept-Encoding allows gzip.
func clientAcceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// gzipMiddleware compresses /api/ responses for clients that accept gzip.
// It is scoped to /api/ paths in v1 to avoid Range-request edge cases on
// static files, and skips WebSocket upgrades (which hijack the connection).
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.Header.Get("Upgrade") != "" ||
			!clientAcceptsGzip(r) ||
			w.Header().Get("Content-Encoding") != "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			gz.Close()
			gzipPool.Put(gz)
		}()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}
