package main

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
)

// captureWriter is an http.ResponseWriter that buffers the entire response
// so it can be stored in the cache instead of streamed to the client.
type captureWriter struct {
	header http.Header
	buf    bytes.Buffer
	status int
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header), status: http.StatusOK}
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) WriteHeader(status int)      { c.status = status }
func (c *captureWriter) Write(b []byte) (int, error) { return c.buf.Write(b) }

// cacheKey builds a deterministic key from method, path, sorted query
// parameters, and whether the client accepts gzip. Sorting makes the key
// stable regardless of query-parameter order; the gzip suffix keeps
// compressed and uncompressed responses in separate cache slots.
func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(r.Method)
	sb.WriteByte(' ')
	sb.WriteString(r.URL.Path)
	sb.WriteByte('?')
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(v)
			sb.WriteByte('&')
		}
	}
	if clientAcceptsGzip(r) {
		sb.WriteString("|gz")
	}
	return sb.String()
}
