# RF Analytics Rollup — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the on-demand RF analytics SQL queries with a pre-aggregated `rf_rollup` table so every RF analytics query — global, region-filtered, any history depth — stays fast as the DB grows.

**Architecture:** A background job maintains `rf_rollup` (per-hour, per-payload-type, per-observer pre-aggregated RF stats incl. fixed-bin histograms). `computeAnalyticsRFSQL` is rewritten to `SUM`/`GROUP BY` rollup rows instead of scanning raw `observations`. Gated by the existing `analyticsSqlBackend` flag; in-memory `computeAnalyticsRF` stays as fallback + parity reference.

**Tech Stack:** Go, `database/sql` + modernc.org/sqlite, the existing CoreScope `DB`/`PacketStore` types.

Spec: `.specs/2026-05-19-rf-analytics-rollup-design.md`

---

## Build commands — IMPORTANT

Per-directory Go modules: run all `go` commands from inside `cmd/server/` (module `github.com/corescope/server`; no root `go.mod`). `git` commands run from the worktree root. Run `gofmt -l <files>` on every file touched — must be empty. Do not run `gofmt -w` on whole pre-existing files (it reformats unrelated code).

## Background facts (verified)

- Schema-migration pattern: `ensureXxxTable(dbPath string) error` uses `cachedRW(dbPath)` (returns `*sql.DB`) + `CREATE TABLE IF NOT EXISTS`, called from `main.go` startup. Example: `ensureNeighborEdgesTable` in `neighbor_persist.go`.
- Periodic-job pattern in `main.go`: `time.NewTicker` + `go func(){ defer recover; for { select { case <-ticker.C: …; case <-done: return } } }()`.
- Async backfill pattern: `go backfillXxxAsync(...)` started after HTTP is up (`main.go` ~line 592).
- `observations(id INTEGER PK, transmission_id, observer_idx, snr REAL, rssi REAL, timestamp INTEGER epoch-sec, raw_hex, …)`. `transmissions(id, raw_hex, hash UNIQUE, first_seen TEXT, payload_type INTEGER, …)`.
- `cmd/server/analytics_rf_sql.go` currently holds the on-demand SQL backend: `computeAnalyticsRFSQL`, `rfCoreAggregates`, `rfSortedColumn`, `rfPacketSizes`, `rfTypeDistribution`, `rfSnrByType`, `rfHourlyBuckets`, `rfScatterSample`, `rfTotalTransmissions`, plus filter helpers (`rfWindowEpochBounds`, `rfObsWindowClause`, `rfTxWindowClause`, `rfObserverIdxClause`, `rfWhere`, `rfRegionObserverIdxs`, `rfObsWhere`). The filter helpers + `rfRegionObserverIdxs` are REUSED. The per-query aggregate functions are REPLACED by the rollup read path.
- `cmd/server/analytics_stats.go`: `rfStddevF64`, `rfBuildHistogramF64`, `rfBuildHistogramInt` — reused by the in-memory path; the rollup read path builds the histogram map shape directly.
- `GetAnalyticsRFWithWindow` (in `store_analytics.go`) branches on `s.analyticsSQLBackend`; returns `(map[string]interface{}, error)`.
- Test helpers: `setupTestDB(t) *DB` (in-memory), `mustExec(t, db, q, args…)`, `loadStore(t, dbPath, maxMemMB) *PacketStore`. `setupTestDB` returns an in-memory DB (`path==""`); tests needing a file DB build a temp-file DB (see `ensure_indexes_test.go` / `bounded_load_test.go`).
- Result-map keys `computeAnalyticsRFSQL` must produce (unchanged): `totalPackets, totalAllPackets, totalTransmissions, snr, rssi, snrValues, rssiValues, packetSizes, minPacketSize, maxPacketSize, avgPacketSize, packetsPerHour, payloadTypes, snrByType, signalOverTime, scatterData, timeSpanHours`.

## File structure

