// analytics_rf_sql.go — SQL-backed RF analytics (spec 2026-05-19-rf-analytics-sql).

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// rfWindowEpochBounds converts a TimeWindow to inclusive Unix-second bounds for
// the observations.timestamp column. ok=false means the window is the zero
// window (no bounds at all). Unset individual ends use wide sentinels.
func rfWindowEpochBounds(w TimeWindow) (lo, hi int64, ok bool) {
	if w.IsZero() {
		return 0, 0, false
	}
	lo = -62135596800 // year 1, effectively unbounded low
	hi = 253402300799 // year 9999, effectively unbounded high
	if w.Since != "" {
		if t, err := time.Parse(time.RFC3339, w.Since); err == nil {
			lo = t.Unix()
		}
	}
	if w.Until != "" {
		if t, err := time.Parse(time.RFC3339, w.Until); err == nil {
			hi = t.Unix()
		}
	}
	return lo, hi, true
}

// rfObsWindowClause returns a WHERE fragment + args filtering observations by
// epoch-second timestamp. Empty when the window is zero.
func rfObsWindowClause(w TimeWindow) (string, []interface{}) {
	lo, hi, ok := rfWindowEpochBounds(w)
	if !ok {
		return "", nil
	}
	return "o.timestamp >= ? AND o.timestamp <= ?", []interface{}{lo, hi}
}

// rfTxWindowClause returns a WHERE fragment + args filtering transmissions by
// the RFC3339 first_seen string. Empty when the window is zero.
func rfTxWindowClause(w TimeWindow) (string, []interface{}) {
	if w.IsZero() {
		return "", nil
	}
	var parts []string
	var args []interface{}
	if w.Since != "" {
		parts = append(parts, "t.first_seen >= ?")
		args = append(args, w.Since)
	}
	if w.Until != "" {
		parts = append(parts, "t.first_seen <= ?")
		args = append(args, w.Until)
	}
	return strings.Join(parts, " AND "), args
}

// rfObserverIdxClause returns a WHERE fragment + args restricting to a set of
// observer_idx values. Empty when idxs is empty.
func rfObserverIdxClause(idxs []int) (string, []interface{}) {
	if len(idxs) == 0 {
		return "", nil
	}
	ph := make([]string, len(idxs))
	args := make([]interface{}, len(idxs))
	for i, v := range idxs {
		ph[i] = "?"
		args[i] = v
	}
	return fmt.Sprintf("o.observer_idx IN (%s)", strings.Join(ph, ",")), args
}

// rfWhere joins non-empty clauses with AND and concatenates their args.
func rfWhere(clauses []string, argSets [][]interface{}) (string, []interface{}) {
	var parts []string
	var args []interface{}
	for i, c := range clauses {
		if c == "" {
			continue
		}
		parts = append(parts, c)
		args = append(args, argSets[i]...)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(parts, " AND "), args
}

// rfAggregates holds the scalar aggregate results for RF analytics.
type rfAggregates struct {
	NObs      int64
	NSnr      int64
	SnrSum    float64
	SnrSumSq  float64
	SnrMin    float64
	SnrMax    float64
	NRssi     int64
	RssiSum   float64
	RssiSumSq float64
	RssiMin   float64
	RssiMax   float64
	MinTS     int64
	MaxTS     int64
}

// rfCoreAggregates runs the single-scan aggregate query. region="" means no
// region filter (window applies to transmissions.first_seen); a non-empty
// region filters observations by observer_idx and by observations.timestamp.
func rfCoreAggregates(db *DB, region string, window TimeWindow) (rfAggregates, error) {
	var agg rfAggregates
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return agg, err
	}

	var where string
	var args []interface{}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, args = rfWhere([]string{txW}, [][]interface{}{txArgs})
	} else {
		obsW, obsArgs := rfObsWindowClause(window)
		idxW, idxArgs := rfObserverIdxClause(idxs)
		where, args = rfWhere([]string{obsW, idxW}, [][]interface{}{obsArgs, idxArgs})
	}

	q := `SELECT
		COUNT(*),
		COUNT(o.snr), COALESCE(SUM(o.snr),0), COALESCE(SUM(o.snr*o.snr),0),
		COALESCE(MIN(o.snr),0), COALESCE(MAX(o.snr),0),
		COUNT(o.rssi), COALESCE(SUM(o.rssi),0), COALESCE(SUM(o.rssi*o.rssi),0),
		COALESCE(MIN(o.rssi),0), COALESCE(MAX(o.rssi),0),
		COALESCE(MIN(o.timestamp),0), COALESCE(MAX(o.timestamp),0)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		` + where

	row := db.conn.QueryRow(q, args...)
	if err := row.Scan(
		&agg.NObs,
		&agg.NSnr, &agg.SnrSum, &agg.SnrSumSq, &agg.SnrMin, &agg.SnrMax,
		&agg.NRssi, &agg.RssiSum, &agg.RssiSumSq, &agg.RssiMin, &agg.RssiMax,
		&agg.MinTS, &agg.MaxTS,
	); err != nil {
		return agg, fmt.Errorf("rfCoreAggregates scan: %w", err)
	}
	return agg, nil
}

