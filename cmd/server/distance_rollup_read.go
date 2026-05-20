// distance_rollup_read.go — distance analytics result assembly from the
// distance rollup tables.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// computeAnalyticsDistanceFromRollup builds the distance analytics result
// map from the four distance rollup tables. region "" = global; window zero
// → default 24h via rfEffectiveWindow.
func computeAnalyticsDistanceFromRollup(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	eff := rfEffectiveWindow(window)
	sinceHour, untilHour := rfWindowHourBounds(eff)
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	// Observer-set clause for distance_hourly / distance_pair_hourly:
	// region "" → observer_idx = -1 (global deduped); region != "" → IN(region idxs).
	var oiClause string
	var oiArgs []interface{}
	if region == "" {
		oiClause = "observer_idx = ?"
		oiArgs = []interface{}{-1}
	} else {
		oiClause = "observer_idx IN (" + rfIntPlaceholders(len(idxs)) + ")"
		oiArgs = make([]interface{}, len(idxs))
		for i, v := range idxs {
			oiArgs[i] = v
		}
	}
	baseArgs := []interface{}{sinceHour, untilHour}
	allArgs := append(append([]interface{}{}, baseArgs...), oiArgs...)

	// Per-type aggregates + global histogram counts.
	type catAgg struct {
		count            int
		distSum          float64
		distMin, distMax float64
		haveDist         bool
		distBins         []int
	}
	byType := map[string]*catAgg{
		"R↔R": {distBins: make([]int, distDistBinCount)},
		"C↔R": {distBins: make([]int, distDistBinCount)},
		"C↔C": {distBins: make([]int, distDistBinCount)},
	}
	hourlyRows, err := db.conn.Query(
		`SELECT type, count, dist_sum, dist_min, dist_max, dist_bins
		 FROM distance_hourly
		 WHERE hour >= ? AND hour <= ? AND `+oiClause, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("distance hourly read: %w", err)
	}
	for hourlyRows.Next() {
		var typ string
		var cnt int
		var ds float64
		var dmn, dmx sql.NullFloat64
		var bins []byte
		if err := hourlyRows.Scan(&typ, &cnt, &ds, &dmn, &dmx, &bins); err != nil {
			hourlyRows.Close()
			return nil, fmt.Errorf("distance hourly scan: %w", err)
		}
		a := byType[typ]
		if a == nil {
			continue
		}
		a.count += cnt
		a.distSum += ds
		if dmn.Valid {
			if !a.haveDist || dmn.Float64 < a.distMin {
				a.distMin = dmn.Float64
			}
			a.haveDist = true
		}
		if dmx.Valid {
			if dmx.Float64 > a.distMax {
				a.distMax = dmx.Float64
			}
		}
		rfAddBins(a.distBins, rfUnpackBins(bins, distDistBinCount))
	}
	hourlyRows.Close()
	if err := hourlyRows.Err(); err != nil {
		return nil, fmt.Errorf("distance hourly rows: %w", err)
	}

	// Build catStats.
	catStats := map[string]interface{}{}
	totalCount := 0
	totalSum := 0.0
	globalMin, globalMax, haveGlobal := 0.0, 0.0, false
	globalBins := make([]int, distDistBinCount)
	for _, typ := range []string{"R↔R", "C↔R", "C↔C"} {
		a := byType[typ]
		if a.count == 0 {
			catStats[typ] = map[string]interface{}{"count": 0, "avg": 0, "median": 0, "min": 0, "max": 0}
			continue
		}
		avg := a.distSum / float64(a.count)
		med := distMedianFromBins(a.distBins)
		catStats[typ] = map[string]interface{}{
			"count":  a.count,
			"avg":    math.Round(avg*100) / 100,
			"median": math.Round(med*100) / 100,
			"min":    math.Round(a.distMin*100) / 100,
			"max":    math.Round(a.distMax*100) / 100,
		}
		totalCount += a.count
		totalSum += a.distSum
		if !haveGlobal || a.distMin < globalMin {
			globalMin = a.distMin
		}
		if !haveGlobal || a.distMax > globalMax {
			globalMax = a.distMax
		}
		haveGlobal = true
		rfAddBins(globalBins, a.distBins)
	}

	// summary.totalPaths from distance_paths (+ optional region join).
	var totalPaths int
	if region == "" {
		if err := db.conn.QueryRow(
			`SELECT COUNT(*) FROM distance_paths WHERE hour >= ? AND hour <= ?`,
			sinceHour, untilHour).Scan(&totalPaths); err != nil {
			return nil, fmt.Errorf("distance totalPaths: %w", err)
		}
	} else {
		pathRegionArgs := append([]interface{}{sinceHour, untilHour}, oiArgs...)
		if err := db.conn.QueryRow(
			`SELECT COUNT(DISTINCT p.tx_id)
			 FROM distance_paths p
			 JOIN distance_path_observers o ON o.tx_id = p.tx_id
			 WHERE p.hour >= ? AND p.hour <= ?
			   AND o.observer_idx IN (`+rfIntPlaceholders(len(idxs))+`)`,
			pathRegionArgs...).Scan(&totalPaths); err != nil {
			return nil, fmt.Errorf("distance totalPaths region: %w", err)
		}
	}

	summary := map[string]interface{}{
		"totalHops":  totalCount,
		"totalPaths": totalPaths,
		"avgDist":    0.0,
		"maxDist":    0.0,
	}
	if totalCount > 0 {
		summary["avgDist"] = math.Round((totalSum/float64(totalCount))*100) / 100
		summary["maxDist"] = math.Round(globalMax*100) / 100
	}

	// distHistogram (fixed 25 bins).
	var distHistogram interface{} = []interface{}{}
	if totalCount > 0 {
		distHistogram = rfHistogramFromBins(globalBins, distDistBinMin, distDistBinWidth)
		// rfHistogramFromBins returns {bins:[{x,w,count}], min, max} keyed; for
		// distance the in-memory shape uses the raw dist min/max — overlay them.
		if m, ok := distHistogram.(map[string]interface{}); ok {
			m["min"] = math.Round(globalMin*100) / 100
			m["max"] = math.Round(globalMax*100) / 100
			distHistogram = m
		}
	}

	// distOverTime: GROUP BY hour, sum count + dist_sum.
	timeRows, err := db.conn.Query(
		`SELECT hour, SUM(count), SUM(dist_sum)
		 FROM distance_hourly
		 WHERE hour >= ? AND hour <= ? AND `+oiClause+`
		 GROUP BY hour ORDER BY hour`, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("distance over_time: %w", err)
	}
	distOverTime := make([]map[string]interface{}, 0)
	for timeRows.Next() {
		var hr string
		var cnt int
		var sum float64
		if err := timeRows.Scan(&hr, &cnt, &sum); err != nil {
			timeRows.Close()
			return nil, fmt.Errorf("distance over_time scan: %w", err)
		}
		if cnt == 0 {
			continue
		}
		distOverTime = append(distOverTime, map[string]interface{}{
			"hour":  hr,
			"avg":   math.Round((sum/float64(cnt))*100) / 100,
			"count": cnt,
		})
	}
	timeRows.Close()

	// topHops: per (pair_key, type) over window, take row with max best_dist.
	pairRows, err := db.conn.Query(
		`SELECT pair_key, type, count, best_dist, best_from_name, best_from_pk,
		        best_to_name, best_to_pk, snr_max, snr_bins
		 FROM distance_pair_hourly
		 WHERE hour >= ? AND hour <= ? AND `+oiClause, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("distance pair read: %w", err)
	}
	type pairAgg struct {
		count    int
		bestDist float64
		fromName string
		fromPk   string
		toName   string
		toPk     string
		typ      string
		snrMax   float64
		haveSnr  bool
		snrBins  []int
	}
	pairAggs := map[string]*pairAgg{}
	for pairRows.Next() {
		var pk, typ string
		var cnt int
		var bd float64
		var fn, fpk, tn, tpk sql.NullString
		var snr sql.NullFloat64
		var sbins []byte
		if err := pairRows.Scan(&pk, &typ, &cnt, &bd, &fn, &fpk, &tn, &tpk, &snr, &sbins); err != nil {
			pairRows.Close()
			return nil, fmt.Errorf("distance pair scan: %w", err)
		}
		k := pk + "|" + typ
		a := pairAggs[k]
		if a == nil {
			a = &pairAgg{typ: typ, snrBins: make([]int, distSnrBinCount)}
			pairAggs[k] = a
		}
		a.count += cnt
		if bd > a.bestDist {
			a.bestDist = bd
			a.fromName = fn.String
			a.fromPk = fpk.String
			a.toName = tn.String
			a.toPk = tpk.String
		}
		if snr.Valid {
			if !a.haveSnr || snr.Float64 > a.snrMax {
				a.snrMax = snr.Float64
				a.haveSnr = true
			}
		}
		rfAddBins(a.snrBins, rfUnpackBins(sbins, distSnrBinCount))
	}
	pairRows.Close()
	pairs := make([]*pairAgg, 0, len(pairAggs))
	for _, a := range pairAggs {
		pairs = append(pairs, a)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].bestDist > pairs[j].bestDist })
	if len(pairs) > 20 {
		pairs = pairs[:20]
	}
	topHops := make([]map[string]interface{}, 0, len(pairs))
	for _, a := range pairs {
		row := map[string]interface{}{
			"fromName": a.fromName, "fromPk": a.fromPk,
			"toName": a.toName, "toPk": a.toPk,
			"dist":     a.bestDist,
			"type":     a.typ,
			"obsCount": a.count,
		}
		if a.haveSnr {
			row["bestSnr"] = a.snrMax
			row["medianSnr"] = distMedianSnrFromBins(a.snrBins)
		}
		topHops = append(topHops, row)
	}

	// topPaths: ORDER BY total_dist DESC LIMIT 20.
	var pathRows *sql.Rows
	if region == "" {
		pathRows, err = db.conn.Query(
			`SELECT tx_id, total_dist, hop_count, hash, timestamp, hops_json
			 FROM distance_paths
			 WHERE hour >= ? AND hour <= ?
			 ORDER BY total_dist DESC LIMIT 20`, sinceHour, untilHour)
	} else {
		pathArgs := append([]interface{}{sinceHour, untilHour}, oiArgs...)
		pathRows, err = db.conn.Query(
			`SELECT p.tx_id, p.total_dist, p.hop_count, p.hash, p.timestamp, p.hops_json
			 FROM distance_paths p
			 JOIN distance_path_observers o ON o.tx_id = p.tx_id
			 WHERE p.hour >= ? AND p.hour <= ?
			   AND o.observer_idx IN (`+rfIntPlaceholders(len(idxs))+`)
			 GROUP BY p.tx_id
			 ORDER BY p.total_dist DESC LIMIT 20`, pathArgs...)
	}
	if err != nil {
		return nil, fmt.Errorf("distance paths read: %w", err)
	}
	topPaths := make([]map[string]interface{}, 0)
	for pathRows.Next() {
		var txID, hc int
		var td float64
		var hsh, ts, hops sql.NullString
		if err := pathRows.Scan(&txID, &td, &hc, &hsh, &ts, &hops); err != nil {
			pathRows.Close()
			return nil, fmt.Errorf("distance paths scan: %w", err)
		}
		var hopList []map[string]interface{}
		if hops.String != "" {
			_ = json.Unmarshal([]byte(hops.String), &hopList)
		}
		topPaths = append(topPaths, map[string]interface{}{
			"hash":      hsh.String,
			"totalDist": td,
			"hopCount":  hc,
			"timestamp": ts.String,
			"hops":      hopList,
		})
	}
	pathRows.Close()

	return map[string]interface{}{
		"summary":       summary,
		"topHops":       topHops,
		"topPaths":      topPaths,
		"catStats":      catStats,
		"distHistogram": distHistogram,
		"distOverTime":  distOverTime,
	}, nil
}

// distMedianFromBins interpolates the median distance from a 25-bin
// histogram of [0,300) km using 12 km bins.
func distMedianFromBins(bins []int) float64 {
	total := 0
	for _, c := range bins {
		total += c
	}
	if total == 0 {
		return 0
	}
	half := total / 2
	cum := 0
	for i, c := range bins {
		cum += c
		if cum >= half {
			return float64(distDistBinMin) + (float64(i)+0.5)*float64(distDistBinWidth)
		}
	}
	return 0
}

// distMedianSnrFromBins interpolates the median SNR from a 50-bin histogram
// of [-30,20) dB using 1 dB bins.
func distMedianSnrFromBins(bins []int) float64 {
	total := 0
	for _, c := range bins {
		total += c
	}
	if total == 0 {
		return 0
	}
	half := total / 2
	cum := 0
	for i, c := range bins {
		cum += c
		if cum >= half {
			return float64(distSnrBinMin) + (float64(i)+0.5)*float64(distSnrBinWidth)
		}
	}
	return 0
}