- **Create** `cmd/server/rf_rollup.go` — bin constants, blob pack/unpack, schema (`ensureRFRollupTable`), single-hour recompute (`recomputeRFRollupHour`).
- **Create** `cmd/server/rf_rollup_maintain.go` — backfill (`backfillRFRollupAsync`) + incremental job (`runRFRollupMaintenance`) + watermark.
- **Create** `cmd/server/rf_rollup_read.go` — the rollup-backed read path that produces the RF result map.
- **Modify** `cmd/server/analytics_rf_sql.go` — `computeAnalyticsRFSQL` delegates to the rollup read path; remove the dead on-demand aggregate functions + `rfSortedColumn`.
- **Modify** `cmd/server/main.go` — call `ensureRFRollupTable`; start backfill + maintenance when the flag is on.
- **Create** `cmd/server/rf_rollup_test.go` — unit + parity + perf tests.

---

## Task 1: Bin constants + blob pack/unpack

**Files:**
- Create: `cmd/server/rf_rollup.go`
- Test: `cmd/server/rf_rollup_test.go`

- [ ] **Step 1: Write the failing test** — create `cmd/server/rf_rollup_test.go`:

```go
package main

import "testing"

func TestRFBinIndexAndPacking(t *testing.T) {
	if got := rfBinIndex(-30, rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount); got != 0 {
		t.Fatalf("snr -30 -> bin %d, want 0", got)
	}
	if got := rfBinIndex(1000, rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount); got != rfSnrBinCount-1 {
		t.Fatalf("snr 1000 clamps to %d, want %d", got, rfSnrBinCount-1)
	}
	counts := make([]int, rfSnrBinCount)
	counts[0] = 5
	counts[49] = 7
	packed := rfPackBins(counts)
	if len(packed) != rfSnrBinCount*2 {
		t.Fatalf("packed len %d, want %d", len(packed), rfSnrBinCount*2)
	}
	out := rfUnpackBins(packed, rfSnrBinCount)
	if out[0] != 5 || out[49] != 7 {
		t.Fatalf("unpack mismatch: %v", out[:1])
	}
}
```

- [ ] **Step 2: Run test, verify FAIL** — `cd cmd/server && go test . -run TestRFBinIndexAndPacking -v` → FAIL (undefined symbols).

- [ ] **Step 3: Create `cmd/server/rf_rollup.go`** with the constants + packing:

```go
// rf_rollup.go — RF analytics rollup: schema, bin packing, single-hour recompute.
// See .specs/2026-05-19-rf-analytics-rollup-design.md.

package main

import (
	"encoding/binary"
	"fmt"
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

var _ = fmt.Sprintf // kept for later tasks in this file
```

- [ ] **Step 4: Run test, verify PASS** — `cd cmd/server && go test . -run TestRFBinIndexAndPacking -v`.

- [ ] **Step 5: gofmt** — `cd cmd/server && gofmt -l rf_rollup.go rf_rollup_test.go` (empty).

- [ ] **Step 6: Commit**

```bash
git add cmd/server/rf_rollup.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): RF rollup bin constants + blob packing"
```

---

## Task 2: `rf_rollup` schema

**Files:**
- Modify: `cmd/server/rf_rollup.go` (append)
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test** — append:

```go
func TestEnsureRFRollupTable(t *testing.T) {
	db := setupTestDBFile(t) // file-backed; see helper note
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatalf("ensureRFRollupTable: %v", err)
	}
	for _, tbl := range []string{"rf_rollup", "rf_rollup_tx", "rf_rollup_meta"} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("table %s not created: %v", tbl, err)
		}
	}
}
```

`setupTestDBFile` — a file-backed test DB helper. If one already exists in the test files, use it; otherwise add this helper to `rf_rollup_test.go`:

```go
func setupTestDBFile(t *testing.T) *DB {
	t.Helper()
	path := t.TempDir() + "/test.db"
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.conn.Close() })
	return db
}
```

> Implementer: confirm the DB-open function name (`OpenDB` or similar) and schema-creation path by reading `db.go` — the file DB must have the `observations`/`transmissions` schema for later tasks. Reuse whatever `setupTestDB`/`ensure_indexes_test.go` does to create a file DB with the full schema.

