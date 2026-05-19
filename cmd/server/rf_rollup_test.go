package main

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

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
	// Keep the writable connection open so mustExec (which writes via db.conn)
	// can insert test fixtures. OpenDB would open read-only, which is fine for
	// querying but blocks writes needed by test helpers.
	db := &DB{conn: conn, path: dbPath, isV3: true, hasResolvedPath: true, hasObsRawHex: true}
	t.Cleanup(func() {
		conn.Close()
		closeRWCache()
	})
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

func TestRecomputeRFRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccddee','h2','2026-05-18T10:05:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,5.0,-80.0,1779098400),(1,2,7.0,-90.0,1779098400),(2,1,9.0,-70.0,1779098700)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeRFRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	var nObs, nSnr int
	var snrSum float64
	err = rw.QueryRow(`SELECT SUM(n_obs),SUM(n_snr),SUM(snr_sum) FROM rf_rollup WHERE hour=?`,
		"2026-05-18T10").Scan(&nObs, &nSnr, &snrSum)
	if err != nil {
		t.Fatal(err)
	}
	if nObs != 3 || nSnr != 3 || snrSum != 21.0 {
		t.Fatalf("rollup wrong: nObs=%d nSnr=%d snrSum=%v", nObs, nSnr, snrSum)
	}
	var distinctTx int
	if err := rw.QueryRow(`SELECT SUM(distinct_tx) FROM rf_rollup_tx WHERE hour=?`,
		"2026-05-18T10").Scan(&distinctTx); err != nil {
		t.Fatal(err)
	}
	if distinctTx != 2 {
		t.Fatalf("distinct_tx=%d want 2", distinctTx)
	}
}

func TestRFRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779098400)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	var n int
	rw.QueryRow(`SELECT SUM(n_obs) FROM rf_rollup`).Scan(&n)
	if n != 1 {
		t.Fatalf("after first run n_obs=%d want 1", n)
	}
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (2,1,2,6.0,-81.0,1779098500)`)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT SUM(n_obs) FROM rf_rollup`).Scan(&n)
	if n != 2 {
		t.Fatalf("after second run n_obs=%d want 2", n)
	}
}

func TestComputeAnalyticsRFSQLUsesRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	// 1779098400 = real UTC epoch inside hour 2026-05-18T10
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779098400)`)
	rw, _ := cachedRW(db.path)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	res, err := computeAnalyticsRFSQL(db, "",
		TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"})
	if err != nil {
		t.Fatalf("computeAnalyticsRFSQL: %v", err)
	}
	if res["totalAllPackets"].(int) != 1 {
		t.Fatalf("totalAllPackets=%v want 1", res["totalAllPackets"])
	}
}

func TestComputeRFFromRollupShape(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	// 1779098400 / 1779098410 are real UTC epoch seconds inside hour 2026-05-18T10.
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779098400),(2,1,2,7.0,-90.0,1779098410)`)
	rw, _ := cachedRW(db.path)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	// Explicit window covering the fixture (do NOT use TimeWindow{} — its 24h
	// default is relative to wall-clock now and would exclude fixed past data).
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}
	res, err := computeRFFromRollup(db, "", win)
	if err != nil {
		t.Fatalf("computeRFFromRollup: %v", err)
	}
	for _, k := range []string{"totalPackets", "totalAllPackets", "totalTransmissions",
		"snr", "rssi", "snrValues", "rssiValues", "packetSizes", "packetsPerHour",
		"payloadTypes", "snrByType", "signalOverTime", "scatterData", "timeSpanHours"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if res["totalAllPackets"].(int) != 2 {
		t.Errorf("totalAllPackets=%v want 2", res["totalAllPackets"])
	}
}

