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
