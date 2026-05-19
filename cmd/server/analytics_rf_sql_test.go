package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAnalyticsSQLBackendFlagWiring(t *testing.T) {
	db := setupTestDB(t)
	cfg := &PacketStoreConfig{AnalyticsSQLBackend: true}
	ps := NewPacketStore(db, cfg)
	if !ps.analyticsSQLBackend {
		t.Fatal("expected analyticsSQLBackend true when config sets it")
	}
}

func TestRFRegionObserverIdxs(t *testing.T) {
	db := setupTestDB(t)
	idxs, err := rfRegionObserverIdxs(db, "")
	if err != nil {
		t.Fatalf("rfRegionObserverIdxs empty region: %v", err)
	}
	if idxs != nil {
		t.Fatalf("empty region must return nil idx set, got %v", idxs)
	}
}

func TestRFHistogramHelperPackageLevel(t *testing.T) {
	h := rfBuildHistogramF64([]float64{1, 2, 3, 4}, 2)
	bins, ok := h["bins"].([]map[string]interface{})
	if !ok || len(bins) != 2 {
		t.Fatalf("expected 2 bins, got %#v", h["bins"])
	}
	if h["min"].(float64) != 1 || h["max"].(float64) != 4 {
		t.Fatalf("min/max wrong: %#v", h)
	}
}

func TestEnsureServerIndexesCreatesTSSnrRssi(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	stmts := []string{
		`CREATE TABLE transmissions (id INTEGER PRIMARY KEY, raw_hex TEXT, hash TEXT, first_seen TEXT, route_type INTEGER, payload_type INTEGER, payload_version INTEGER, decoded_json TEXT)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, transmission_id INTEGER, observer_idx INTEGER, snr REAL, rssi REAL, path_json TEXT, timestamp INTEGER)`,
		`CREATE TABLE observers (id TEXT PRIMARY KEY, name TEXT, last_seen TEXT)`,
		`CREATE TABLE nodes (public_key TEXT PRIMARY KEY, name TEXT, role TEXT, last_seen TEXT)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	conn.Close()
	if err := ensureServerIndexes(dbPath); err != nil {
		t.Fatalf("ensureServerIndexes: %v", err)
	}
	conn2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer conn2.Close()
	var name string
	err = conn2.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_observations_ts_snr_rssi'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("composite index not created: %v", err)
	}
}
