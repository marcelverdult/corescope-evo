// analytics_rf_sql.go — SQL-backed RF analytics (spec 2026-05-19-rf-analytics-sql).

package main

import (
	"fmt"
)

// rfRegionObserverIdxs returns the observer_idx values whose observers belong
// to the given region. An empty region returns (nil, nil) — caller treats nil
// as "no region filter". The region->observer mapping must mirror the in-memory
// resolveRegionObservers in store.go.
//
// Schema notes: the observers table has no "region" column and no "observer_idx"
// column. Region membership is via the "iata" column (IATA airport code).
// observer_idx in observations is observers.rowid (SQLite implicit rowid). The
// region parameter may be comma-separated (e.g. "SJC,SFO"), matching the
// normalizeRegionCodes convention used by GetObserverIdsForRegion.
func rfRegionObserverIdxs(db *DB, region string) ([]int, error) {
	codes := normalizeRegionCodes(region)
	if len(codes) == 0 {
		return nil, nil
	}
	placeholders := sqlPlaceholders(len(codes))
	args := make([]interface{}, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	rows, err := db.conn.Query(
		fmt.Sprintf(`SELECT rowid FROM observers WHERE UPPER(TRIM(iata)) IN (%s)`, placeholders),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("rfRegionObserverIdxs query: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, fmt.Errorf("rfRegionObserverIdxs scan: %w", err)
		}
		out = append(out, idx)
	}
	return out, rows.Err()
}

// rfScatterPoint mirrors the old scatter point shape.
type rfScatterPoint struct {
	SNR  float64 `json:"snr"`
	RSSI float64 `json:"rssi"`
}

// computeAnalyticsRFSQL produces RF analytics from the rf_rollup table.
func computeAnalyticsRFSQL(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	return computeRFFromRollup(db, region, window)
}