// rfSortedColumn fetches one non-null REAL column ("snr" or "rssi") from
// observations, ascending. Used for medians and histograms.
func rfSortedColumn(db *DB, col, region string, window TimeWindow) ([]float64, error) {
	if col != "snr" && col != "rssi" {
		return nil, fmt.Errorf("rfSortedColumn: unsupported column %q", col)
	}
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	var where string
	var args []interface{}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, args = rfWhere([]string{txW, "o." + col + " IS NOT NULL"},
			[][]interface{}{txArgs, nil})
	} else {
		obsW, obsArgs := rfObsWindowClause(window)
		idxW, idxArgs := rfObserverIdxClause(idxs)
		where, args = rfWhere([]string{obsW, idxW, "o." + col + " IS NOT NULL"},
			[][]interface{}{obsArgs, idxArgs, nil})
	}
	q := `SELECT o.` + col + ` FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id ` + where +
		` ORDER BY o.` + col + ` ASC`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfSortedColumn query: %w", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("rfSortedColumn scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// rfPacketSizes returns one byte-size per transmission (len(raw_hex)/2),
// matching the old "unique by hash" set since hash is UNIQUE.
func rfPacketSizes(db *DB, region string, window TimeWindow) ([]int, error) {
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	var q string
	var args []interface{}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, wArgs := rfWhere([]string{txW, "t.raw_hex != ''"},
			[][]interface{}{txArgs, nil})
		q = `SELECT length(t.raw_hex)/2 FROM transmissions t ` + where
		args = wArgs
	} else {
		obsW, obsArgs := rfObsWindowClause(window)
		idxW, idxArgs := rfObserverIdxClause(idxs)
		where, wArgs := rfWhere([]string{obsW, idxW, "t.raw_hex != ''"},
			[][]interface{}{obsArgs, idxArgs, nil})
		q = `SELECT DISTINCT t.id, length(t.raw_hex)/2 FROM transmissions t
			JOIN observations o ON o.transmission_id = t.id ` + where
		args = wArgs
	}
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfPacketSizes query: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		if region == "" {
			var sz int
			if err := rows.Scan(&sz); err != nil {
				return nil, fmt.Errorf("rfPacketSizes scan: %w", err)
			}
			out = append(out, sz)
		} else {
			var id, sz int
			if err := rows.Scan(&id, &sz); err != nil {
				return nil, fmt.Errorf("rfPacketSizes scan: %w", err)
			}
			out = append(out, sz)
		}
	}
	return out, rows.Err()
}