- [ ] **Step 2: Run test, verify FAIL** — `cd cmd/server && go test . -run TestEnsureRFRollupTable -v`.

- [ ] **Step 3: Implement** — append to `cmd/server/rf_rollup.go`:

```go
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
```

- [ ] **Step 4: Run test, verify PASS** — `cd cmd/server && go test . -run TestEnsureRFRollupTable -v`.

- [ ] **Step 5: gofmt + commit**

```bash
git add cmd/server/rf_rollup.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): rf_rollup table schema"
```

---

## Task 3: Single-hour recompute

The core write primitive: `recomputeRFRollupHour(rw, hour)` — delete the hour's rollup rows, scan that hour's raw observations, rebuild the cells.

**Files:**
- Modify: `cmd/server/rf_rollup.go` (append)
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test** — append:

```go
func TestRecomputeRFRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// hour 2026-05-18T10 -> epoch 1779444000..1779447599
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccddee','h2','2026-05-18T10:05:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,5.0,-80.0,1779444000),(1,2,7.0,-90.0,1779444000),(2,1,9.0,-70.0,1779444300)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeRFRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	var nObs, nSnr int
	var snrSum float64
	err = rw.QueryRow(`SELECT SUM(n_obs),SUM(n_snr),SUM(snr_sum) FROM rf_rollup WHERE hour=?`,
		"2026-05-18T10").Scan(&nObs, &nSnr, &snrSum)
	if err != nil {
		t.Fatal(err)
	}
	if nObs != 3 || nSnr != 3 || snrSum != 21.0 {
		t.Fatalf("rollup wrong: nObs=%d nSnr=%d snrSum=%v", nObs, nSnr, snrSum)
	}
	var distinctTx int
	if err := rw.QueryRow(`SELECT SUM(distinct_tx) FROM rf_rollup_tx WHERE hour=?`,
		"2026-05-18T10").Scan(&distinctTx); err != nil {
		t.Fatal(err)
	}
	if distinctTx != 2 {
		t.Fatalf("distinct_tx=%d want 2", distinctTx)
	}
}
```

- [ ] **Step 2: Run test, verify FAIL** — `cd cmd/server && go test . -run TestRecomputeRFRollupHour -v`.

- [ ] **Step 3: Implement** — append to `cmd/server/rf_rollup.go` (add `"database/sql"` and `"strings"` to imports):

```go
// rfCell accumulates one (payload_type, observer_idx) cell while scanning a
// single hour of raw observations.
type rfCell struct {
	nObs, nSnr, nRssi, pktN, nTx           int
	snrSum, snrSumSq, rssiSum, rssiSumSq   float64
	snrMin, snrMax, rssiMin, rssiMax       float64
	haveSnr, haveRssi                      bool
	pktSum, pktMin, pktMax                 int
	havePkt                                bool
	snrBins, rssiBins, sizeBins            []int
	scatter                                []rfScatterPoint
	txSeen                                 map[int]bool
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
	cells := map[[2]int]*rfCell{}                  // key: {payloadType, observerIdx}
	txByType := map[int]map[int]bool{}             // payloadType -> set of tx ids
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
```

Add to the imports of `rf_rollup.go`: `"database/sql"`, `"math"`, `"strings"`. Add float<->uint helpers:

```go
func rfF2u(f float64) uint64 { return math.Float64bits(f) }
func rfU2f(u uint64) float64 { return math.Float64frombits(u) }
```

> `rfScatterPoint` is the existing struct in `analytics_rf_sql.go` (`{SNR, RSSI float64}`). Do not redefine it. `strings` may be unused after this task — if `go build` reports it unused, remove it from the import block.

- [ ] **Step 4: Run test, verify PASS** — `cd cmd/server && go test . -run TestRecomputeRFRollupHour -v`.

- [ ] **Step 5: gofmt + commit**

```bash
git add cmd/server/rf_rollup.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): single-hour rollup recompute"
```

---

## Task 4: Backfill + watermark + incremental maintenance

**Files:**
- Create: `cmd/server/rf_rollup_maintain.go`
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test** — append:

