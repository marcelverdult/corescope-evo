package main

import (
	"database/sql"
	"fmt"
	"math"
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

func TestRFWindowEpochBounds(t *testing.T) {
	w := TimeWindow{Since: "2026-05-22T00:00:00Z", Until: "2026-05-22T12:00:00Z"}
	lo, hi, ok := rfWindowEpochBounds(w)
	if !ok {
		t.Fatal("expected ok for bounded window")
	}
	if lo != 1779408000 || hi != 1779451200 {
		t.Fatalf("epoch bounds wrong: lo=%d hi=%d", lo, hi)
	}
	if _, _, ok := rfWindowEpochBounds(TimeWindow{}); ok {
		t.Fatal("zero window must report ok=false")
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

func TestRFCoreAggregates(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccddee','h2','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000),(1,1,7.0,-90.0,1779444000),(2,0,NULL,-70.0,1779447600)`)

	agg, err := rfCoreAggregates(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfCoreAggregates: %v", err)
	}
	if agg.NObs != 3 {
		t.Fatalf("NObs=%d want 3", agg.NObs)
	}
	if agg.NSnr != 2 {
		t.Fatalf("NSnr=%d want 2", agg.NSnr)
	}
	if agg.SnrSum != 12.0 || agg.SnrMin != 5.0 || agg.SnrMax != 7.0 {
		t.Fatalf("snr agg wrong: %+v", agg)
	}
	if agg.NRssi != 3 {
		t.Fatalf("NRssi=%d want 3", agg.NRssi)
	}
	if agg.MinTS != 1779444000 || agg.MaxTS != 1779447600 {
		t.Fatalf("timestamp bounds wrong: min=%d max=%d", agg.MinTS, agg.MaxTS)
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

func TestRFSortedColumns(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,7.0,-80.0,1779444000),(1,1,5.0,-90.0,1779444000)`)

	snr, err := rfSortedColumn(db, "snr", "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfSortedColumn snr: %v", err)
	}
	if len(snr) != 2 || snr[0] != 5.0 || snr[1] != 7.0 {
		t.Fatalf("snr column not sorted ascending: %v", snr)
	}

	sizes, err := rfPacketSizes(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfPacketSizes: %v", err)
	}
	if len(sizes) != 1 || sizes[0] != 2 { // 'aabb' = 4 hex chars / 2 = 2 bytes
		t.Fatalf("packet sizes wrong: %v", sizes)
	}
}

