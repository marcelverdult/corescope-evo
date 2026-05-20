package main

import (
	"fmt"
	"testing"
	"time"
)

func TestEnsureDistanceRollupTable(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatalf("ensureDistanceRollupTable: %v", err)
	}
	for _, tbl := range []string{
		"distance_hourly", "distance_pair_hourly", "distance_paths",
		"distance_path_observers", "distance_rollup_meta",
	} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}

func TestRecomputeDistanceRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// 3 GPS-bearing nodes: A (repeater, sender) → B (repeater) → C (client).
	// C is a client so canAppearInPath rejects it as a path hop.
	// Only the A→B hop (R↔R) is valid; distances roughly:
	//   A(52,4) → B(53,5)  ≈ 127 km   R↔R
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0),
		('cc','C','client',54.0,6.0)`)
	// Sender pubkey 'aa' encoded in decoded_json's pubKey field.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json) VALUES
		(1,'aa','h1','2026-05-18T10:00:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr) VALUES
		(1,7,1779098400,'["bb","cc"]',5.0)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeDistanceRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	// Global row (observer_idx=-1) should have 1 hop total (A→B only).
	var n int
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=-1`,
		"2026-05-18T10").Scan(&n)
	if n != 1 {
		t.Fatalf("global hop count=%d want 1", n)
	}
	// Per-observer row (oi=7) should also have 1 hop.
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=7`,
		"2026-05-18T10").Scan(&n)
	if n != 1 {
		t.Fatalf("per-observer hop count=%d want 1", n)
	}
	// Per-type breakdown on the global row: 1 R↔R, 0 C↔R.
	var rr int
	rw.QueryRow(`SELECT COALESCE(count,0) FROM distance_hourly WHERE hour=? AND observer_idx=-1 AND type='R↔R'`,
		"2026-05-18T10").Scan(&rr)
	if rr != 1 {
		t.Fatalf("R↔R=%d want 1", rr)
	}
	// distance_paths: one row with total_dist ≈ 127 km, hop_count=1.
	var total float64
	var hopCount int
	rw.QueryRow(`SELECT total_dist, hop_count FROM distance_paths WHERE tx_id=1`).Scan(&total, &hopCount)
	if hopCount != 1 {
		t.Fatalf("hop_count=%d want 1", hopCount)
	}
	if total < 120 || total > 135 {
		t.Fatalf("total_dist=%.2f want ~127 km", total)
	}
	// distance_path_observers: one row (tx_id=1, observer_idx=7).
	var oi int
	rw.QueryRow(`SELECT observer_idx FROM distance_path_observers WHERE tx_id=1`).Scan(&oi)
	if oi != 7 {
		t.Fatalf("path_observers oi=%d want 7", oi)
	}
	// Idempotent: second run yields same numbers.
	if err := recomputeDistanceRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=-1`,
		"2026-05-18T10").Scan(&n)
	if n != 1 {
		t.Fatalf("after rerun global=%d want 1", n)
	}
}

func TestDistanceRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0)`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (1,1,1779098400,'["bb"]',5.0)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if distanceRollupReady(rw) {
		t.Fatal("rollup should not be ready before first run")
	}
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	if !distanceRollupReady(rw) {
		t.Fatal("rollup should be ready after first run")
	}
	var n int
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE observer_idx=-1`).Scan(&n)
	if n != 1 {
		t.Fatalf("after run 1 hop count=%d want 1", n)
	}
	// Second transmission in the same hour with a new path.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (2,'bb','h2','2026-05-18T10:20:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (2,1,1779099600,'["bb"]',6.0)`)
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE observer_idx=-1`).Scan(&n)
	if n != 2 {
		t.Fatalf("after run 2 hop count=%d want 2", n)
	}
}

func TestComputeAnalyticsDistanceFromRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0)`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (1,1,1779098400,'["bb"]',5.0)`)
	rw, _ := cachedRW(db.path)
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}
	res, err := computeAnalyticsDistanceFromRollup(db, "", win)
	if err != nil {
		t.Fatalf("computeAnalyticsDistanceFromRollup: %v", err)
	}
	for _, k := range []string{"summary", "topHops", "topPaths", "catStats", "distHistogram", "distOverTime"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	sum := res["summary"].(map[string]interface{})
	if sum["totalHops"].(int) != 1 {
		t.Errorf("totalHops=%v want 1", sum["totalHops"])
	}
	if sum["totalPaths"].(int) != 1 {
		t.Errorf("totalPaths=%v want 1", sum["totalPaths"])
	}
	tops := res["topHops"].([]map[string]interface{})
	if len(tops) != 1 {
		t.Errorf("topHops len=%d want 1", len(tops))
	}
	paths := res["topPaths"].([]map[string]interface{})
	if len(paths) != 1 {
		t.Errorf("topPaths len=%d want 1", len(paths))
	}
}

func TestDistanceRollupParity(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0),
		('cc','C','client',54.0,6.0)`)
	// One tx in the current hour; cc is a client so only bb qualifies as a
	// path hop → chain is [aa, bb], one hop.
	now := time.Now().UTC()
	fs := now.Format("2006-01-02T15:04:05Z")
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1',?,1,'{"pubKey":"aa"}')`, fs)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (1,1,?,'["bb","cc"]',5.0)`, now.Unix())
	rw, _ := cachedRW(db.path)
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}

	// In-memory reference.
	ps := loadStore(t, db.path, 0)
	memRes := ps.GetAnalyticsDistanceWithWindow("",
		TimeWindow{Since: now.Add(-1 * time.Hour).Format(time.RFC3339),
			Until: now.Add(1 * time.Hour).Format(time.RFC3339)})

	// Rollup path.
	ps.analyticsSQLBackend = true
	// Drop the cached in-memory result so the flag path takes over.
	ps.cacheMu.Lock()
	ps.distCache = map[string]*cachedResult{}
	ps.cacheMu.Unlock()
	rollupRes := ps.GetAnalyticsDistanceWithWindow("",
		TimeWindow{Since: now.Add(-1 * time.Hour).Format(time.RFC3339),
			Until: now.Add(1 * time.Hour).Format(time.RFC3339)})

	memTotal := memRes["summary"].(map[string]interface{})["totalHops"].(int)
	rollupTotal := rollupRes["summary"].(map[string]interface{})["totalHops"].(int)
	if memTotal != rollupTotal {
		t.Fatalf("totalHops rollup=%d in-memory=%d", rollupTotal, memTotal)
	}
	if rollupTotal != 1 {
		t.Fatalf("totalHops=%d want 1", rollupTotal)
	}
	memPaths := memRes["summary"].(map[string]interface{})["totalPaths"].(int)
	rollupPaths := rollupRes["summary"].(map[string]interface{})["totalPaths"].(int)
	if memPaths != rollupPaths {
		t.Fatalf("totalPaths rollup=%d in-memory=%d", rollupPaths, memPaths)
	}
	if rollupPaths != 1 {
		t.Fatalf("totalPaths=%d want 1", rollupPaths)
	}
}