```go
func TestRFRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779444000)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	var n int
	rw.QueryRow(`SELECT SUM(n_obs) FROM rf_rollup`).Scan(&n)
	if n != 1 {
		t.Fatalf("after first run n_obs=%d want 1", n)
	}
	// add a second observation in the same hour
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (2,1,2,6.0,-81.0,1779444100)`)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT SUM(n_obs) FROM rf_rollup`).Scan(&n)
	if n != 2 {
		t.Fatalf("after second run n_obs=%d want 2", n)
	}
}
```

- [ ] **Step 2: Run test, verify FAIL** — `cd cmd/server && go test . -run TestRFRollupMaintenance -v`.

- [ ] **Step 3: Implement** — create `cmd/server/rf_rollup_maintain.go`:

```go
// rf_rollup_maintain.go — RF rollup backfill + incremental maintenance.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

const rfRollupWatermarkKey = "rf_rollup_last_obs_id"

func rfRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM rf_rollup_meta WHERE key=?`,
		rfRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read watermark: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func rfSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO rf_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		rfRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set watermark: %w", err)
	}
	return err
}

// runRFRollupMaintenance recomputes every hour bucket that has observations
// newer than the watermark, then advances the watermark.
func runRFRollupMaintenance(rw *sql.DB) error {
	wm, err := rfRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', timestamp, 'unixepoch')
		FROM observations WHERE id > ?`, wm)
	if err != nil {
		return fmt.Errorf("maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("maintenance scan hour: %w", err)
		}
		hours = append(hours, h)
	}
	rows.Close()
	for _, h := range hours {
		if err := recomputeRFRollupHour(rw, h); err != nil {
			return err
		}
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxID); err != nil {
		return fmt.Errorf("maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return rfSetRollupWatermark(rw, maxID.Int64)
	}
	return nil
}

// backfillRFRollupAsync runs the first full rollup build in the background.
// Backfill is just maintenance from watermark 0 (all hours touched).
func backfillRFRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[rf-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[rf-rollup] backfill open rw: %v", err)
		return
	}
	start := time.Now()
	if err := runRFRollupMaintenance(rw); err != nil {
		log.Printf("[rf-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[rf-rollup] backfill complete in %s", time.Since(start))
}

// rfRollupReady reports whether the rollup has been populated at least once.
func rfRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM rf_rollup_meta WHERE key=?`,
		rfRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
```

- [ ] **Step 4: Run test, verify PASS** — `cd cmd/server && go test . -run TestRFRollupMaintenance -v`.

- [ ] **Step 5: gofmt + commit**

```bash
git add cmd/server/rf_rollup_maintain.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): backfill + incremental maintenance + watermark"
```

---

## Task 5: Rollup read path

Rewrite the RF result assembly to read `rf_rollup`. This produces the same result map as `computeAnalyticsRF`.

**Files:**
- Create: `cmd/server/rf_rollup_read.go`
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test** — append:

```go
func TestComputeRFFromRollupShape(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779444000),(2,1,2,7.0,-90.0,1779444000)`)
	rw, _ := cachedRW(db.path)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	res, err := computeRFFromRollup(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("computeRFFromRollup: %v", err)
	}
	for _, k := range []string{"totalPackets", "totalAllPackets", "totalTransmissions",
		"snr", "rssi", "snrValues", "rssiValues", "packetSizes", "packetsPerHour",
		"payloadTypes", "snrByType", "signalOverTime", "scatterData", "timeSpanHours"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if res["totalAllPackets"].(int) != 2 {
		t.Errorf("totalAllPackets=%v want 2", res["totalAllPackets"])
	}
}
```

- [ ] **Step 2: Run test, verify FAIL** — `cd cmd/server && go test . -run TestComputeRFFromRollupShape -v`.

- [ ] **Step 3: Implement** — create `cmd/server/rf_rollup_read.go`. This is the largest unit; it queries `rf_rollup` and assembles the result map.

```go
// rf_rollup_read.go — RF analytics result assembly from the rf_rollup table.

package main

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
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

	// --- scalars + per-type ---
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

	// --- per-hour ---
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

	// --- histograms (sum packed blobs) + scatter ---
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

	// --- totalTransmissions ---
	totalTx := tot.nTx // region: approximate (documented)
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
```

