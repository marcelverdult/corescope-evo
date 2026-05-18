# API Response Cache + Gzip Compression Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Go server survive hundreds of concurrent users by serving hot `/api/` reads from a short-TTL shared cache (computed once per TTL, not once per request) and gzipping API responses.

**Architecture:** Two `gorilla/mux` middlewares added to the existing chain in `RegisterRoutes`. `gzipMiddleware` (inner) compresses `/api/` responses for clients that accept gzip. `responseCache.middleware` (outer) is a TTL cache with single-flight: concurrent misses for the same key run the handler exactly once and share the result. Because the cache is outside gzip, a cached entry stores the already-gzipped bytes — compression happens once per TTL, and a cache hit is a buffer copy with zero query, zero JSON marshal, zero gzip work. The underlying packet data is append-only and the WebSocket poller pushes live updates on a separate channel, so a few seconds of REST staleness is acceptable and no explicit invalidation is needed — TTL handles it.

**Tech Stack:** Go 1.25, `gorilla/mux`, stdlib `compress/gzip`. No new dependencies. SQLite (`modernc.org/sqlite`) read-only. All code is `package main` in `cmd/server/`.

---

## File Structure

- **Create** `cmd/server/gzip.go` — `gzipMiddleware`, `gzipResponseWriter`, `clientAcceptsGzip`, gzip writer pool. Responsibility: transport compression of `/api/` responses.
- **Create** `cmd/server/gzip_test.go` — tests for the gzip middleware.
- **Create** `cmd/server/respcache.go` — `captureWriter`, `cacheKey`, `cacheEntry`, `responseCache`, `responseCache.middleware`, eviction, single-flight. Responsibility: shared TTL response cache.
- **Create** `cmd/server/respcache_test.go` — unit + concurrency tests for the cache.
- **Create** `cmd/server/cache_integration_test.go` — end-to-end test through the real router.
- **Modify** `cmd/server/routes.go` — add `respCache *responseCache` field to `Server` struct (~routes.go:67, after `router *mux.Router`); add two `r.Use(...)` calls in `RegisterRoutes` (~routes.go:153, after `r.Use(s.backfillStatusMiddleware)`).
- **Modify** `cmd/server/main.go` — read `CORESCOPE_API_CACHE_TTL` env var and set `srv.respCache` before `srv.RegisterRoutes(router)` is called (~main.go:320).
- **Modify** `.env.example` — document the new `CORESCOPE_API_CACHE_TTL` env var.

**Middleware order after this change** (outermost → innermost): `corsMiddleware` → `rateLimitMiddleware` → `perfMiddleware` → `backfillStatusMiddleware` → `responseCache.middleware` → `gzipMiddleware` → handler. Rate limiting still protects the box; `perfMiddleware` still measures cache hits; the cache sits outside gzip so it stores compressed bytes.

**Test command:** `cd cmd/server && go test -race ./...` (Go 1.25; race detector required, matching CI).

---

## Task 1: Gzip middleware

**Files:**
- Create: `cmd/server/gzip.go`
- Test: `cmd/server/gzip_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/server/gzip_test.go`:

```go
package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGzipMiddleware_CompressesWhenAccepted(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hello":"world"}`))
	}))
	req := httptest.NewRequest("GET", "/api/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding gzip, got %q", w.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	body, _ := io.ReadAll(gr)
	if string(body) != `{"hello":"world"}` {
		t.Errorf("decompressed body = %q", body)
	}
}

func TestGzipMiddleware_PlainWhenNotAccepted(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("plain"))
	}))
	req := httptest.NewRequest("GET", "/api/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("expected no Content-Encoding, got %q", w.Header().Get("Content-Encoding"))
	}
	if w.Body.String() != "plain" {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestGzipMiddleware_SkipsWebSocketUpgrade(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ws"))
	}))
	req := httptest.NewRequest("GET", "/api/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("must not gzip a WebSocket upgrade request")
	}
}

