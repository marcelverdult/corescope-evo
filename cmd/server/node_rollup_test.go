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