func TestDistanceHopChain(t *testing.T) {
	nodes := map[string]*distNode{
		"aa": {Name: "A", Role: "repeater", Lat: 52.0, Lon: 4.0, HasGPS: true},
		"bb": {Name: "B", Role: "repeater", Lat: 53.0, Lon: 5.0, HasGPS: true},
		"cc": {Name: "C", Role: "client", Lat: 54.0, Lon: 6.0, HasGPS: true},
		"dd": {Name: "D", Role: "client", HasGPS: false}, // no GPS, skipped
	}
	cases := []struct {
		name, path, resolved, sender string
		want                         []string // names in chain order
	}{
		// sender is exempt from canAppearInPath; path hop cc (client) is filtered out.
		{"sender + 1 repeater hop", `["bb","cc"]`, ``, "aa", []string{"A", "B"}},
		{"resolved overrides raw", `["AA","BB"]`, `["aa",null]`, "", []string{"A", "B"}},
		// dd has no GPS (skipped) and cc is a client (filtered by canAppearInPath).
		{"no-GPS and client hops skipped", `["dd","cc"]`, ``, "aa", []string{"A"}},
		{"unknown sender ignored", `["aa","bb"]`, ``, "zz", []string{"A", "B"}},
		{"empty path", `[]`, ``, "aa", nil},
		{"single GPS node", `[]`, ``, "aa", nil}, // < 2 nodes → caller drops
		// Explicit: client role is rejected as a path hop even with GPS.
		{"client hop is skipped", `["cc"]`, ``, "aa", []string{"A"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chain := distanceHopChain(c.path, c.resolved, c.sender, nodes)
			got := make([]string, len(chain))
			for i, n := range chain {
				got[i] = n.Name
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
			}
		})
	}
}

func TestDistanceRollupPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	rw, _ := cachedRW(db.path)
	// 20 GPS-bearing REPEATERS around the Netherlands. All role="repeater"
	// so distanceHopChain (which requires canAppearInPath) admits them.
	for i := 0; i < 20; i++ {
		pk := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", i)
		lat := 51.5 + float64(i%5)*0.4
		lon := 4.0 + float64(i/5)*0.4
		mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES (?,?,?,?,?)`,
			pk, fmt.Sprintf("N%d", i), "repeater", lat, lon)
	}
	// ~30k transmissions spread over 7 days; each path has 2 hops cycling
	// among the 20 nodes.
	base := time.Now().UTC().Add(-6 * 24 * time.Hour)
	tx, err := rw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30000; i++ {
		ts := base.Add(time.Duration(i) * 18 * time.Second)
		fs := ts.Format("2006-01-02T15:04:05Z")
		sender := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", i%20)
		hop1 := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", (i+7)%20)
		hop2 := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", (i+11)%20)
		if _, err := tx.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
			VALUES (?,?,?,?,1,?)`, i, "aa", fmt.Sprintf("h%d", i), fs, `{"pubKey":"`+sender+`"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
			VALUES (?,1,?,?,5.0)`, i, ts.Unix(), `["`+hop1+`","`+hop2+`"]`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}

	win := TimeWindow{
		Since: base.Format(time.RFC3339),
		Until: time.Now().UTC().Format(time.RFC3339),
	}
	start := time.Now()
	res, err := computeAnalyticsDistanceFromRollup(db, "", win)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res["summary"]; !ok {
		t.Fatal("missing summary")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("rollup read took %s, want < 500ms", elapsed)
	}
	t.Logf("distance rollup read over 30k tx: %s", elapsed)
}