func TestGzipMiddleware_SkipsNonAPIPath(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("static"))
	}))
	req := httptest.NewRequest("GET", "/index.html", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("v1 gzip is scoped to /api/ paths only")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/server && go test -run TestGzipMiddleware -race ./...`
Expected: FAIL — compile error, `undefined: gzipMiddleware`.

- [ ] **Step 3: Write the implementation**

Create `cmd/server/gzip.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd cmd/server && go test -run TestGzipMiddleware -race ./...`
Expected: PASS — all four tests.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/gzip.go cmd/server/gzip_test.go
git commit -m "feat(server): gzip middleware for /api/ responses"
```

---

## Task 2: Cache helpers — capture writer and cache key

**Files:**
- Create: `cmd/server/respcache.go` (partial — helpers only; Task 3 appends the cache)
- Test: `cmd/server/respcache_test.go` (partial — Task 3 appends more)

- [ ] **Step 1: Write the failing tests**

Create `cmd/server/respcache_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCaptureWriter_BuffersBodyHeaderAndStatus(t *testing.T) {
	c := newCaptureWriter()
	c.Header().Set("Content-Type", "application/json")
	c.WriteHeader(201)
	c.Write([]byte("abc"))
	c.Write([]byte("def"))

	if c.status != 201 {
		t.Errorf("status = %d, want 201", c.status)
	}
	if c.buf.String() != "abcdef" {
		t.Errorf("body = %q, want abcdef", c.buf.String())
	}
	if c.Header().Get("Content-Type") != "application/json" {
		t.Errorf("header lost: %q", c.Header().Get("Content-Type"))
	}
}

func TestCaptureWriter_DefaultStatusIs200(t *testing.T) {
	c := newCaptureWriter()
	c.Write([]byte("x"))
	if c.status != 200 {
		t.Errorf("default status = %d, want 200", c.status)
	}
}

func TestCacheKey_StableAcrossParamOrder(t *testing.T) {
	r1 := httptest.NewRequest("GET", "/api/packets?b=2&a=1", nil)
	r2 := httptest.NewRequest("GET", "/api/packets?a=1&b=2", nil)
	if cacheKey(r1) != cacheKey(r2) {
		t.Errorf("keys differ across param order: %q vs %q", cacheKey(r1), cacheKey(r2))
	}
}

func TestCacheKey_DistinctPaths(t *testing.T) {
	r1 := httptest.NewRequest("GET", "/api/nodes", nil)
	r2 := httptest.NewRequest("GET", "/api/stats", nil)
	if cacheKey(r1) == cacheKey(r2) {
		t.Error("different paths must produce different keys")
	}
}

func TestCacheKey_GzipDistinctFromPlain(t *testing.T) {
	r1 := httptest.NewRequest("GET", "/api/stats", nil)
	r2 := httptest.NewRequest("GET", "/api/stats", nil)
	r2.Header.Set("Accept-Encoding", "gzip")
	if cacheKey(r1) == cacheKey(r2) {
		t.Error("gzip and non-gzip requests must have distinct cache keys")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/server && go test -run 'TestCaptureWriter|TestCacheKey' -race ./...`
Expected: FAIL — compile error, `undefined: newCaptureWriter`, `undefined: cacheKey`.

- [ ] **Step 3: Write the implementation**

Create `cmd/server/respcache.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd cmd/server && go test -run 'TestCaptureWriter|TestCacheKey' -race ./...`
Expected: PASS — all five tests.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/respcache.go cmd/server/respcache_test.go
git commit -m "feat(server): response capture writer and cache key helper"
```

---

## Task 3: Response cache with single-flight middleware

**Files:**
- Modify: `cmd/server/respcache.go` (append the cache type + middleware)
- Test: `cmd/server/respcache_test.go` (append cache tests)

- [ ] **Step 1: Write the failing tests**

Append to `cmd/server/respcache_test.go`. Add `"sync"`, `"sync/atomic"`, and `"time"` to its import block so it reads:

```go
import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)
```

Then append these test functions:

```go
func TestResponseCache_ServesRepeatedRequestsFromCache(t *testing.T) {
	rc := newResponseCache(time.Minute)
	var calls int32
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte("payload"))
	}))
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/stats", nil))
		if w.Body.String() != "payload" {
			t.Fatalf("request %d body = %q", i, w.Body.String())
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("handler called %d times, want 1", got)
	}
}

