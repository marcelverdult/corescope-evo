// rf_rollup.go — RF analytics rollup: schema, bin packing, single-hour recompute.
// See .specs/2026-05-19-rf-analytics-rollup-design.md.

package main

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
)

// Fixed histogram bins. Values outside the range clamp to the end bin.
const (
	rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount    = -30, 1, 50
	rfRssiBinMin, rfRssiBinWidth, rfRssiBinCount = -130, 1, 110
	rfSizeBinMin, rfSizeBinWidth, rfSizeBinCount = 0, 4, 64
)

// rfBinIndex maps a value to a clamped [0,count) bin index.
func rfBinIndex(v, min, width, count int) int {
	i := (v - min) / width
	if i < 0 {
		return 0
	}
	if i >= count {
		return count - 1
	}
	return i
}

// rfPackBins encodes per-bin counts as little-endian int16. Counts above the
// int16 max are clamped (a single hour/observer cell never approaches 32767).
func rfPackBins(counts []int) []byte {
	b := make([]byte, len(counts)*2)
	for i, c := range counts {
		if c > 32767 {
			c = 32767
		}
		if c < 0 {
			c = 0
		}
		binary.LittleEndian.PutUint16(b[i*2:], uint16(c))
	}
	return b
}

// rfUnpackBins decodes a packed blob into count integers. A nil/short blob
// yields a zero slice of length count.
func rfUnpackBins(b []byte, count int) []int {
	out := make([]int, count)
	for i := 0; i < count && (i*2+1) < len(b); i++ {
		out[i] = int(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// ensureRFRollupTable creates the rollup tables. Idempotent (IF NOT EXISTS).
func ensureRFRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for rf_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rf_rollup (
			hour TEXT NOT NULL,
			payload_type INTEGER NOT NULL,
			observer_idx INTEGER NOT NULL,
			n_obs INTEGER NOT NULL DEFAULT 0,
			n_snr INTEGER NOT NULL DEFAULT 0,
			snr_sum REAL NOT NULL DEFAULT 0,
			snr_sumsq REAL NOT NULL DEFAULT 0,
			snr_min REAL, snr_max REAL,
			n_rssi INTEGER NOT NULL DEFAULT 0,
			rssi_sum REAL NOT NULL DEFAULT 0,
			rssi_sumsq REAL NOT NULL DEFAULT 0,
			rssi_min REAL, rssi_max REAL,
			pkt_n INTEGER NOT NULL DEFAULT 0,
			pkt_sum INTEGER NOT NULL DEFAULT 0,
			pkt_min INTEGER, pkt_max INTEGER,
			n_tx INTEGER NOT NULL DEFAULT 0,
			snr_bins BLOB, rssi_bins BLOB, size_bins BLOB,
			scatter BLOB,
			PRIMARY KEY (hour, payload_type, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rf_rollup_hour ON rf_rollup(hour)`,
		`CREATE INDEX IF NOT EXISTS idx_rf_rollup_observer ON rf_rollup(observer_idx)`,
		`CREATE TABLE IF NOT EXISTS rf_rollup_tx (
			hour TEXT NOT NULL,
			payload_type INTEGER NOT NULL,
			distinct_tx INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, payload_type)
		)`,
		`CREATE TABLE IF NOT EXISTS rf_rollup_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("rf_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}

// rfCell accumulates one (payload_type, observer_idx) cell while scanning a
// single hour of raw observations.
type rfCell struct {
	nObs, nSnr, nRssi, pktN, nTx         int
	snrSum, snrSumSq, rssiSum, rssiSumSq float64
	snrMin, snrMax, rssiMin, rssiMax     float64
	haveSnr, haveRssi                    bool
	pktSum, pktMin, pktMax               int
	havePkt                              bool
	snrBins, rssiBins, sizeBins          []int
	scatter                              []rfScatterPoint
	txSeen                               map[int]bool
}

func newRFCell() *rfCell {
	return &rfCell{
		snrBins:  make([]int, rfSnrBinCount),
		rssiBins: make([]int, rfRssiBinCount),
		sizeBins: make([]int, rfSizeBinCount),
		txSeen:   map[int]bool{},
	}
}

// recomputeRFRollupHour rebuilds all rollup rows for the given hour bucket
// ("2026-05-18T10") from raw observations. Idempotent: deletes then re-inserts.
func recomputeRFRollupHour(rw *sql.DB, hour string) error {
	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("recompute begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM rf_rollup WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("recompute delete rollup: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM rf_rollup_tx WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("recompute delete rollup_tx: %w", err)
	}

	rows, err := tx.Query(`
		SELECT t.payload_type, o.observer_idx, o.snr, o.rssi, o.transmission_id,
		       length(t.raw_hex)/2
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		WHERE strftime('%Y-%m-%dT%H', o.timestamp, 'unixepoch') = ?`, hour)
	if err != nil {
		return fmt.Errorf("recompute scan: %w", err)
	}
	cells := map[[2]int]*rfCell{}
	txByType := map[int]map[int]bool{}
	for rows.Next() {
		var ptN sql.NullInt64
		var obsIdx int
		var snr, rssi sql.NullFloat64
		var txID, size int
		if err := rows.Scan(&ptN, &obsIdx, &snr, &rssi, &txID, &size); err != nil {
			rows.Close()
			return fmt.Errorf("recompute row: %w", err)
		}
		pt := -1
		if ptN.Valid {
			pt = int(ptN.Int64)
		}
		key := [2]int{pt, obsIdx}
		c := cells[key]
		if c == nil {
			c = newRFCell()
			cells[key] = c
		}
		c.nObs++
		if snr.Valid {
			v := snr.Float64
			c.nSnr++
			c.snrSum += v
			c.snrSumSq += v * v
			if !c.haveSnr || v < c.snrMin {
				c.snrMin = v
			}
			if !c.haveSnr || v > c.snrMax {
				c.snrMax = v
			}
			c.haveSnr = true
			c.snrBins[rfBinIndex(int(v), rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount)]++
		}
		if rssi.Valid {
			v := rssi.Float64
			c.nRssi++
			c.rssiSum += v
			c.rssiSumSq += v * v
			if !c.haveRssi || v < c.rssiMin {
				c.rssiMin = v
			}
			if !c.haveRssi || v > c.rssiMax {
				c.rssiMax = v
			}
			c.haveRssi = true
			c.rssiBins[rfBinIndex(int(v), rfRssiBinMin, rfRssiBinWidth, rfRssiBinCount)]++
		}
		if !c.txSeen[txID] {
			c.txSeen[txID] = true
			c.nTx++
			c.pktN++
			c.pktSum += size
			if !c.havePkt || size < c.pktMin {
				c.pktMin = size
			}
			if !c.havePkt || size > c.pktMax {
				c.pktMax = size
			}
			c.havePkt = true
			c.sizeBins[rfBinIndex(size, rfSizeBinMin, rfSizeBinWidth, rfSizeBinCount)]++
		}
		if snr.Valid && rssi.Valid && len(c.scatter) < 8 {
			c.scatter = append(c.scatter, rfScatterPoint{SNR: snr.Float64, RSSI: rssi.Float64})
		}
		if txByType[pt] == nil {
			txByType[pt] = map[int]bool{}
		}
		txByType[pt][txID] = true
	}
	rows.Close()

	for key, c := range cells {
		if err := rfInsertCell(tx, hour, key[0], key[1], c); err != nil {
			return err
		}
	}
	for pt, set := range txByType {
		if _, err := tx.Exec(`INSERT INTO rf_rollup_tx(hour,payload_type,distinct_tx)
			VALUES (?,?,?)`, hour, pt, len(set)); err != nil {
			return fmt.Errorf("recompute insert rollup_tx: %w", err)
		}
	}
	return tx.Commit()
}

func rfInsertCell(tx *sql.Tx, hour string, pt, obsIdx int, c *rfCell) error {
	scatterBlob := rfPackScatter(c.scatter)
	_, err := tx.Exec(`INSERT INTO rf_rollup
		(hour,payload_type,observer_idx,n_obs,n_snr,snr_sum,snr_sumsq,snr_min,snr_max,
		 n_rssi,rssi_sum,rssi_sumsq,rssi_min,rssi_max,pkt_n,pkt_sum,pkt_min,pkt_max,
		 n_tx,snr_bins,rssi_bins,size_bins,scatter)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		hour, pt, obsIdx, c.nObs, c.nSnr, c.snrSum, c.snrSumSq,
		rfNullF(c.haveSnr, c.snrMin), rfNullF(c.haveSnr, c.snrMax),
		c.nRssi, c.rssiSum, c.rssiSumSq,
		rfNullF(c.haveRssi, c.rssiMin), rfNullF(c.haveRssi, c.rssiMax),
		c.pktN, c.pktSum, rfNullI(c.havePkt, c.pktMin), rfNullI(c.havePkt, c.pktMax),
		c.nTx, rfPackBins(c.snrBins), rfPackBins(c.rssiBins), rfPackBins(c.sizeBins),
		scatterBlob)
	if err != nil {
		return fmt.Errorf("insert rollup cell: %w", err)
	}
	return nil
}

func rfNullF(have bool, v float64) interface{} {
	if !have {
		return nil
	}
	return v
}
func rfNullI(have bool, v int) interface{} {
	if !have {
		return nil
	}
	return v
}

// rfPackScatter / rfUnpackScatter encode scatter points as little-endian
// float64 pairs.
func rfPackScatter(pts []rfScatterPoint) []byte {
	b := make([]byte, len(pts)*16)
	for i, p := range pts {
		binary.LittleEndian.PutUint64(b[i*16:], rfF2u(p.SNR))
		binary.LittleEndian.PutUint64(b[i*16+8:], rfF2u(p.RSSI))
	}
	return b
}
func rfUnpackScatter(b []byte) []rfScatterPoint {
	var out []rfScatterPoint
	for i := 0; i+16 <= len(b); i += 16 {
		out = append(out, rfScatterPoint{
			SNR:  rfU2f(binary.LittleEndian.Uint64(b[i:])),
			RSSI: rfU2f(binary.LittleEndian.Uint64(b[i+8:])),
		})
	}
	return out
}

func rfF2u(f float64) uint64 { return math.Float64bits(f) }
func rfU2f(u uint64) float64 { return math.Float64frombits(u) }
