package main

import (
	"testing"
	"time"
)

func TestEnsureNodeRollupTable(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatalf("ensureNodeRollupTable: %v", err)
	}
	for _, tbl := range []string{"node_rollup", "node_rollup_total", "node_rollup_meta"} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}

func TestNodeHopKeys(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		resolved string
		want     []string
	}{
		{"raw only", `["ab","cd"]`, ``, []string{"ab", "cd"}},
		{"resolved overrides", `["ab","cd"]`, `["ABCDEF",null]`, []string{"abcdef", "cd"}},
		{"null resolved falls back", `["ab"]`, `[null]`, []string{"ab"}},
		{"short resolved falls back", `["ab","cd"]`, `["ABCDEF"]`, []string{"abcdef", "cd"}},
		{"dedup within observation", `["ab","ab"]`, ``, []string{"ab"}},
		{"empty path", `[]`, ``, nil},
		{"empty resolved string", `["ab"]`, ``, []string{"ab"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nodeHopKeys(c.path, c.resolved)
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

func TestNodeRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json)
		VALUES (1,1,1779098400,'["ab"]')`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if !(!nodeRollupReady(rw)) {
		t.Fatal("rollup should not be ready before first run")
	}
	if err := runNodeRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	if !nodeRollupReady(rw) {
		t.Fatal("rollup should be ready after first run")
	}
	var ab int
	rw.QueryRow(`SELECT COALESCE(relay_count,0) FROM node_rollup WHERE hop_key='ab'`).Scan(&ab)
	if ab != 1 {
		t.Fatalf("after run 1 relay_count=%d want 1", ab)
	}
	// New observation on a new transmission in the same hour.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (2,'bb','h2','2026-05-18T10:20:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json)
		VALUES (2,1,1779099600,'["ab"]')`)
	if err := runNodeRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(relay_count,0) FROM node_rollup WHERE hop_key='ab'`).Scan(&ab)
	if ab != 2 {
		t.Fatalf("after run 2 relay_count=%d want 2", ab)
	}
}

func TestRecomputeNodeRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Two non-advert transmissions in 2026-05-18T10, one advert (ignored).
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type) VALUES
		(1,'aa','h1','2026-05-18T10:00:00Z',1),
		(2,'bb','h2','2026-05-18T10:30:00Z',2),
		(3,'cc','h3','2026-05-18T10:45:00Z',4)`)
	// tx1 path ab,cd ; tx2 path ab ; tx3 (advert) path ab.
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json) VALUES
		(1,1,1779098400,'["ab","cd"]'),
		(2,1,1779100200,'["ab"]'),
		(3,1,1779100500,'["ab"]')`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeNodeRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	// hop "ab": tx1 + tx2 = 2 (advert tx3 excluded). hop "cd": tx1 = 1.
	var ab, cd int
	rw.QueryRow(`SELECT relay_count FROM node_rollup WHERE hour=? AND hop_key='ab'`, "2026-05-18T10").Scan(&ab)
	rw.QueryRow(`SELECT relay_count FROM node_rollup WHERE hour=? AND hop_key='cd'`, "2026-05-18T10").Scan(&cd)
	if ab != 2 || cd != 1 {
		t.Fatalf("ab=%d cd=%d want 2 and 1", ab, cd)
	}
	var lastAb string
	rw.QueryRow(`SELECT last_relayed FROM node_rollup WHERE hour=? AND hop_key='ab'`, "2026-05-18T10").Scan(&lastAb)
	if lastAb != "2026-05-18T10:30:00Z" {
		t.Fatalf("last_relayed ab=%q want 2026-05-18T10:30:00Z", lastAb)
	}
	// n_nonadvert: tx1 + tx2 = 2.
	var total int
	rw.QueryRow(`SELECT n_nonadvert FROM node_rollup_total WHERE hour=?`, "2026-05-18T10").Scan(&total)
	if total != 2 {
		t.Fatalf("n_nonadvert=%d want 2", total)
	}
	// Idempotent: a second run yields the same numbers.
	if err := recomputeNodeRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute 2: %v", err)
	}
	rw.QueryRow(`SELECT relay_count FROM node_rollup WHERE hour=? AND hop_key='ab'`, "2026-05-18T10").Scan(&ab)
	if ab != 2 {
		t.Fatalf("after rerun ab=%d want 2", ab)
	}
}

func TestComputeNodeRelayFromRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Current hour so the 24h/7d windows include it.
	hour := time.Now().UTC().Format("2006-01-02T15")
	fs := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	// node "ab00...": appears as hop "ab" in 3 non-advert tx this hour.
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`INSERT INTO node_rollup(hour,hop_key,relay_count,last_relayed)
		VALUES (?,?,?,?)`, hour, "ab", 3, fs); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`INSERT INTO node_rollup_total(hour,n_nonadvert) VALUES (?,?)`,
		hour, 12); err != nil {
		t.Fatal(err)
	}
	pk := "ab00000000000000000000000000000000000000000000000000000000000000"
	res, err := computeNodeRelayFromRollup(db, []string{pk}, 24)
	if err != nil {
		t.Fatalf("computeNodeRelayFromRollup: %v", err)
	}
	r, ok := res[pk]
	if !ok {
		t.Fatalf("missing pubkey result")
	}
	if r.Relay.RelayCount24h != 3 {
		t.Errorf("RelayCount24h=%d want 3", r.Relay.RelayCount24h)
	}
	if !r.Relay.RelayActive {
		t.Errorf("RelayActive=false want true")
	}
	if r.Relay.LastRelayed != fs {
		t.Errorf("LastRelayed=%q want %q", r.Relay.LastRelayed, fs)
	}
	want := 3.0 / 12.0
	if r.Usefulness < want-0.0001 || r.Usefulness > want+0.0001 {
		t.Errorf("Usefulness=%v want %v", r.Usefulness, want)
	}
}