// rfObsWhere builds the WHERE clause for observation-scoped queries, in both
// region modes. Returns clause (incl. "WHERE") and args.
func rfObsWhere(db *DB, region string, window TimeWindow, extra string) (string, []interface{}, error) {
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return "", nil, err
	}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, args := rfWhere([]string{txW, extra}, [][]interface{}{txArgs, nil})
		return where, args, nil
	}
	obsW, obsArgs := rfObsWindowClause(window)
	idxW, idxArgs := rfObserverIdxClause(idxs)
	where, args := rfWhere([]string{obsW, idxW, extra}, [][]interface{}{obsArgs, idxArgs, nil})
	return where, args, nil
}

// rfTypeDistribution returns payload_type -> distinct-transmission count.
func rfTypeDistribution(db *DB, region string, window TimeWindow) (map[int]int, error) {
	where, args, err := rfObsWhere(db, region, window, "t.payload_type IS NOT NULL")
	if err != nil {
		return nil, err
	}
	q := `SELECT t.payload_type, COUNT(DISTINCT t.id)
		FROM transmissions t JOIN observations o ON o.transmission_id = t.id
		` + where + ` GROUP BY t.payload_type`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfTypeDistribution query: %w", err)
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var pt, c int
		if err := rows.Scan(&pt, &c); err != nil {
			return nil, fmt.Errorf("rfTypeDistribution scan: %w", err)
		}
		out[pt] = c
	}
	return out, rows.Err()
}

// rfTypeStat aggregates snr per payload type.
type rfTypeStat struct {
	Count         int
	Sum, Min, Max float64
}

// rfSnrByType returns payload_type -> snr stats.
func rfSnrByType(db *DB, region string, window TimeWindow) (map[int]rfTypeStat, error) {
	where, args, err := rfObsWhere(db, region, window, "o.snr IS NOT NULL AND t.payload_type IS NOT NULL")
	if err != nil {
		return nil, err
	}
	q := `SELECT t.payload_type, COUNT(o.snr), COALESCE(SUM(o.snr),0),
		COALESCE(MIN(o.snr),0), COALESCE(MAX(o.snr),0)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		` + where + ` GROUP BY t.payload_type`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfSnrByType query: %w", err)
	}
	defer rows.Close()
	out := map[int]rfTypeStat{}
	for rows.Next() {
		var pt, c int
		var st rfTypeStat
		if err := rows.Scan(&pt, &c, &st.Sum, &st.Min, &st.Max); err != nil {
			return nil, fmt.Errorf("rfSnrByType scan: %w", err)
		}
		st.Count = c
		out[pt] = st
	}
	return out, rows.Err()
}

// rfHourBucket holds per-hour counts and snr.
type rfHourBucket struct {
	count    int
	snrCount int
	snrSum   float64
}

// rfHourlyBuckets returns hour ("2026-05-18T10") -> bucket. count is distinct
// transmissions per hour; snrSum/snrCount feed signal-over-time.
func rfHourlyBuckets(db *DB, region string, window TimeWindow) (map[string]rfHourBucket, error) {
	where, args, err := rfObsWhere(db, region, window, "")
	if err != nil {
		return nil, err
	}
	q := `SELECT strftime('%Y-%m-%dT%H', o.timestamp, 'unixepoch') AS hr,
		COUNT(DISTINCT o.transmission_id),
		COUNT(o.snr), COALESCE(SUM(o.snr),0)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		` + where + ` GROUP BY hr`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfHourlyBuckets query: %w", err)
	}
	defer rows.Close()
	out := map[string]rfHourBucket{}
	for rows.Next() {
		var hr string
		var b rfHourBucket
		if err := rows.Scan(&hr, &b.count, &b.snrCount, &b.snrSum); err != nil {
			return nil, fmt.Errorf("rfHourlyBuckets scan: %w", err)
		}
		out[hr] = b
	}
	return out, rows.Err()
}

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

