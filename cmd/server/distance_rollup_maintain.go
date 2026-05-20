// distance_rollup_maintain.go — distance rollup backfill + incremental
// maintenance. Mirrors node_rollup_maintain.go / rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

const distanceRollupWatermarkKey = "distance_rollup_last_obs_id"

func distanceRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM distance_rollup_meta WHERE key=?`,
		distanceRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read distance watermark: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func distanceSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO distance_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		distanceRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set distance watermark: %w", err)
	}
	return nil
}

var distanceRollupMaintMu sync.Mutex

func runDistanceRollupMaintenanceGuarded(rw *sql.DB) {
	if !distanceRollupMaintMu.TryLock() {
		log.Printf("[distance-rollup] maintenance skipped — run already in progress")
		return
	}
	defer distanceRollupMaintMu.Unlock()
	if err := runDistanceRollupMaintenance(rw); err != nil {
		log.Printf("[distance-rollup] maintenance: %v", err)
	}
}

// runDistanceRollupMaintenance recomputes every hour bucket whose
// transmissions were touched by observations newer than the watermark, then
// advances it. Watermark on observations.id — a new observation on an old
// transmission can change its observer coverage or its representative path.
func runDistanceRollupMaintenance(rw *sql.DB) error {
	wm, err := distanceRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', t.first_seen)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		WHERE o.id > ?`, wm)
	if err != nil {
		return fmt.Errorf("distance maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("distance maintenance scan hour: %w", err)
		}
		if h.Valid && h.String != "" {
			hours = append(hours, h.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("distance maintenance touched-hours err: %w", err)
	}
	for _, h := range hours {
		if err := recomputeDistanceRollupHour(rw, h); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxID); err != nil {
		return fmt.Errorf("distance maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return distanceSetRollupWatermark(rw, maxID.Int64)
	}
	if !maxID.Valid {
		return distanceSetRollupWatermark(rw, 0)
	}
	return nil
}

func backfillDistanceRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[distance-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[distance-rollup] backfill open rw: %v", err)
		return
	}
	distanceRollupMaintMu.Lock()
	defer distanceRollupMaintMu.Unlock()
	start := time.Now()
	if err := runDistanceRollupMaintenance(rw); err != nil {
		log.Printf("[distance-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[distance-rollup] backfill complete in %s", time.Since(start))
}

func distanceRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM distance_rollup_meta WHERE key=?`,
		distanceRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
