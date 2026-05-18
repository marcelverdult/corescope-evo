package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestResponseCache_CachesErrorsBriefly(t *testing.T) {
	rc := newResponseCache(time.Minute)
	var calls int32
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
		w.Write([]byte("err"))
	}))
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/stats", nil))
		if w.Code != 500 {
			t.Fatalf("request %d code = %d, want 500", i, w.Code)
		}
	}
	// Errors are negative-cached, so a rapid burst collapses to one call.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("error burst: handler called %d times, want 1 (negative-cached)", got)
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

func TestResponseCache_ErrorPathCoalescesWithoutDeadlock(t *testing.T) {
	rc := newResponseCache(time.Minute)
	var calls int32
	release := make(chan struct{})
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the first owner so the others pile up on the key
		w.WriteHeader(500)
		w.Write([]byte("err"))
	}))
	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/stats", nil))
		}()
	}
	time.Sleep(50 * time.Millisecond) // let all 10 coalesce onto one key
	close(release)
	wg.Wait() // must return — proves no deadlock/spin

	// The owner negative-caches the 500; waiters serve it from cache.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("error burst: handler called %d times, want 1 (single-flight + negative cache)", got)
	}
}

func TestResponseCache_ErrorCacheExpires(t *testing.T) {
	// rc.ttl shorter than errorCacheTTL, so cacheFor is capped at rc.ttl.
	rc := newResponseCache(20 * time.Millisecond)
	var calls int32
	h := rc.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
		w.Write([]byte("err"))
	}))
	do := func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/stats", nil))
	}
	do()
	time.Sleep(40 * time.Millisecond)
	do()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("handler called %d times, want 2 (error cache must expire)", got)
	}
}
