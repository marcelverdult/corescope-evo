// node_rollup.go — node-health rollup: schema, hop-key extraction,
// single-hour recompute. See .specs/2026-05-19-node-rollup-design.md.
// Mirrors rf_rollup.go / channel_rollup.go.

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ensureNodeRollupTable creates the node-health rollup tables. Idempotent.
func ensureNodeRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for node_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS node_rollup (
			hour TEXT NOT NULL,
			hop_key TEXT NOT NULL,
			relay_count INTEGER NOT NULL DEFAULT 0,
			last_relayed TEXT,
			PRIMARY KEY (hour, hop_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_node_rollup_hop ON node_rollup(hop_key)`,
		`CREATE INDEX IF NOT EXISTS idx_node_rollup_hour ON node_rollup(hour)`,
		`CREATE TABLE IF NOT EXISTS node_rollup_total (
			hour TEXT PRIMARY KEY,
			n_nonadvert INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS node_rollup_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("node_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}

// nodeHopKeys returns the lowercased hop keys for one observation: the
// resolved full pubkey where a hop resolved, else the raw wire hop. Each
// distinct key appears once. Resolution is positional — resolved_path[i]
// corresponds to path_json[i]; a null or missing entry falls back to the
// raw hop.
func nodeHopKeys(pathJSON, resolvedPath string) []string {
	raw := parsePathJSON(pathJSON)
	if len(raw) == 0 {
		return nil
	}
	var resolved []*string
	if resolvedPath != "" {
		_ = json.Unmarshal([]byte(resolvedPath), &resolved)
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for i, rawHop := range raw {
		key := rawHop
		if i < len(resolved) && resolved[i] != nil && *resolved[i] != "" {
			key = *resolved[i]
		}
		key = strings.ToLower(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}
