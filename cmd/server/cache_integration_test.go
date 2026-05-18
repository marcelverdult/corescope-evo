package main

import (
	"compress/gzip"
	"encoding/json"
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
