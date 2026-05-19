package main

import (
	"testing"
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