func TestRFFallbackWhenRollupNotReady(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779098400)`)
	// rollup NOT populated (no maintenance run) -> rfRollupReady is false
	ps := loadStore(t, db.path, 0)
	ps.analyticsSQLBackend = true
	res, err := ps.GetAnalyticsRFWithWindow("", TimeWindow{})
	if err != nil {
		t.Fatalf("expected in-memory fallback, got error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	if got := res["totalAllPackets"].(int); got != 1 {
		t.Fatalf("expected in-memory fallback to report totalAllPackets=1, got %d", got)
	}
}

func TestRFRollupParity(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO observers(rowid,id,name,iata) VALUES
		(1,'o1','Obs1','SJC'),(2,'o2','Obs2','SJC'),(3,'o3','Obs3','LAX')`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type) VALUES
		(1,'aabbcc','h1','2026-05-18T10:00:00Z',1),
		(2,'ddee','h2','2026-05-18T10:30:00Z',1),
		(3,'ff0011','h3','2026-05-18T11:00:00Z',2)`)
	// epoch seconds inside 2026-05-18: 1779098400=10:00, 1779100200=10:30, 1779102000=11:00
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp) VALUES
		(1,1,1,5.0,-80.0,1779098400),(2,1,2,7.0,-88.0,1779098400),
		(3,2,1,6.0,-82.0,1779100200),(4,3,3,9.0,-70.0,1779102000)`)
	rw, _ := cachedRW(db.path)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	store := loadStore(t, db.path, 0)
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}

	for _, region := range []string{"", "SJC"} {
		mem := store.computeAnalyticsRF(region, win)
		roll, err := computeRFFromRollup(db, region, win)
		if err != nil {
			t.Fatalf("[region=%q] rollup: %v", region, err)
		}
		for _, k := range []string{"totalPackets", "totalAllPackets"} {
			if fmt.Sprint(mem[k]) != fmt.Sprint(roll[k]) {
				t.Errorf("[region=%q] %s: mem=%v rollup=%v", region, k, mem[k], roll[k])
			}
		}
		ms := mem["snr"].(map[string]interface{})
		rs := roll["snr"].(map[string]interface{})
		if math.Abs(rfToF(ms["avg"])-rfToF(rs["avg"])) > 1e-9 {
			t.Errorf("[region=%q] snr.avg: mem=%v rollup=%v", region, ms["avg"], rs["avg"])
		}
	}
}

func TestRecomputeRFRollupHourRangeBoundary(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	// 1779098400 = 2026-05-18T10:00:00Z. In-hour: +0, +3599. Out: -1, +3600.
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,5.0,-80.0,1779098399),(1,1,5.0,-80.0,1779098400),
		       (1,1,5.0,-80.0,1779101999),(1,1,5.0,-80.0,1779102000)`)
	rw, _ := cachedRW(db.path)
	if err := recomputeRFRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatal(err)
	}
	var nObs int
	rw.QueryRow(`SELECT COALESCE(SUM(n_obs),0) FROM rf_rollup WHERE hour=?`,
		"2026-05-18T10").Scan(&nObs)
	if nObs != 2 {
		t.Fatalf("hour 2026-05-18T10 n_obs=%d, want 2 (only the two in-range rows)", nObs)
	}
}

func TestRecomputeRFRollupHourNullObserver(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	// One observation with a real observer_idx, one with NULL observer_idx.
	// 1779098400 = 2026-05-18T10:00:00Z.
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,5,7.0,-80.0,1779098400),(1,NULL,9.0,-70.0,1779098400)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeRFRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute must not fail on NULL observer_idx: %v", err)
	}
	var nObs int
	if err := rw.QueryRow(`SELECT COALESCE(SUM(n_obs),0) FROM rf_rollup WHERE hour=?`,
		"2026-05-18T10").Scan(&nObs); err != nil {
		t.Fatal(err)
	}
	if nObs != 2 {
		t.Fatalf("n_obs=%d, want 2 (both observations counted, NULL-observer included)", nObs)
	}
	// The NULL-observer row is stored under observer_idx = -1.
	var nNull int
	rw.QueryRow(`SELECT COALESCE(SUM(n_obs),0) FROM rf_rollup WHERE hour=? AND observer_idx=-1`,
		"2026-05-18T10").Scan(&nNull)
	if nNull != 1 {
		t.Fatalf("expected 1 obs under observer_idx=-1 sentinel, got %d", nNull)
	}
}

func rfToF(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func TestRFRollupPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	rw, _ := cachedRW(db.path)
	gen, err := rw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1779000000)
	txID := 0
	for d := 0; d < 30; d++ {
		for n := 0; n < 1200; n++ {
			txID++
			ts := base + int64(d)*86400 + int64(n)*60
			first := time.Unix(ts, 0).UTC().Format(time.RFC3339)
			pt := txID % 4
			if _, err := gen.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
				VALUES (?,?,?,?,?)`, txID, "aabbcc", fmt.Sprintf("h%d", txID), first, pt); err != nil {
				t.Fatal(err)
			}
			for o := 0; o < 28; o++ {
				if _, err := gen.Exec(`INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
					VALUES (?,?,?,?,?)`, txID, o%100, float64(o%20-10), float64(-60-o%50), ts); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := gen.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	win24 := TimeWindow{
		Since: time.Unix(base+29*86400, 0).UTC().Format(time.RFC3339),
		Until: time.Unix(base+30*86400, 0).UTC().Format(time.RFC3339),
	}
	t0 := time.Now()
	if _, err := computeRFFromRollup(db, "", win24); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 200*time.Millisecond {
		t.Errorf("24h query took %s, want < 200ms", d)
	}

	t1 := time.Now()
	if _, err := computeRFFromRollup(db, "", TimeWindow{
		Since: "2026-01-01T00:00:00Z", Until: "2027-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t1); d > 2*time.Second {
		t.Errorf("full-history query took %s, want < 2s", d)
	}
}
