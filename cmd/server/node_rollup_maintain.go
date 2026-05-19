// node_rollup_maintain.go — node-health rollup backfill + incremental
// maintenance. Mirrors rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

const nodeRollupWatermarkKey = "node_rollup_last_obs_id"

func nodeRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM node_rollup_meta WHERE key=?`,
		nodeRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read node watermark: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func nodeSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO node_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		nodeRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set node watermark: %w", err)
	}
	return nil
}

// nodeRollupMaintMu serializes backfill vs the periodic maintenance job.
var nodeRollupMaintMu sync.Mutex

func runNodeRollupMaintenanceGuarded(rw *sql.DB) {
	if !nodeRollupMaintMu.TryLock() {
		log.Printf("[node-rollup] maintenance skipped — run already in progress")
		return
	}
	defer nodeRollupMaintMu.Unlock()
	if err := runNodeRollupMaintenance(rw); err != nil {
		log.Printf("[node-rollup] maintenance: %v", err)
	}
}

// runNodeRollupMaintenance recomputes every hour bucket whose transmissions
// were touched by observations newer than the watermark, then advances it.
// The watermark tracks observations.id so a new observation on an OLD
// transmission still re-rolls that transmission's hour.
func runNodeRollupMaintenance(rw *sql.DB) error {
	wm, err := nodeRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', t.first_seen)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		WHERE o.id > ?`, wm)
	if err != nil {
		return fmt.Errorf("node maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("node maintenance scan hour: %w", err)
		}
		if h.Valid && h.String != "" {
			hours = append(hours, h.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("node maintenance touched-hours err: %w", err)
	}
	for _, h := range hours {
		if err := recomputeNodeRollupHour(rw, h); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxID); err != nil {
		return fmt.Errorf("node maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return nodeSetRollupWatermark(rw, maxID.Int64)
	}
	// Still record the watermark key on an empty DB so nodeRollupReady flips.
	if !maxID.Valid {
		return nodeSetRollupWatermark(rw, 0)
	}
	return nil
}

// backfillNodeRollupAsync runs the first full rollup build in the background.
// Backfill is maintenance from watermark 0 (all hours touched).
func backfillNodeRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[node-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[node-rollup] backfill open rw: %v", err)
		return
	}
	nodeRollupMaintMu.Lock()
	defer nodeRollupMaintMu.Unlock()
	start := time.Now()
	if err := runNodeRollupMaintenance(rw); err != nil {
		log.Printf("[node-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[node-rollup] backfill complete in %s", time.Since(start))
}

// nodeRollupReady reports whether the rollup has been populated at least once.
func nodeRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM node_rollup_meta WHERE key=?`,
		nodeRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
