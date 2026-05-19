package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// setupTestDBFile creates a file-backed SQLite DB with the full v3 schema and
// returns a *DB whose .path is a real temp file. Tasks that call cachedRW by
// path (e.g. ensureRFRollupTable) must use this rather than the in-memory
// setupTestDB.
func setupTestDBFile(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	conn, err := sql.Open("sqlite", "file:"+dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	schema := `
		CREATE TABLE nodes (
			public_key TEXT PRIMARY KEY,
			name TEXT, role TEXT, lat REAL, lon REAL,
			last_seen TEXT, first_seen TEXT,
			advert_count INTEGER DEFAULT 0,
			battery_mv INTEGER, temperature_c REAL,
			foreign_advert INTEGER DEFAULT 0
		);
		CREATE TABLE observers (
			id TEXT PRIMARY KEY,
			name TEXT, iata TEXT, last_seen TEXT, first_seen TEXT,
			packet_count INTEGER DEFAULT 0,
			model TEXT, firmware TEXT, client_version TEXT, radio TEXT,
			battery_mv INTEGER, uptime_secs INTEGER, noise_floor REAL,
			inactive INTEGER DEFAULT 0, last_packet_at TEXT DEFAULT NULL
		);
		CREATE TABLE transmissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			raw_hex TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
			first_seen TEXT NOT NULL, route_type INTEGER,
			payload_type INTEGER, payload_version INTEGER,
			decoded_json TEXT, channel_hash TEXT DEFAULT NULL,
			from_pubkey TEXT DEFAULT NULL,
			created_at TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			transmission_id INTEGER NOT NULL REFERENCES transmissions(id),
			observer_idx INTEGER, direction TEXT,
			snr REAL, rssi REAL, score INTEGER,
			path_json TEXT, timestamp INTEGER NOT NULL,
			resolved_path TEXT, raw_hex TEXT
		);
	`
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		t.Fatalf("setupTestDBFile schema: %v", err)
	}
	conn.Close()

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("setupTestDBFile OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRFBinIndexAndPacking(t *testing.T) {
	if got := rfBinIndex(-30, rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount); got != 0 {
		t.Fatalf("snr -30 -> bin %d, want 0", got)
	}
	if got := rfBinIndex(1000, rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount); got != rfSnrBinCount-1 {
		t.Fatalf("snr 1000 clamps to %d, want %d", got, rfSnrBinCount-1)
	}
	counts := make([]int, rfSnrBinCount)
	counts[0] = 5
	counts[49] = 7
	packed := rfPackBins(counts)
	if len(packed) != rfSnrBinCount*2 {
		t.Fatalf("packed len %d, want %d", len(packed), rfSnrBinCount*2)
	}
	out := rfUnpackBins(packed, rfSnrBinCount)
	if out[0] != 5 || out[49] != 7 {
		t.Fatalf("unpack mismatch: %v", out[:1])
	}
}

func TestEnsureRFRollupTable(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatalf("ensureRFRollupTable: %v", err)
	}
	for _, tbl := range []string{"rf_rollup", "rf_rollup_tx", "rf_rollup_meta"} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("table %s not created: %v", tbl, err)
		}
	}
}