func TestRFGroupByQueries(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccdd','h2','2026-05-18T10:30:00Z',1),
		       (3,'eeff','h3','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000),(2,0,7.0,-81.0,1779445800),(3,0,9.0,-82.0,1779447600)`)

	types, err := rfTypeDistribution(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfTypeDistribution: %v", err)
	}
	if types[1] != 2 || types[2] != 1 {
		t.Fatalf("type distribution wrong: %v", types)
	}

	hourly, err := rfHourlyBuckets(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfHourlyBuckets: %v", err)
	}
	// Unix timestamps 1779444000/1779445800 → 2026-05-22T10 UTC,
	// 1779447600 → 2026-05-22T11 UTC (strftime uses observations.timestamp).
	if hourly["2026-05-22T10"].count != 2 || hourly["2026-05-22T11"].count != 1 {
		t.Fatalf("hourly buckets wrong: %v", hourly)
	}
}

func TestComputeAnalyticsRFSQLShape(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccdd','h2','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000),(1,1,7.0,-90.0,1779444000),(2,0,9.0,-70.0,1779447600)`)

	res, err := computeAnalyticsRFSQL(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("computeAnalyticsRFSQL: %v", err)
	}
	for _, k := range []string{"totalPackets", "totalAllPackets", "totalTransmissions",
		"snr", "rssi", "snrValues", "rssiValues", "packetSizes", "packetsPerHour",
		"payloadTypes", "snrByType", "signalOverTime", "scatterData", "timeSpanHours"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing result key %q", k)
		}
	}
	if res["totalAllPackets"].(int) != 3 {
		t.Errorf("totalAllPackets=%v want 3", res["totalAllPackets"])
	}
	if res["totalPackets"].(int) != 3 {
		t.Errorf("totalPackets=%v want 3", res["totalPackets"])
	}
}

func TestGetAnalyticsRFWithWindowUsesSQLBackend(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000)`)
	ps := NewPacketStore(db, &PacketStoreConfig{AnalyticsSQLBackend: true})
	res, err := ps.GetAnalyticsRFWithWindow("", TimeWindow{})
	if err != nil {
		t.Fatalf("GetAnalyticsRFWithWindow: %v", err)
	}
	if res["totalAllPackets"].(int) != 1 {
		t.Fatalf("totalAllPackets=%v want 1", res["totalAllPackets"])
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

// createParityFileDB creates a file-backed SQLite DB with the full v3 schema
// needed for both loadStore (in-memory path) and computeAnalyticsRFSQL (SQL path).
func createParityFileDB(t testing.TB, dbPath string) {
	t.Helper()
	conn, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("createParityFileDB open: %v", err)
	}
	defer conn.Close()

	stmts := []string{
		`CREATE TABLE nodes (
			public_key TEXT PRIMARY KEY,
			name TEXT, role TEXT, lat REAL, lon REAL,
			last_seen TEXT, first_seen TEXT,
			advert_count INTEGER DEFAULT 0,
			battery_mv INTEGER, temperature_c REAL,
			foreign_advert INTEGER DEFAULT 0
		)`,
		`CREATE TABLE observers (
			id TEXT PRIMARY KEY,
			name TEXT, iata TEXT,
			last_seen TEXT, first_seen TEXT,
			packet_count INTEGER DEFAULT 0,
			model TEXT, firmware TEXT, client_version TEXT,
			radio TEXT, battery_mv INTEGER, uptime_secs INTEGER,
			noise_floor REAL, inactive INTEGER DEFAULT 0,
			last_packet_at TEXT DEFAULT NULL
		)`,
		`CREATE TABLE transmissions (
			id INTEGER PRIMARY KEY,
			raw_hex TEXT NOT NULL,
			hash TEXT NOT NULL UNIQUE,
			first_seen TEXT NOT NULL,
			route_type INTEGER,
			payload_type INTEGER,
			payload_version INTEGER,
			decoded_json TEXT,
			channel_hash TEXT DEFAULT NULL,
			from_pubkey TEXT DEFAULT NULL
		)`,
		`CREATE TABLE observations (
			id INTEGER PRIMARY KEY,
			transmission_id INTEGER NOT NULL REFERENCES transmissions(id),
			observer_idx INTEGER,
			direction TEXT,
			snr REAL,
			rssi REAL,
			score INTEGER,
			path_json TEXT,
			timestamp INTEGER NOT NULL,
			resolved_path TEXT,
			raw_hex TEXT
		)`,
		`CREATE TABLE schema_version (version INTEGER)`,
		`INSERT INTO schema_version (version) VALUES (3)`,
		`CREATE INDEX IF NOT EXISTS idx_tx_first_seen ON transmissions(first_seen)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("createParityFileDB exec %q: %v", s, err)
		}
	}
}

// seedRFParityData inserts the parity fixture into a DB that was created via createParityFileDB.
// Observers are inserted at explicit rowids (1, 2, 3) so that observer_idx joins work correctly.
// The in-memory Load() deduplicates observations by (obs.id, path_json); without observers,
// all observer_idx values produce NULL obs.id and collapse to one obs per tx.
func seedRFParityData(t *testing.T, db *DB) {
	t.Helper()
	// Insert observers at rowid 1, 2, 3 to match observer_idx values used in observations.
	// Observers 1 and 2 share iata 'SJC'; observer 3 has iata 'LAX'.
	mustExec(t, db, `INSERT INTO observers(rowid,id,name,iata) VALUES
		(1,'obs1','Alpha','SJC'),
		(2,'obs2','Bravo','SJC'),
		(3,'obs3','Charlie','LAX')`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type) VALUES
		(1,'aabbcc','h1','2026-05-18T10:00:00Z',1),
		(2,'ddee','h2','2026-05-18T10:30:00Z',1),
		(3,'ff00112233','h3','2026-05-18T23:00:00Z',2),
		(4,'4455','h4','2026-05-17T09:00:00Z',3)`)
	// observer_idx references observers.rowid. Use 1,2,3 instead of 0,1,2
	// so every idx maps to an existing observer (rowid=0 is not standard in SQLite).
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp) VALUES
		(1,1,5.0,-80.0,1779444000),(1,2,7.5,-88.0,1779444000),
		(2,1,6.0,-82.0,1779445800),(2,2,NULL,-95.0,1779445800),
		(3,1,9.0,-70.0,1779490800),(3,3,9.0,NULL,1779490800),
		(4,2,3.0,-101.0,1779354000)`)
}

func toF64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

func assertHistogramEqual(t *testing.T, label, key string, oldH, sqlH interface{}) {
	t.Helper()
	om := oldH.(map[string]interface{})
	sm := sqlH.(map[string]interface{})
	ob, _ := om["bins"].([]map[string]interface{})
	sb, _ := sm["bins"].([]map[string]interface{})
	if len(ob) != len(sb) {
		t.Errorf("[%s] %s: bin count old=%d sql=%d", label, key, len(ob), len(sb))
		return
	}
	for i := range ob {
		if fmt.Sprint(ob[i]["count"]) != fmt.Sprint(sb[i]["count"]) {
			t.Errorf("[%s] %s bin %d: old count=%v sql=%v", label, key, i,
				ob[i]["count"], sb[i]["count"])
		}
	}
}

func assertRFParity(t *testing.T, label, region string, window TimeWindow, dbPath string) {
	t.Helper()
	memStore := loadStore(t, dbPath, 0)
	defer memStore.db.conn.Close()
	old := memStore.computeAnalyticsRF(region, window)
	sqlRes, err := computeAnalyticsRFSQL(memStore.db, region, window)
	if err != nil {
		t.Fatalf("[%s] SQL backend error: %v", label, err)
	}
	for _, k := range []string{"totalPackets", "totalAllPackets", "totalTransmissions",
		"minPacketSize", "maxPacketSize", "avgPacketSize"} {
		if fmt.Sprint(old[k]) != fmt.Sprint(sqlRes[k]) {
			t.Errorf("[%s] %s: old=%v sql=%v", label, k, old[k], sqlRes[k])
		}
	}
	for _, grp := range []string{"snr", "rssi"} {
		om := old[grp].(map[string]interface{})
		sm := sqlRes[grp].(map[string]interface{})
		for _, stat := range []string{"min", "max", "avg", "median", "stddev"} {
			of := toF64(om[stat])
			sf := toF64(sm[stat])
			if math.Abs(of-sf) > 1e-9 {
				t.Errorf("[%s] %s.%s: old=%v sql=%v", label, grp, stat, of, sf)
			}
		}
	}
	for _, k := range []string{"snrValues", "rssiValues", "packetSizes"} {
		assertHistogramEqual(t, label, k, old[k], sqlRes[k])
	}
}

func TestRFAnalyticsParity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "parity.db")

	// Create the file-backed v3-schema DB and seed it.
	createParityFileDB(t, dbPath)
	openDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open for seeding: %v", err)
	}
	seedDB := &DB{conn: openDB}
	seedRFParityData(t, seedDB)
	openDB.Close()

	cases := []struct {
		label  string
		region string
		window TimeWindow
	}{
		{"no-region/zero-window", "", TimeWindow{}},
		{"no-region/24h", "", TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}},
		{"region-SJC/zero-window", "SJC", TimeWindow{}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			assertRFParity(t, c.label, c.region, c.window, dbPath)
		})
	}
}

func TestRFAnalyticsParityEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "parity_empty.db")

	// File-backed empty DB (schema only, no rows).
	createParityFileDB(t, dbPath)

	assertRFParity(t, "empty-dataset", "", TimeWindow{}, dbPath)
}
