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
