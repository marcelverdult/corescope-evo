// node_rollup.go — node-health rollup: schema, hop-key extraction,
// single-hour recompute. See .specs/2026-05-19-node-rollup-design.md.
// Mirrors rf_rollup.go / channel_rollup.go.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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

// recomputeNodeRollupHour rebuilds node_rollup + node_rollup_total for one
// hour bucket ("2026-05-18T10") from raw non-advert transmissions.
// Idempotent: deletes then re-inserts. The raw read runs OUTSIDE the write
// transaction and filters on the indexed first_seen RFC3339 range.
func recomputeNodeRollupHour(rw *sql.DB, hour string) error {
	ht, err := time.Parse("2006-01-02T15", hour)
	if err != nil {
		return fmt.Errorf("node recompute parse hour %q: %w", hour, err)
	}
	lo := ht.UTC().Format("2006-01-02T15:04:05Z")
	hi := ht.UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")

	rows, err := rw.Query(`
		SELECT t.id, t.first_seen, o.path_json, o.resolved_path
		FROM transmissions t JOIN observations o ON o.transmission_id = t.id
		WHERE t.first_seen >= ? AND t.first_seen < ?
		  AND (t.payload_type IS NULL OR t.payload_type != 4)`, lo, hi)
	if err != nil {
		return fmt.Errorf("node recompute scan: %w", err)
	}
	txByHop := map[string]map[int]bool{}
	lastByHop := map[string]string{}
	for rows.Next() {
		var txID int
		var firstSeen string
		var pathJSON, resolvedPath sql.NullString
		if err := rows.Scan(&txID, &firstSeen, &pathJSON, &resolvedPath); err != nil {
			rows.Close()
			return fmt.Errorf("node recompute row: %w", err)
		}
		for _, key := range nodeHopKeys(pathJSON.String, resolvedPath.String) {
			if txByHop[key] == nil {
				txByHop[key] = map[int]bool{}
			}
			txByHop[key][txID] = true
			if firstSeen > lastByHop[key] {
				lastByHop[key] = firstSeen
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("node recompute rows: %w", err)
	}

	var nNonAdvert int
	if err := rw.QueryRow(`
		SELECT COUNT(*) FROM transmissions
		WHERE first_seen >= ? AND first_seen < ?
		  AND (payload_type IS NULL OR payload_type != 4)`, lo, hi).Scan(&nNonAdvert); err != nil {
		return fmt.Errorf("node recompute count: %w", err)
	}

	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("node recompute begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_rollup WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("node recompute delete rollup: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM node_rollup_total WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("node recompute delete total: %w", err)
	}
	for key, set := range txByHop {
		if _, err := tx.Exec(`INSERT INTO node_rollup(hour,hop_key,relay_count,last_relayed)
			VALUES (?,?,?,?)`, hour, key, len(set), lastByHop[key]); err != nil {
			return fmt.Errorf("node recompute insert rollup: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO node_rollup_total(hour,n_nonadvert) VALUES (?,?)`,
		hour, nNonAdvert); err != nil {
		return fmt.Errorf("node recompute insert total: %w", err)
	}
	return tx.Commit()
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