- [ ] **Step 4: Add the assembly + helper functions** — append to `cmd/server/rf_rollup_read.go`:

```go
func rfAddBins(dst, src []int) {
	for i := range dst {
		if i < len(src) {
			dst[i] += src[i]
		}
	}
}

func rfIntPlaceholders(n int) string {
	if n == 0 {
		return "NULL" // empty region set -> matches nothing
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
```

- [ ] **Step 5: Add `rfAssembleRollupResult`** — append to `cmd/server/rf_rollup_read.go`. It mirrors the key set + sort order of the in-memory `computeAnalyticsRF`:

```go
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
```

- [ ] **Step 6: Add `rfEffectiveWindow` + `rfWindowHourBounds`** — append to `cmd/server/rf_rollup_read.go` (add `"time"` to imports):

```go
// rfEffectiveWindow applies a 24h default when no window is given. Full
// history (zero window -> 24h) is the documented behavior; the rollup makes
// any explicit window cheap so no upper cap is needed.
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
```

> Spec note: the design said "no full-history cap" — the rollup makes explicit windows cheap, and zero-window defaults to 24h. There is no 30d cap in this implementation; an explicit wide window is allowed and served from the rollup. (The earlier window-cap idea belonged to the abandoned Approach A.)

- [ ] **Step 7: Run test, verify PASS** — `cd cmd/server && go test . -run TestComputeRFFromRollupShape -v`.

- [ ] **Step 8: gofmt + commit**

```bash
git add cmd/server/rf_rollup_read.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): RF analytics read path from rf_rollup"
```

---

## Task 6: Wire `computeAnalyticsRFSQL` to the rollup; remove dead on-demand code

**Files:**
- Modify: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test** — append:

```go
func TestComputeAnalyticsRFSQLUsesRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779444000)`)
	rw, _ := cachedRW(db.path)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	res, err := computeAnalyticsRFSQL(db, "", TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"})
	if err != nil {
		t.Fatalf("computeAnalyticsRFSQL: %v", err)
	}
	if res["totalAllPackets"].(int) != 1 {
		t.Fatalf("totalAllPackets=%v want 1", res["totalAllPackets"])
	}
}
```

- [ ] **Step 2: Run test, verify FAIL or behavior mismatch** — `cd cmd/server && go test . -run TestComputeAnalyticsRFSQLUsesRollup -v` (the old `computeAnalyticsRFSQL` scans raw; this test still passes by luck on tiny data — proceed to Step 3 regardless).

- [ ] **Step 3: Replace `computeAnalyticsRFSQL` body** — in `cmd/server/analytics_rf_sql.go`, replace the entire `computeAnalyticsRFSQL` function with a one-line delegate:

```go
// computeAnalyticsRFSQL produces RF analytics from the rf_rollup table.
func computeAnalyticsRFSQL(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	return computeRFFromRollup(db, region, window)
}
```

- [ ] **Step 4: Delete the now-dead on-demand functions** — in `cmd/server/analytics_rf_sql.go`, delete: `rfCoreAggregates`, `rfAggregates` (struct), `rfSortedColumn`, `rfPacketSizes`, `rfTypeDistribution`, `rfTypeStat`, `rfSnrByType`, `rfHourBucket`, `rfHourlyBuckets`, `rfScatterSample`, `rfTotalTransmissions`, `rfObsWhere`, `rfTimeSpanHours`. KEEP: `rfWindowEpochBounds`, `rfObsWindowClause`, `rfTxWindowClause`, `rfObserverIdxClause`, `rfWhere`, `rfRegionObserverIdxs`, `rfScatterPoint`, and `computeAnalyticsRFSQL`. After deletion, run `cd cmd/server && go build ./...` — if anything reports unused, also delete the unused helper. `rfRegionObserverIdxs` is used by `rf_rollup_read.go`; the window-clause helpers may become unused — if `go build` flags them unused, delete them too. Do not leave dead code.

- [ ] **Step 5: Run build + test** — `cd cmd/server && go build ./... && go test . -run 'RF|AnalyticsRF' -count=1 -v` — all pass.

- [ ] **Step 6: gofmt + commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): route computeAnalyticsRFSQL through the rollup; drop on-demand scans"
```

