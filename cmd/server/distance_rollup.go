// distance_rollup.go — distance analytics rollup: schema, hop helpers,
// single-hour recompute. See .specs/2026-05-20-distance-rollup-design.md.
// Mirrors rf_rollup.go / node_rollup.go.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Fixed-bin distance histogram: 0..300 km, 12 km width = 25 bins. Values
// outside the range clamp to the end bins. Matches the in-memory
// computeAnalyticsDistance 25-bin count, with fixed (not dynamic) edges.
const (
	distDistBinMin, distDistBinWidth, distDistBinCount = 0, 12, 25
	distSnrBinMin, distSnrBinWidth, distSnrBinCount    = -30, 1, 50
)

// distNode is the per-pubkey lookup the recompute uses to resolve hops to
// GPS-bearing nodes.
type distNode struct {
	Name   string
	Role   string
	Lat    float64
	Lon    float64
	HasGPS bool
}

// distanceLoadNodeMap fetches all nodes with role + GPS, keyed by pubkey.
// The recompute calls this once per hour, not per tx.
func distanceLoadNodeMap(db *sql.DB) (map[string]*distNode, error) {
	rows, err := db.Query(`SELECT public_key, name, role, lat, lon FROM nodes`)
	if err != nil {
		return nil, fmt.Errorf("distance load nodes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]*distNode, 1024)
	for rows.Next() {
		var pk string
		var name, role sql.NullString
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&pk, &name, &role, &lat, &lon); err != nil {
			return nil, fmt.Errorf("distance load nodes scan: %w", err)
		}
		n := &distNode{Name: name.String, Role: role.String}
		if lat.Valid && lon.Valid {
			n.Lat = lat.Float64
			n.Lon = lon.Float64
			n.HasGPS = true
		}
		out[pk] = n
	}
	return out, rows.Err()
}

// distanceHopChain reconstructs the chain of GPS-bearing nodes for one
// observation: sender (if GPS-known) followed by every positional hop that
// resolves to a GPS-known node. Returns the chain in path order. A chain of
// fewer than 2 nodes yields no haversine pairs; callers must check.
func distanceHopChain(pathJSON, resolvedPath, senderPk string, nodeByPk map[string]*distNode) []*distNode {
	raw := parsePathJSON(pathJSON)
	if len(raw) == 0 {
		return nil
	}
	var resolved []*string
	if resolvedPath != "" {
		_ = json.Unmarshal([]byte(resolvedPath), &resolved)
	}
	chain := make([]*distNode, 0, len(raw)+1)
	if s, ok := nodeByPk[senderPk]; ok && s != nil && s.HasGPS {
		chain = append(chain, s)
	}
	for i, rawHop := range raw {
		pk := strings.ToLower(rawHop)
		if i < len(resolved) && resolved[i] != nil && *resolved[i] != "" {
			pk = strings.ToLower(*resolved[i])
		}
		if n, ok := nodeByPk[pk]; ok && n != nil && n.HasGPS {
			chain = append(chain, n)
		}
	}
	return chain
}

// distanceClassify returns "R↔R" / "C↔R" / "C↔C" from two roles. The rule
// is: role contains "repeater" (case-insensitive) → R, else C.
func distanceClassify(roleA, roleB string) string {
	aRep := strings.Contains(strings.ToLower(roleA), "repeater")
	bRep := strings.Contains(strings.ToLower(roleB), "repeater")
	switch {
	case aRep && bRep:
		return "R↔R"
	case !aRep && !bRep:
		return "C↔C"
	default:
		return "C↔R"
	}
}

// distancePairKey returns the unordered-pair key "<min>|<max>".
func distancePairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// ensureDistanceRollupTable creates the distance rollup tables. Idempotent.
func ensureDistanceRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for distance_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS distance_hourly (
			hour TEXT NOT NULL,
			type TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			dist_sum REAL NOT NULL DEFAULT 0,
			dist_min REAL,
			dist_max REAL,
			dist_bins BLOB,
			PRIMARY KEY (hour, type, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_hourly_hour ON distance_hourly(hour)`,
		`CREATE TABLE IF NOT EXISTS distance_pair_hourly (
			hour TEXT NOT NULL,
			pair_key TEXT NOT NULL,
			type TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			best_dist REAL NOT NULL DEFAULT 0,
			best_from_name TEXT,
			best_from_pk TEXT,
			best_to_name TEXT,
			best_to_pk TEXT,
			best_hash TEXT,
			best_timestamp TEXT,
			snr_max REAL,
			snr_bins BLOB,
			PRIMARY KEY (hour, pair_key, type, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_pair_hourly_hour ON distance_pair_hourly(hour)`,
		`CREATE TABLE IF NOT EXISTS distance_paths (
			hour TEXT NOT NULL,
			tx_id INTEGER PRIMARY KEY,
			total_dist REAL NOT NULL DEFAULT 0,
			hop_count INTEGER NOT NULL DEFAULT 0,
			hash TEXT,
			timestamp TEXT,
			hops_json TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_paths_hour_dist ON distance_paths(hour, total_dist DESC)`,
		`CREATE TABLE IF NOT EXISTS distance_path_observers (
			tx_id INTEGER NOT NULL,
			observer_idx INTEGER NOT NULL,
			PRIMARY KEY (tx_id, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_path_observers_obs ON distance_path_observers(observer_idx)`,
		`CREATE TABLE IF NOT EXISTS distance_rollup_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("distance_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}