func TestResponseCache_ExpiresAfterTTL(t *testing.T) {
	rc := newResponseCache(20 * time.Millisecond)
	var calls int32
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte("x"))
	}))
	do := func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/stats", nil))
	}
	do()
	time.Sleep(40 * time.Millisecond)
	do()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("handler called %d times, want 2 (one per TTL window)", got)
	}
}

func TestResponseCache_NonCacheablePathBypasses(t *testing.T) {
	rc := newResponseCache(time.Minute)
	var calls int32
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte("x"))
	}))
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/perf", nil))
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("non-cacheable path: handler called %d times, want 3", got)
	}
}

func TestResponseCache_DoesNotCacheErrorResponses(t *testing.T) {
	rc := newResponseCache(time.Minute)
	var calls int32
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
		w.Write([]byte("err"))
	}))
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/stats", nil))
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("error responses must not be cached: handler called %d times, want 3", got)
	}
}

func TestResponseCache_SingleFlightCoalescesConcurrentMisses(t *testing.T) {
	rc := newResponseCache(time.Minute)
	var calls int32
	release := make(chan struct{})
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release // block so all concurrent requests pile up on one key
		w.Write([]byte("ok"))
	}))
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/stats", nil))
		}()
	}
	time.Sleep(50 * time.Millisecond) // let all 20 arrive and coalesce
	close(release)
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("single-flight failed: handler called %d times, want 1", got)
	}
}