// rfScatterSample returns at most 500 (snr,rssi) points, strided to match the
// old "every Nth of all points in id order" downsampling.
func rfScatterSample(db *DB, region string, window TimeWindow) ([]rfScatterPoint, error) {
	where, args, err := rfObsWhere(db, region, window,
		"o.snr IS NOT NULL AND o.rssi IS NOT NULL")
	if err != nil {
		return nil, err
	}
	var total int
	cq := `SELECT COUNT(*) FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id ` + where
	if err := db.conn.QueryRow(cq, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("rfScatterSample count: %w", err)
	}
	if total == 0 {
		return []rfScatterPoint{}, nil
	}
	stride := 1
	if total > 500 {
		stride = total / 500
	}
	q := `SELECT snr, rssi FROM (
		SELECT o.snr AS snr, o.rssi AS rssi,
		       ROW_NUMBER() OVER (ORDER BY o.id) - 1 AS rn
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id ` + where +
		`) WHERE rn % ? = 0`
	qArgs := append(append([]interface{}{}, args...), stride)
	rows2, err := db.conn.Query(q, qArgs...)
	if err != nil {
		return nil, fmt.Errorf("rfScatterSample query: %w", err)
	}
	defer rows2.Close()
	out2 := make([]rfScatterPoint, 0, 500)
	for rows2.Next() {
		var p rfScatterPoint
		if err := rows2.Scan(&p.SNR, &p.RSSI); err != nil {
			return nil, fmt.Errorf("rfScatterSample scan: %w", err)
		}
		out2 = append(out2, p)
	}
	return out2, rows2.Err()
}

// rfTotalTransmissions returns the distinct-transmission count for the window/region.
func rfTotalTransmissions(db *DB, region string, window TimeWindow) (int, error) {
	where, args, err := rfObsWhere(db, region, window, "")
	if err != nil {
		return 0, err
	}
	q := `SELECT COUNT(DISTINCT t.id) FROM transmissions t
		JOIN observations o ON o.transmission_id = t.id ` + where
	var n int
	if err := db.conn.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("rfTotalTransmissions: %w", err)
	}
	return n, nil
}

func rfTimeSpanHours(minTS, maxTS int64) float64 {
	if minTS == 0 || maxTS == 0 || minTS == maxTS {
		return 0
	}
	return float64(maxTS-minTS) / 3600.0
}

