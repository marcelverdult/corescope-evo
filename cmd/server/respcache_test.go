package main

import (
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
