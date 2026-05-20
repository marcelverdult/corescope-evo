package main

import (
	"testing"
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
	// 3 GPS-bearing nodes: A (repeater) → B (repeater) → C (client).
	// Sender is A; path is [B, C]. Distances roughly:
	//   A(52,4) → B(53,5)  ≈ 127 km   R↔R
	//   B(53,5) → C(54,6)  ≈ 124 km   C↔R
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
	// Global row (observer_idx=-1) should have 2 hops total.
	var n int
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=-1`,
		"2026-05-18T10").Scan(&n)
	if n != 2 {
		t.Fatalf("global hop count=%d want 2", n)
	}
	// Per-observer row (oi=7) should also have 2 hops.
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=7`,
		"2026-05-18T10").Scan(&n)
	if n != 2 {
		t.Fatalf("per-observer hop count=%d want 2", n)
	}
	// Per-type breakdown on the global row: 1 R↔R + 1 C↔R.
	var rr, cr int
	rw.QueryRow(`SELECT COALESCE(count,0) FROM distance_hourly WHERE hour=? AND observer_idx=-1 AND type='R↔R'`,
		"2026-05-18T10").Scan(&rr)
	rw.QueryRow(`SELECT COALESCE(count,0) FROM distance_hourly WHERE hour=? AND observer_idx=-1 AND type='C↔R'`,
		"2026-05-18T10").Scan(&cr)
	if rr != 1 || cr != 1 {
		t.Fatalf("R↔R=%d C↔R=%d want 1 and 1", rr, cr)
	}
	// distance_paths: one row with total_dist ≈ 127+124 = ~251 km.
	var total float64
	var hopCount int
	rw.QueryRow(`SELECT total_dist, hop_count FROM distance_paths WHERE tx_id=1`).Scan(&total, &hopCount)
	if hopCount != 2 {
		t.Fatalf("hop_count=%d want 2", hopCount)
	}
	if total < 240 || total > 260 {
		t.Fatalf("total_dist=%.2f want ~251 km", total)
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
	if n != 2 {
		t.Fatalf("after rerun global=%d want 2", n)
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
		{"sender + 2 hops", `["bb","cc"]`, ``, "aa", []string{"A", "B", "C"}},
		{"resolved overrides raw", `["AA","BB"]`, `["aa",null]`, "", []string{"A", "B"}},
		{"no-GPS hop skipped", `["dd","cc"]`, ``, "aa", []string{"A", "C"}},
		{"unknown sender ignored", `["aa","bb"]`, ``, "zz", []string{"A", "B"}},
		{"empty path", `[]`, ``, "aa", nil},
		{"single GPS node", `[]`, ``, "aa", nil}, // < 2 nodes → caller drops
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