// computeAnalyticsRFSQL is the SQL-backed equivalent of computeAnalyticsRF.
func computeAnalyticsRFSQL(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	agg, err := rfCoreAggregates(db, region, window)
	if err != nil {
		return nil, err
	}
	snrVals, err := rfSortedColumn(db, "snr", region, window)
	if err != nil {
		return nil, err
	}
	rssiVals, err := rfSortedColumn(db, "rssi", region, window)
	if err != nil {
		return nil, err
	}
	sizes, err := rfPacketSizes(db, region, window)
	if err != nil {
		return nil, err
	}
	typeDist, err := rfTypeDistribution(db, region, window)
	if err != nil {
		return nil, err
	}
	snrByType, err := rfSnrByType(db, region, window)
	if err != nil {
		return nil, err
	}
	hourly, err := rfHourlyBuckets(db, region, window)
	if err != nil {
		return nil, err
	}
	scatter, err := rfScatterSample(db, region, window)
	if err != nil {
		return nil, err
	}
	totalTx, err := rfTotalTransmissions(db, region, window)
	if err != nil {
		return nil, err
	}

	ptNames := payloadTypeNames

	snrStats := map[string]interface{}{"min": 0.0, "max": 0.0, "avg": 0.0, "median": 0.0, "stddev": 0.0}
	if len(snrVals) > 0 {
		avg := agg.SnrSum / float64(agg.NSnr)
		snrStats = map[string]interface{}{
			"min": snrVals[0], "max": snrVals[len(snrVals)-1],
			"avg": avg, "median": snrVals[len(snrVals)/2],
			"stddev": rfStddevF64(snrVals, avg),
		}
	}
	rssiStats := map[string]interface{}{"min": 0.0, "max": 0.0, "avg": 0.0, "median": 0.0, "stddev": 0.0}
	if len(rssiVals) > 0 {
		avg := agg.RssiSum / float64(agg.NRssi)
		rssiStats = map[string]interface{}{
			"min": rssiVals[0], "max": rssiVals[len(rssiVals)-1],
			"avg": avg, "median": rssiVals[len(rssiVals)/2],
			"stddev": rfStddevF64(rssiVals, avg),
		}
	}

	type hourCount struct {
		Hour  string `json:"hour"`
		Count int    `json:"count"`
	}
	hourKeys := make([]string, 0, len(hourly))
	for k := range hourly {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	packetsPerHour := make([]hourCount, len(hourKeys))
	signalOverTime := make([]map[string]interface{}, 0, len(hourKeys))
	for i, k := range hourKeys {
		b := hourly[k]
		packetsPerHour[i] = hourCount{Hour: k, Count: b.count}
		if b.snrCount > 0 {
			signalOverTime = append(signalOverTime, map[string]interface{}{
				"hour": k, "count": b.snrCount, "avgSnr": b.snrSum / float64(b.snrCount),
			})
		}
	}

	type ptEntry struct {
		Type  int    `json:"type"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	payloadTypes := make([]ptEntry, 0, len(typeDist))
	for pt, c := range typeDist {
		name := ptNames[pt]
		if name == "" {
			name = fmt.Sprintf("UNK(%d)", pt)
		}
		payloadTypes = append(payloadTypes, ptEntry{Type: pt, Name: name, Count: c})
	}
	sort.Slice(payloadTypes, func(i, j int) bool { return payloadTypes[i].Count > payloadTypes[j].Count })

	type snrTypeEntry struct {
		Name  string  `json:"name"`
		Count int     `json:"count"`
		Avg   float64 `json:"avg"`
		Min   float64 `json:"min"`
		Max   float64 `json:"max"`
	}
	snrByTypeArr := make([]snrTypeEntry, 0, len(snrByType))
	for pt, st := range snrByType {
		name := ptNames[pt]
		if name == "" {
			name = fmt.Sprintf("UNK(%d)", pt)
		}
		avg := 0.0
		if st.Count > 0 {
			avg = st.Sum / float64(st.Count)
		}
		snrByTypeArr = append(snrByTypeArr, snrTypeEntry{
			Name: name, Count: st.Count, Avg: avg, Min: st.Min, Max: st.Max,
		})
	}
	sort.Slice(snrByTypeArr, func(i, j int) bool { return snrByTypeArr[i].Count > snrByTypeArr[j].Count })

	avgPkt, minPkt, maxPkt := 0, 0, 0
	if len(sizes) > 0 {
		sum := 0
		minPkt, maxPkt = sizes[0], sizes[0]
		for _, v := range sizes {
			sum += v
			if v < minPkt {
				minPkt = v
			}
			if v > maxPkt {
				maxPkt = v
			}
		}
		avgPkt = sum / len(sizes)
	}

	timeSpanHours := rfTimeSpanHours(agg.MinTS, agg.MaxTS)

	return map[string]interface{}{
		"totalPackets":       int(agg.NSnr),
		"totalAllPackets":    int(agg.NObs),
		"totalTransmissions": totalTx,
		"snr":                snrStats,
		"rssi":               rssiStats,
		"snrValues":          rfBuildHistogramF64(snrVals, 20),
		"rssiValues":         rfBuildHistogramF64(rssiVals, 20),
		"packetSizes":        rfBuildHistogramInt(sizes, 25),
		"minPacketSize":      minPkt,
		"maxPacketSize":      maxPkt,
		"avgPacketSize":      avgPkt,
		"packetsPerHour":     packetsPerHour,
		"payloadTypes":       payloadTypes,
		"snrByType":          snrByTypeArr,
		"signalOverTime":     signalOverTime,
		"scatterData":        scatter,
		"timeSpanHours":      timeSpanHours,
	}, nil
}