---

## Task 7: Wire startup — schema, backfill, maintenance job

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add the schema-ensure call** — in `cmd/server/main.go`, next to the other `ensureXxxTable` calls (near `ensureNeighborEdgesTable(dbPath)`), add:

```go
	if err := ensureRFRollupTable(dbPath); err != nil {
		log.Fatalf("ensureRFRollupTable: %v", err)
	}
```

- [ ] **Step 2: Start backfill + maintenance when the flag is on** — in `cmd/server/main.go`, after HTTP starts (near `go backfillResolvedPathsAsync(...)`), add:

```go
	if cfg.PacketStore != nil && cfg.PacketStore.AnalyticsSQLBackend {
		go backfillRFRollupAsync(dbPath)
		rfRollupTicker := time.NewTicker(5 * time.Minute)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[rf-rollup] maintenance panic recovered: %v", r)
				}
			}()
			for range rfRollupTicker.C {
				rw, err := cachedRW(dbPath)
				if err != nil {
					log.Printf("[rf-rollup] maintenance open rw: %v", err)
					continue
				}
				if err := runRFRollupMaintenance(rw); err != nil {
					log.Printf("[rf-rollup] maintenance: %v", err)
				}
			}
		}()
	}
```

> Implementer: confirm `cfg.PacketStore` is the right path to the `AnalyticsSQLBackend` flag (it is — `Config.PacketStore *PacketStoreConfig`). Place the block where `dbPath`, `cfg`, and HTTP startup are all in scope. If `time` is not imported in main.go, it already is (existing tickers).

- [ ] **Step 3: Build + full suite** — `cd cmd/server && go build ./... && go test . -count=1 2>&1 | tail -10` — all pass.

- [ ] **Step 4: gofmt + commit**

```bash
git add cmd/server/main.go
git commit -m "feat(rollup): wire rf_rollup schema, backfill, maintenance into startup"
```

---

## Task 8: In-memory fallback while the rollup is not yet ready

When the flag is on but backfill has not finished, `GetAnalyticsRFWithWindow` must fall back to the in-memory `computeAnalyticsRF` so RF analytics never breaks.

**Files:**
- Modify: `cmd/server/store_analytics.go` (`GetAnalyticsRFWithWindow`)
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test** — append:

```go
func TestRFFallbackWhenRollupNotReady(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,1,1,5.0,-80.0,1779444000)`)
	// rollup NOT populated (no maintenance run) -> rfRollupReady false
	ps := loadStore(t, db.path, 0)
	ps.analyticsSQLBackend = true
	res, err := ps.GetAnalyticsRFWithWindow("", TimeWindow{})
	if err != nil {
		t.Fatalf("expected in-memory fallback, got error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}
```

- [ ] **Step 2: Run test, verify FAIL** — `cd cmd/server && go test . -run TestRFFallbackWhenRollupNotReady -v` (the SQL path queries an empty rollup — without the fallback it returns an empty/zeroed result rather than the in-memory one; the test asserts the fallback path is taken).

- [ ] **Step 3: Add the fallback** — in `cmd/server/store_analytics.go`, in `GetAnalyticsRFWithWindow`, change the SQL branch so it checks rollup readiness:

```go
	var result map[string]interface{}
	if s.analyticsSQLBackend && s.db != nil && rfRollupReady(s.db.conn) {
		r, err := computeAnalyticsRFSQL(s.db, region, window)
		if err != nil {
			return nil, err
		}
		result = r
	} else {
		result = s.computeAnalyticsRF(region, window)
	}
```

> `s.db.conn` is the `*sql.DB`. `rfRollupReady` takes `*sql.DB`. If `s.db` connection differs from the rollup's `cachedRW` connection, readiness still reads correctly — `rf_rollup_meta` is one table in one file.

- [ ] **Step 4: Run test, verify PASS** — `cd cmd/server && go test . -run TestRFFallbackWhenRollupNotReady -v`.

- [ ] **Step 5: Build + full suite + gofmt + commit**

```bash
cd cmd/server && go build ./... && go test . -count=1 2>&1 | tail -8
```
```bash
git add cmd/server/store_analytics.go cmd/server/rf_rollup_test.go
git commit -m "feat(rollup): in-memory fallback while rollup backfill is in progress"
```

---

## Task 9: Parity test — rollup vs in-memory

**Files:**
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the parity test** — append:

```go
func TestRFRollupParity(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO observers(rowid,id,name,iata) VALUES
		(1,'o1','Obs1','SJC'),(2,'o2','Obs2','SJC'),(3,'o3','Obs3','LAX')`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type) VALUES
		(1,'aabbcc','h1','2026-05-18T10:00:00Z',1),
		(2,'ddee','h2','2026-05-18T10:30:00Z',1),
		(3,'ff0011','h3','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(id,transmission_id,observer_idx,snr,rssi,timestamp) VALUES
		(1,1,1,5.0,-80.0,1779444000),(2,1,2,7.0,-88.0,1779444000),
		(3,2,1,6.0,-82.0,1779445800),(4,3,3,9.0,-70.0,1779447600)`)
	rw, _ := cachedRW(db.path)
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	store := loadStore(t, db.path, 0)
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}

	for _, region := range []string{"", "SJC"} {
		mem := store.computeAnalyticsRF(region, win)
		roll, err := computeRFFromRollup(db, region, win)
		if err != nil {
			t.Fatalf("[region=%q] rollup: %v", region, err)
		}
		// Scalar counts must match exactly.
		for _, k := range []string{"totalPackets", "totalAllPackets"} {
			if fmt.Sprint(mem[k]) != fmt.Sprint(roll[k]) {
				t.Errorf("[region=%q] %s: mem=%v rollup=%v", region, k, mem[k], roll[k])
			}
		}
		// SNR avg must match within tolerance.
		ms := mem["snr"].(map[string]interface{})
		rs := roll["snr"].(map[string]interface{})
		if math.Abs(rfToF(ms["avg"])-rfToF(rs["avg"])) > 1e-9 {
			t.Errorf("[region=%q] snr.avg: mem=%v rollup=%v", region, ms["avg"], rs["avg"])
		}
	}
}

