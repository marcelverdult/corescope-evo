package main

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
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

// cacheableAPIPaths is the allowlist of GET endpoints whose responses are
// safe to serve from a short-TTL shared cache. All are read-only; the
// underlying packet data is append-only and live updates arrive separately
// over the WebSocket, so a few seconds of staleness is acceptable.
var cacheableAPIPaths = map[string]bool{
	"/api/stats":       true,
	"/api/packets":     true,
	"/api/nodes":       true,
	"/api/observers":   true,
	"/api/channels":    true,
	"/api/iata-coords": true,
}

// errorCacheTTL bounds how long a non-200 response is served from cache.
// Non-200s are cached briefly so an error burst still collapses to one
// handler call (stampede protection), but not for the full TTL — a
// transient failure must recover quickly.
const errorCacheTTL = 2 * time.Second

// cacheEntry is one stored HTTP response.
type cacheEntry struct {
	status  int
	header  http.Header
	body    []byte
	expires time.Time
}

// responseCache is a TTL cache with single-flight. Concurrent misses for the
// same key run the wrapped handler exactly once and share the result, so a
// burst of traffic collapses to a single query instead of overrunning the DB.
type responseCache struct {
	mu         sync.Mutex
	entries    map[string]*cacheEntry
	inflight   map[string]*sync.WaitGroup
	ttl        time.Duration
	maxEntries int
}

func newResponseCache(ttl time.Duration) *responseCache {
	return &responseCache{
		entries:    make(map[string]*cacheEntry),
		inflight:   make(map[string]*sync.WaitGroup),
		ttl:        ttl,
		maxEntries: 64,
	}
}

// middleware wraps a handler with TTL caching + single-flight for the
// allowlisted GET endpoints. All other requests pass straight through.
func (rc *responseCache) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !cacheableAPIPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		key := cacheKey(r)

		for {
			rc.mu.Lock()
			if e, ok := rc.entries[key]; ok && time.Now().Before(e.expires) {
				rc.mu.Unlock()
				writeCachedEntry(w, e)
				return
			}
			if wg, ok := rc.inflight[key]; ok {
				// Another request is already computing this key. Wait for
				// it, then loop to re-check the cache.
				rc.mu.Unlock()
				wg.Wait()
				continue
			}
			// This goroutine owns the computation for this key.
			wg := &sync.WaitGroup{}
			wg.Add(1)
			rc.inflight[key] = wg
			rc.mu.Unlock()

			cap := newCaptureWriter()
			next.ServeHTTP(cap, r)

			// cap.buf.Bytes() aliases the buffer's internal slice; safe
			// because cap is discarded here and never written again. Any
			// future change that reuses/pools captureWriter must copy.
			e := &cacheEntry{
				status: cap.status,
				header: cap.header.Clone(),
				body:   cap.buf.Bytes(),
			}
			rc.mu.Lock()
			// 200s cached for the full TTL; non-200s cached only briefly
			// (capped at rc.ttl) so an error burst still collapses to one
			// handler call without serving a stale error for long.
			cacheFor := rc.ttl
			if cap.status != http.StatusOK {
				cacheFor = errorCacheTTL
				if rc.ttl < cacheFor {
					cacheFor = rc.ttl
				}
			}
			e.expires = time.Now().Add(cacheFor)
			rc.entries[key] = e
			rc.evictLocked()
			delete(rc.inflight, key)
			rc.mu.Unlock()
			wg.Done()

			writeCachedEntry(w, e)
			return
		}
	})
}

// evictLocked keeps the cache under maxEntries. It first drops expired
// entries, then drops arbitrary entries until under cap. Caller holds rc.mu.
func (rc *responseCache) evictLocked() {
	if len(rc.entries) <= rc.maxEntries {
		return
	}
	now := time.Now()
	for k, e := range rc.entries {
		if now.After(e.expires) {
			delete(rc.entries, k)
		}
	}
	for k := range rc.entries {
		if len(rc.entries) <= rc.maxEntries {
			break
		}
		delete(rc.entries, k)
	}
}

// writeCachedEntry replays a stored response onto a real ResponseWriter.
func writeCachedEntry(w http.ResponseWriter, e *cacheEntry) {
	for k, vals := range e.header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(e.status)
	w.Write(e.body)
}
