// rf_rollup_read.go — RF analytics result assembly from the rf_rollup table.

package main

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// agg accumulates RF aggregates across a set of rollup rows. Package-level so
// both computeRFFromRollup and rfAssembleRollupResult can name it.
type agg struct {
	nObs, nSnr, nRssi, pktN, nTx         int
	snrSum, snrSumSq, rssiSum, rssiSumSq float64
	snrMin, snrMax, rssiMin, rssiMax     sql.NullFloat64
	pktSum                               int
	pktMin, pktMax                       sql.NullInt64
}

// hourCount is one {hour,count} entry for packetsPerHour.
type hourCount struct {
	Hour  string `json:"hour"`
	Count int    `json:"count"`
}

// computeRFFromRollup builds the RF analytics result map from rf_rollup.
// region "" = global. window zero = a default 24h window applied here.
func computeRFFromRollup(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	eff := rfEffectiveWindow(window)
	sinceHour, untilHour := rfWindowHourBounds(eff)

	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}

	where := "hour >= ? AND hour <= ?"
	args := []interface{}{sinceHour, untilHour}
	if region != "" {
		where += " AND observer_idx IN (" + rfIntPlaceholders(len(idxs)) + ")"
		for _, v := range idxs {
			args = append(args, v)
		}
	}

	typeRows, err := db.conn.Query(`
		SELECT payload_type,
		       SUM(n_obs), SUM(n_snr), SUM(snr_sum), SUM(snr_sumsq),
		       MIN(snr_min), MAX(snr_max),
		       SUM(n_rssi), SUM(rssi_sum), SUM(rssi_sumsq),
		       MIN(rssi_min), MAX(rssi_max),
		       SUM(pkt_n), SUM(pkt_sum), MIN(pkt_min), MAX(pkt_max), SUM(n_tx)
		FROM rf_rollup WHERE `+where+` GROUP BY payload_type`, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup type query: %w", err)
	}
	byType := map[int]*agg{}
	tot := &agg{}
	for typeRows.Next() {
		var pt int
		a := &agg{}
		if err := typeRows.Scan(&pt, &a.nObs, &a.nSnr, &a.snrSum, &a.snrSumSq,
			&a.snrMin, &a.snrMax, &a.nRssi, &a.rssiSum, &a.rssiSumSq,
			&a.rssiMin, &a.rssiMax, &a.pktN, &a.pktSum, &a.pktMin, &a.pktMax, &a.nTx); err != nil {
			typeRows.Close()
			return nil, fmt.Errorf("rollup type scan: %w", err)
		}
		byType[pt] = a
		tot.nObs += a.nObs
		tot.nSnr += a.nSnr
		tot.snrSum += a.snrSum
		tot.snrSumSq += a.snrSumSq
		tot.nRssi += a.nRssi
		tot.rssiSum += a.rssiSum
		tot.rssiSumSq += a.rssiSumSq
		tot.pktN += a.pktN
		tot.pktSum += a.pktSum
		tot.nTx += a.nTx
		tot.snrMin = rfMinNF(tot.snrMin, a.snrMin)
		tot.snrMax = rfMaxNF(tot.snrMax, a.snrMax)
		tot.rssiMin = rfMinNF(tot.rssiMin, a.rssiMin)
		tot.rssiMax = rfMaxNF(tot.rssiMax, a.rssiMax)
		tot.pktMin = rfMinNI(tot.pktMin, a.pktMin)
		tot.pktMax = rfMaxNI(tot.pktMax, a.pktMax)
	}
	typeRows.Close()

	hourRows, err := db.conn.Query(`
		SELECT hour, SUM(n_tx), SUM(n_snr), SUM(snr_sum)
		FROM rf_rollup WHERE `+where+` GROUP BY hour ORDER BY hour`, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup hour query: %w", err)
	}
	var packetsPerHour []hourCount
	var signalOverTime []map[string]interface{}
	for hourRows.Next() {
		var h string
		var nTx, nSnr int
		var snrSum float64
		if err := hourRows.Scan(&h, &nTx, &nSnr, &snrSum); err != nil {
			hourRows.Close()
			return nil, fmt.Errorf("rollup hour scan: %w", err)
		}
		packetsPerHour = append(packetsPerHour, hourCount{Hour: h, Count: nTx})
		if nSnr > 0 {
			signalOverTime = append(signalOverTime, map[string]interface{}{
				"hour": h, "count": nSnr, "avgSnr": snrSum / float64(nSnr),
			})
		}
	}
	hourRows.Close()

	snrBins := make([]int, rfSnrBinCount)
	rssiBins := make([]int, rfRssiBinCount)
	sizeBins := make([]int, rfSizeBinCount)
	var scatterAll []rfScatterPoint
	blobRows, err := db.conn.Query(`
		SELECT snr_bins, rssi_bins, size_bins, scatter
		FROM rf_rollup WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup blob query: %w", err)
	}
	for blobRows.Next() {
		var sb, rb, zb, sc []byte
		if err := blobRows.Scan(&sb, &rb, &zb, &sc); err != nil {
			blobRows.Close()
			return nil, fmt.Errorf("rollup blob scan: %w", err)
		}
		rfAddBins(snrBins, rfUnpackBins(sb, rfSnrBinCount))
		rfAddBins(rssiBins, rfUnpackBins(rb, rfRssiBinCount))
		rfAddBins(sizeBins, rfUnpackBins(zb, rfSizeBinCount))
		scatterAll = append(scatterAll, rfUnpackScatter(sc)...)
	}
	blobRows.Close()

	totalTx := tot.nTx
	if region == "" {
		var dt sql.NullInt64
		if err := db.conn.QueryRow(`SELECT SUM(distinct_tx) FROM rf_rollup_tx
			WHERE hour >= ? AND hour <= ?`, sinceHour, untilHour).Scan(&dt); err != nil {
			return nil, fmt.Errorf("rollup distinct_tx: %w", err)
		}
		if dt.Valid {
			totalTx = int(dt.Int64)
		}
	}

	return rfAssembleRollupResult(tot, byType, packetsPerHour, signalOverTime,
		snrBins, rssiBins, sizeBins, scatterAll, totalTx), nil
}

func rfAddBins(dst, src []int) {
	for i := range dst {
		if i < len(src) {
			dst[i] += src[i]
		}
	}
}

func rfIntPlaceholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	s := "?"
	for i := 1; i < n; i++ {
		s += ",?"
	}
	return s
}

func rfMinNF(a, b sql.NullFloat64) sql.NullFloat64 {
	if !a.Valid {
		return b
	}
	if b.Valid && b.Float64 < a.Float64 {
		return b
	}
	return a
}
func rfMaxNF(a, b sql.NullFloat64) sql.NullFloat64 {
	if !a.Valid {
		return b
	}
	if b.Valid && b.Float64 > a.Float64 {
		return b
	}
	return a
}
func rfMinNI(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid {
		return b
	}
	if b.Valid && b.Int64 < a.Int64 {
		return b
	}
	return a
}
func rfMaxNI(a, b sql.NullInt64) sql.NullInt64 {
	if !a.Valid {
		return b
	}
	if b.Valid && b.Int64 > a.Int64 {
		return b
	}
	return a
}

// rfHistogramFromBins builds the {bins,min,max} map from fixed-bin counts.
func rfHistogramFromBins(counts []int, binMin, binWidth int) map[string]interface{} {
	bins := make([]map[string]interface{}, len(counts))
	for i, c := range counts {
		bins[i] = map[string]interface{}{
			"x": float64(binMin + i*binWidth), "w": float64(binWidth), "count": c,
		}
	}
	lo, hi := 0.0, 0.0
	for i, c := range counts {
		if c > 0 {
			lo = float64(binMin + i*binWidth)
			break
		}
	}
	for i := len(counts) - 1; i >= 0; i-- {
		if counts[i] > 0 {
			hi = float64(binMin + i*binWidth)
			break
		}
	}
	return map[string]interface{}{"bins": bins, "min": lo, "max": hi}
}

// rfMedianFromBins returns the value at the median bin (lower edge).
func rfMedianFromBins(counts []int, binMin, binWidth int) float64 {
	total := 0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := total / 2
	cum := 0
	for i, c := range counts {
		cum += c
		if cum > target {
			return float64(binMin + i*binWidth)
		}
	}
	return float64(binMin + (len(counts)-1)*binWidth)
}

// rfEffectiveWindow applies a 24h default when no window is given.
func rfEffectiveWindow(w TimeWindow) TimeWindow {
	if w.IsZero() {
		since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
		return TimeWindow{Since: since, Label: "24h"}
	}
	return w
}

// rfWindowHourBounds returns inclusive hour-bucket strings for a window.
func rfWindowHourBounds(w TimeWindow) (sinceHour, untilHour string) {
	sinceHour = "0000-00-00T00"
	untilHour = "9999-99-99T99"
	if w.Since != "" {
		if t, err := time.Parse(time.RFC3339, w.Since); err == nil {
			sinceHour = t.UTC().Format("2006-01-02T15")
		}
	}
	if w.Until != "" {
		if t, err := time.Parse(time.RFC3339, w.Until); err == nil {
			untilHour = t.UTC().Format("2006-01-02T15")
		}
	}
	return sinceHour, untilHour
}

func rfNFval(n sql.NullFloat64) float64 {
	if n.Valid {
		return n.Float64
	}
	return 0
}
func rfNIval(n sql.NullInt64) float64 {
	if n.Valid {
		return float64(n.Int64)
	}
	return 0
}

func rfAssembleRollupResult(tot *agg, byType map[int]*agg,
	packetsPerHour []hourCount, signalOverTime []map[string]interface{},
	snrBins, rssiBins, sizeBins []int, scatterAll []rfScatterPoint,
	totalTx int) map[string]interface{} {

	ptNames := payloadTypeNames

	snrStats := map[string]interface{}{"min": 0.0, "max": 0.0, "avg": 0.0, "median": 0.0, "stddev": 0.0}
	if tot.nSnr > 0 {
		avg := tot.snrSum / float64(tot.nSnr)
		snrStats = map[string]interface{}{
			"min": rfNFval(tot.snrMin), "max": rfNFval(tot.snrMax), "avg": avg,
			"median": rfMedianFromBins(snrBins, rfSnrBinMin, rfSnrBinWidth),
			"stddev": math.Sqrt(math.Max(0, tot.snrSumSq/float64(tot.nSnr)-avg*avg)),
		}
	}
	rssiStats := map[string]interface{}{"min": 0.0, "max": 0.0, "avg": 0.0, "median": 0.0, "stddev": 0.0}
	if tot.nRssi > 0 {
		avg := tot.rssiSum / float64(tot.nRssi)
		rssiStats = map[string]interface{}{
			"min": rfNFval(tot.rssiMin), "max": rfNFval(tot.rssiMax), "avg": avg,
			"median": rfMedianFromBins(rssiBins, rfRssiBinMin, rfRssiBinWidth),
			"stddev": math.Sqrt(math.Max(0, tot.rssiSumSq/float64(tot.nRssi)-avg*avg)),
		}
	}

	type ptEntry struct {
		Type  int    `json:"type"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	var payloadTypes []ptEntry
	type snrTypeEntry struct {
		Name  string  `json:"name"`
		Count int     `json:"count"`
		Avg   float64 `json:"avg"`
		Min   float64 `json:"min"`
		Max   float64 `json:"max"`
	}
	var snrByType []snrTypeEntry
	for pt, a := range byType {
		name := ptNames[pt]
		if name == "" {
			name = fmt.Sprintf("UNK(%d)", pt)
		}
		payloadTypes = append(payloadTypes, ptEntry{Type: pt, Name: name, Count: a.nTx})
		if a.nSnr > 0 {
			snrByType = append(snrByType, snrTypeEntry{
				Name: name, Count: a.nSnr, Avg: a.snrSum / float64(a.nSnr),
				Min: rfNFval(a.snrMin), Max: rfNFval(a.snrMax),
			})
		}
	}
	sort.Slice(payloadTypes, func(i, j int) bool { return payloadTypes[i].Count > payloadTypes[j].Count })
	sort.Slice(snrByType, func(i, j int) bool { return snrByType[i].Count > snrByType[j].Count })

	avgPkt, minPkt, maxPkt := 0, 0, 0
	if tot.pktN > 0 {
		avgPkt = tot.pktSum / tot.pktN
		minPkt = int(rfNIval(tot.pktMin))
		maxPkt = int(rfNIval(tot.pktMax))
	}

	timeSpanHours := 0.0
	if len(packetsPerHour) > 1 {
		timeSpanHours = float64(len(packetsPerHour) - 1)
	}

	scatter := scatterAll
	if len(scatter) > 500 {
		step := len(scatter) / 500
		var s []rfScatterPoint
		for i := 0; i < len(scatter); i += step {
			s = append(s, scatter[i])
		}
		scatter = s
	}
	if scatter == nil {
		scatter = []rfScatterPoint{}
	}

	return map[string]interface{}{
		"totalPackets":       tot.nSnr,
		"totalAllPackets":    tot.nObs,
		"totalTransmissions": totalTx,
		"snr":                snrStats,
		"rssi":               rssiStats,
		"snrValues":          rfHistogramFromBins(snrBins, rfSnrBinMin, rfSnrBinWidth),
		"rssiValues":         rfHistogramFromBins(rssiBins, rfRssiBinMin, rfRssiBinWidth),
		"packetSizes":        rfHistogramFromBins(sizeBins, rfSizeBinMin, rfSizeBinWidth),
		"minPacketSize":      minPkt,
		"maxPacketSize":      maxPkt,
		"avgPacketSize":      avgPkt,
		"packetsPerHour":     packetsPerHour,
		"payloadTypes":       payloadTypes,
		"snrByType":          snrByType,
		"signalOverTime":     signalOverTime,
		"scatterData":        scatter,
		"timeSpanHours":      timeSpanHours,
	}
}