func TestResponseCache_EvictsWhenOverCapacity(t *testing.T) {
	rc := newResponseCache(time.Minute)
	rc.maxEntries = 4
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest("GET", "/api/packets?p="+strconv.Itoa(i), nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	rc.mu.Lock()
	n := len(rc.entries)
	rc.mu.Unlock()
	if n > rc.maxEntries {
		t.Errorf("cache holds %d entries, want <= %d", n, rc.maxEntries)
	}
}
```

Add `"strconv"` to the test file's import block as well (used by the eviction test), so the final import block is:

```go
import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/server && go test -run TestResponseCache -race ./...`
Expected: FAIL — compile error, `undefined: newResponseCache`.

- [ ] **Step 3: Write the implementation**

Append to `cmd/server/respcache.go`. First extend its import block to:

```go
import (
	"bytes"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)
```

Then append:

```go
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

			e := &cacheEntry{
				status: cap.status,
				header: cap.header.Clone(),
				body:   cap.buf.Bytes(),
			}
			rc.mu.Lock()
			if cap.status == http.StatusOK {
				e.expires = time.Now().Add(rc.ttl)
				rc.entries[key] = e
				rc.evictLocked()
			}
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd cmd/server && go test -run TestResponseCache -race ./...`
Expected: PASS — all six tests, no race detector warnings.

- [ ] **Step 5: Commit**

```bash
git add cmd/server/respcache.go cmd/server/respcache_test.go
git commit -m "feat(server): TTL response cache with single-flight"
```

---

## Task 4: Wire middlewares into the server

**Files:**
- Modify: `cmd/server/routes.go` (Server struct ~line 67; `RegisterRoutes` ~line 153)
- Modify: `cmd/server/main.go` (~line 320, before `srv.RegisterRoutes(router)`)
- Modify: `.env.example`
- Test: `cmd/server/cache_integration_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `cmd/server/cache_integration_test.go`:

```go
package main

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// setupCachingTestServer builds a server with the response cache enabled,
// mirroring setupTestServer (routes_test.go) plus srv.respCache.
func setupCachingTestServer(t *testing.T) *mux.Router {
	t.Helper()
	db := setupTestDB(t)
	seedTestData(t, db)
	cfg := &Config{Port: 3000}
	hub := NewHub()
	srv := NewServer(db, cfg, hub)
	store := NewPacketStore(db, nil)
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load failed: %v", err)
	}
	srv.store = store
	srv.respCache = newResponseCache(time.Minute)
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return router
}

func TestIntegration_StatsGzippedAndCached(t *testing.T) {
	router := setupCachingTestServer(t)

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/stats", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w1 := get()
	if w1.Code != 200 {
		t.Fatalf("first request code = %d", w1.Code)
	}
	if w1.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("response not gzipped: Content-Encoding = %q", w1.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(w1.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(gr).Decode(&body); err != nil {
		t.Fatalf("decode gzipped JSON: %v", err)
	}
	if body["engine"] != "go" {
		t.Errorf("engine = %v, want go", body["engine"])
	}

	// Second request is served from the cache and must still be valid.
	w2 := get()
	if w2.Code != 200 {
		t.Fatalf("cached request code = %d", w2.Code)
	}
	gr2, err := gzip.NewReader(w2.Body)
	if err != nil {
		t.Fatalf("cached response not valid gzip: %v", err)
	}
	var body2 map[string]interface{}
	if err := json.NewDecoder(gr2).Decode(&body2); err != nil {
		t.Fatalf("decode cached gzipped JSON: %v", err)
	}
	if body2["engine"] != "go" {
		t.Errorf("cached engine = %v, want go", body2["engine"])
	}
}

func TestIntegration_PlainClientNotGzipped(t *testing.T) {
	router := setupCachingTestServer(t)
	req := httptest.NewRequest("GET", "/api/stats", nil) // no Accept-Encoding
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("client did not request gzip; response must be plain")
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode plain JSON: %v", err)
	}
	if body["engine"] != "go" {
		t.Errorf("engine = %v, want go", body["engine"])
	}
}

var _ = http.MethodGet // keep net/http imported if unused above
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd cmd/server && go test -run TestIntegration_Stats -race ./...`
Expected: FAIL — compile error, `srv.respCache undefined (type *Server has no field respCache)`.

- [ ] **Step 3: Add the `respCache` field to the `Server` struct**

In `cmd/server/routes.go`, in the `Server` struct, the final field is currently:

```go
	// Router reference for OpenAPI spec generation
	router *mux.Router
}
```

Change it to:

```go
	// Router reference for OpenAPI spec generation
	router *mux.Router

	// Shared TTL response cache for hot /api/ reads. nil = caching disabled.
	respCache *responseCache
}
```

- [ ] **Step 4: Register the two middlewares in `RegisterRoutes`**

In `cmd/server/routes.go`, `RegisterRoutes` currently ends its middleware block with:

```go
	// Backfill status header middleware
	r.Use(s.backfillStatusMiddleware)
```

Add immediately after that line:

```go
	// Backfill status header middleware
	r.Use(s.backfillStatusMiddleware)

	// Shared TTL response cache — collapses concurrent reads of hot /api/
	// endpoints to a single query per TTL window. Added before gzip so a
	// cached entry stores the already-compressed bytes.
	if s.respCache != nil {
		r.Use(s.respCache.middleware)
	}

	// Gzip /api/ responses for clients that accept it.
	r.Use(gzipMiddleware)
```

- [ ] **Step 5: Enable the cache in `main.go`**

In `cmd/server/main.go`, the server is constructed and configured here (~main.go:319-321):

```go
	srv := NewServer(database, cfg, hub)
	srv.store = store
	srv.channelKeys = loadServerChannelKeys(cfg, configDir)

	router := mux.NewRouter()
	srv.RegisterRoutes(router)
```

Change it to:

```go
	srv := NewServer(database, cfg, hub)
	srv.store = store
	srv.channelKeys = loadServerChannelKeys(cfg, configDir)

	// API response cache. TTL from CORESCOPE_API_CACHE_TTL (seconds);
	// 0 disables caching. Default 10s.
	apiCacheTTL := 10 * time.Second
	if v := os.Getenv("CORESCOPE_API_CACHE_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			apiCacheTTL = time.Duration(n) * time.Second
		}
	}
	if apiCacheTTL > 0 {
		srv.respCache = newResponseCache(apiCacheTTL)
		log.Printf("[cache] API response cache enabled, TTL %v", apiCacheTTL)
	} else {
		log.Printf("[cache] API response cache disabled")
	}

	router := mux.NewRouter()
	srv.RegisterRoutes(router)
```

`os`, `log`, and `time` are already imported in `main.go`. Confirm `strconv` is in the import block — if not, add `"strconv"` to it.

- [ ] **Step 6: Run the integration test to verify it passes**

Run: `cd cmd/server && go test -run TestIntegration -race ./...`
Expected: PASS — both `TestIntegration_StatsGzippedAndCached` and `TestIntegration_PlainClientNotGzipped`.

- [ ] **Step 7: Run the full server test suite**

Run: `cd cmd/server && go build . && go test -race ./...`
Expected: PASS — the build succeeds and the entire `cmd/server` suite is green (no regression in existing handler tests from the new middleware chain).

- [ ] **Step 8: Document the env var in `.env.example`**

In `.env.example`, add this block (place it near the other server/runtime settings):

```
# API response cache TTL in seconds. The server caches hot read-only /api/
# endpoints (stats, packets, nodes, observers, channels, iata-coords) for
# this many seconds so concurrent users share one query. 0 disables it.
# Default: 10
CORESCOPE_API_CACHE_TTL=10
```

- [ ] **Step 9: Commit**

```bash
git add cmd/server/routes.go cmd/server/main.go cmd/server/cache_integration_test.go .env.example
git commit -m "feat(server): wire response cache + gzip into the middleware chain"
```

---

## Out of Scope (follow-ups, not this plan)

- **Background cache warming** — a goroutine that refreshes hot keys on a timer so no user ever waits for the fill. v1 uses lazy fill + single-flight: one user per TTL pays the fill cost, everyone else is instant. Adequate; revisit if the once-per-TTL latency spike matters.
- **Browser `Cache-Control` headers** on API responses — lets browsers skip revalidation entirely. Deliberately omitted so the server stays the single source of freshness.
- **Static-asset gzip + immutable caching** — v1 gzip is scoped to `/api/`. Compressing/`max-age`-ing the `?v=`-busted JS/CSS is a separate change.
- **`groupByHash` query rewrite** — the 3-correlated-subqueries-per-row shape in `QueryGroupedPackets`. Caching makes it run rarely; rewriting it is a separate optimization.
- **Byte-size cap** — v1 caps the cache at 64 entries by count. A total-bytes cap is more precise for the large `/api/packets` payloads.

---

## Self-Review

**1. Spec coverage:**
- Server-side response caching (collapse N users → 1 query) → Task 3 (`responseCache` + single-flight) + Task 4 (wiring).
- Gzip compression → Task 1 + Task 4 (wiring).
- "Compress once per TTL" → cache ordered outside gzip; entry stores gzipped bytes (Task 3 middleware design + Task 4 ordering).
- "Fixes the crash under burst" → single-flight (Task 3, `TestResponseCache_SingleFlightCoalescesConcurrentMisses`).
- Configurable / disable-able → `CORESCOPE_API_CACHE_TTL` env (Task 4, Steps 5 + 8).
- Bounded memory → `evictLocked` + `maxEntries` (Task 3, `TestResponseCache_EvictsWhenOverCapacity`).
- WAL — already enabled (`db.go:72`), no task needed.

**2. Placeholder scan:** No TBD/TODO; every code step has complete code; every command has expected output. Clear.

**3. Type consistency:** `responseCache`, `cacheEntry`, `captureWriter`, `cacheKey`, `newResponseCache`, `newCaptureWriter`, `gzipMiddleware`, `gzipResponseWriter`, `clientAcceptsGzip`, `cacheableAPIPaths`, `writeCachedEntry`, `evictLocked` are named identically across Tasks 1–4. `responseCache.middleware` and `gzipMiddleware` both match the `gorilla/mux` middleware signature `func(http.Handler) http.Handler`. `Server.respCache` type matches `newResponseCache`'s return type. Consistent.
