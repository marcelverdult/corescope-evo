// distance_rollup.go — distance analytics rollup: schema, hop helpers,
// single-hour recompute. See .specs/2026-05-20-distance-rollup-design.md.
// Mirrors rf_rollup.go / node_rollup.go.

package main

import (
	"fmt"
)

// Fixed-bin distance histogram: 0..300 km, 12 km width = 25 bins. Values
// outside the range clamp to the end bins. Matches the in-memory
// computeAnalyticsDistance 25-bin count, with fixed (not dynamic) edges.
const (
	distDistBinMin, distDistBinWidth, distDistBinCount = 0, 12, 25
	distSnrBinMin, distSnrBinWidth, distSnrBinCount    = -30, 1, 50
)

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