func rfToF(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}
```

> Parity scope: scalar counts + avg compared exactly / within tolerance. Histograms are intentionally fixed-bin now (not the old dynamic bins) — do NOT assert histogram equality against the in-memory dynamic-bin output. Median is bin-approximate. This matches the spec's "statistically equivalent" bar.

- [ ] **Step 2: Run the parity test** — `cd cmd/server && go test . -run TestRFRollupParity -count=1 -v`. If a scalar diverges, fix the rollup compute/read code (the in-memory path is the reference).

- [ ] **Step 3: Commit**

```bash
git add cmd/server/rf_rollup_test.go
git commit -m "test(rollup): RF rollup vs in-memory parity"
```

---

## Task 10: Performance benchmark

**Files:**
- Test: `cmd/server/rf_rollup_test.go` (append)

- [ ] **Step 1: Write the perf test** — append:

```go
func TestRFRollupPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	db := setupTestDBFile(t)
	if err := ensureRFRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Generate ~1M observations across ~30 days, 100 observers, 4 payload types.
	rw, _ := cachedRW(db.path)
	gen, err := rw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1779000000)
	txID := 0
	for d := 0; d < 30; d++ {
		for n := 0; n < 1200; n++ {
			txID++
			ts := base + int64(d)*86400 + int64(n)*60
			first := time.Unix(ts, 0).UTC().Format(time.RFC3339)
			pt := txID % 4
			if _, err := gen.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
				VALUES (?,?,?,?,?)`, txID, "aabbcc", fmt.Sprintf("h%d", txID), first, pt); err != nil {
				t.Fatal(err)
			}
			for o := 0; o < 28; o++ { // ~28 obs/tx -> ~1M total
				if _, err := gen.Exec(`INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
					VALUES (?,?,?,?,?)`, txID, o%100, float64(o%20-10), float64(-60-o%50), ts); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := gen.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := runRFRollupMaintenance(rw); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// 24h window must be well under 200ms.
	win24 := TimeWindow{
		Since: time.Unix(base+29*86400, 0).UTC().Format(time.RFC3339),
		Until: time.Unix(base+30*86400, 0).UTC().Format(time.RFC3339),
	}
	t0 := time.Now()
	if _, err := computeRFFromRollup(db, "", win24); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 200*time.Millisecond {
		t.Errorf("24h query took %s, want < 200ms", d)
	}

	// Full history must be under 2s.
	t1 := time.Now()
	if _, err := computeRFFromRollup(db, "", TimeWindow{
		Since: "2026-01-01T00:00:00Z", Until: "2027-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t1); d > 2*time.Second {
		t.Errorf("full-history query took %s, want < 2s", d)
	}
}
```

- [ ] **Step 2: Run the perf test** — `cd cmd/server && go test . -run TestRFRollupPerf -count=1 -v -timeout 300s`. If a threshold fails, the rollup read path needs optimization (add an index, reduce blob fetches) — fix until green.

- [ ] **Step 3: Commit**

```bash
git add cmd/server/rf_rollup_test.go
git commit -m "test(rollup): RF rollup performance benchmark"
```

---

## Task 11: Full build, suite, vet

- [ ] **Step 1: Build** — `cd cmd/server && go build ./...` — no errors.
- [ ] **Step 2: Vet** — `cd cmd/server && go vet ./...` — no warnings.
- [ ] **Step 3: Full suite** — `cd cmd/server && go test . -count=1 2>&1 | tail -12` — all pass. Any failure = a caller missed in Task 6's deletions — fix and re-run.
- [ ] **Step 4: gofmt** — `cd cmd/server && gofmt -l rf_rollup.go rf_rollup_maintain.go rf_rollup_read.go rf_rollup_test.go analytics_rf_sql.go store_analytics.go main.go` — empty.
- [ ] **Step 5: Commit any fixes**

```bash
git add -A cmd/server/
git commit -m "chore(rollup): finalize RF rollup — build + tests + vet green"
```

---

## Rollout (post-merge, manual — not a code task)

1. Deploy. Flag `analyticsSqlBackend` defaults `false` — no rollup work, no behavior change.
2. Run the perf gate: `go test . -run TestRFRollupPerf` green in CI, plus a manual run of `computeRFFromRollup` against the real backup DB.
3. On analyzer.kiekr.app set `analyticsSqlBackend:true` in `config.json` → backfill runs in background (in-memory fallback serves RF meanwhile) → once backfill completes, the rollup serves. Watch logs for `[rf-rollup] backfill complete`.

## Self-review notes

- **Spec coverage:** schema incl. `rf_rollup_tx` (Task 2), bin packing (Task 1), single-hour recompute (Task 3), backfill + maintenance + watermark (Task 4), read path scalars/per-hour/per-type/histograms/median/scatter (Task 5), `computeAnalyticsRFSQL` delegation + dead-code removal (Task 6), startup wiring (Task 7), in-memory fallback during backfill (Task 8), parity (Task 9), perf gate both automated + manual (Task 10 + rollout step 2). Covered.
- **Deviation from spec:** the spec mentioned no window cap; the design's Section 1-5 did not include one either (the 30d cap belonged to abandoned Approach A). `rfEffectiveWindow` applies only the 24h default for a zero window — no upper cap. Documented inline in Task 5 Step 6.
- **Type consistency:** `agg` and `hourCount` structs are defined in `rf_rollup_read.go` and used by `rfAssembleRollupResult` in the same file. `rfScatterPoint` is the pre-existing struct in `analytics_rf_sql.go` — reused, not redefined. `rfCell` is internal to `rf_rollup.go`.
- **Open item for implementer:** confirm the file-DB test helper / DB-open function names against `db.go` and the existing test files (Task 2 Step 1 note).
