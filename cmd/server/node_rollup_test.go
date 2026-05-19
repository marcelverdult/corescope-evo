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
