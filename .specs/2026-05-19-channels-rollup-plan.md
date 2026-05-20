# Channels Analytics Rollup — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Migrate channels analytics off the in-memory `PacketStore` onto pre-aggregated SQLite rollup tables, so channel analytics cover full history and stay fast.

**Architecture:** Background-maintained `channel_rollup` / `channel_sender_rollup` / `channel_rollup_tx` tables (per-hour, per-channel-hash, per-observer). `computeAnalyticsChannels`'s in-memory path is replaced by a rollup read path on the SQL backend; gated by the existing `packetStore.analyticsSqlBackend` flag; in-memory stays as fallback + parity reference. Mirrors the proven RF rollup (`cmd/server/rf_rollup*.go`, spec `.specs/2026-05-19-rf-analytics-rollup-design.md`).

**Tech Stack:** Go, `database/sql` + modernc.org/sqlite.

Spec: `.specs/2026-05-19-channels-rollup-design.md`

---

## Build commands

Per-directory Go modules — run `go`/`gofmt` from inside `cmd/server/` (module `github.com/corescope/server`). `git` from the worktree root. `gofmt -l` every touched file — must be empty. Never `gofmt -w` a whole pre-existing file.

## Verified facts

- The RF rollup (committed, `main` at `d19d1b89`) is the proven template. Reusable as-is from `rf_rollup.go`: `rfBinIndex(v,min,width,count)`, `rfPackBins([]int)`, `rfUnpackBins([]byte,count)` — generic bin helpers, NOT RF-specific. Reuse them; do not duplicate.
- `cachedRW(dbPath) (*sql.DB, error)` opens a write connection with `PRAGMA busy_timeout=5000`.
- `transmissions(id, raw_hex, hash, first_seen TEXT RFC3339, payload_type, decoded_json TEXT, …)`. Channel messages are `payload_type = 5`. `idx_transmissions_first_seen` and `idx_transmissions_payload_type` exist.
- `observations(transmission_id, observer_idx, …)` — `observer_idx` is nullable.
- `computeAnalyticsChannels(region string, window TimeWindow) map[string]interface{}` in `store_channels.go` is the in-memory reference. It unmarshals each tx's `decoded_json` into `{channelHash|channel_hash, channel, text, sender}`, groups by channel hash byte, validates names with `channelNameMatchesHash`, treats `text=="" && sender==""` as encrypted. Helpers in `store_channels.go`: `channelNameMatchesHash`, `isPlaceholderName`. Output keys: `activeChannels, decryptable, channels[], topSenders[], channelTimeline[], msgLengths`.
- `GetAnalyticsChannelsWithWindow` is the TTL-cached wrapper (`chanCache`); returns `map[string]interface{}`.
- `rfRegionObserverIdxs(db *DB, region string) ([]int, error)` — reuse for region→observer_idx.
- Test helpers: `setupTestDBFile(t) *DB` (file-backed, full schema — in `rf_rollup_test.go`), `mustExec`, `cachedRW`, `loadStore`.

## File structure

- **Create** `cmd/server/channel_rollup.go` — msg-length bin constants, `ensureChannelRollupTable`, `recomputeChannelRollupHour`.
- **Create** `cmd/server/channel_rollup_maintain.go` — backfill, watermark, maintenance.
- **Create** `cmd/server/channel_rollup_read.go` — `computeChannelsFromRollup`.
- **Modify** `cmd/server/store_channels.go` — `GetAnalyticsChannelsWithWindow` branches to the rollup.
- **Modify** `cmd/server/main.go` — wire schema + backfill + maintenance.
- **Modify** `public/analytics.js` — render `msgLengths` as a histogram.
- **Create** `cmd/server/channel_rollup_test.go` — unit + parity + perf tests.

---

## Task 1: Schema + msg-length bins

**Files:** Create `cmd/server/channel_rollup.go`; Test `cmd/server/channel_rollup_test.go`.

- [ ] **Step 1: failing test** — create `cmd/server/channel_rollup_test.go`:

```go
package main

import "testing"

func TestEnsureChannelRollupTable(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatalf("ensureChannelRollupTable: %v", err)
	}
	for _, tbl := range []string{"channel_rollup", "channel_sender_rollup", "channel_rollup_tx", "channel_rollup_meta"} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}
```

- [ ] **Step 2: run, verify FAIL** — `cd cmd/server && go test . -run TestEnsureChannelRollupTable -v`.

- [ ] **Step 3: implement** — create `cmd/server/channel_rollup.go`:

```go
// channel_rollup.go — channels analytics rollup: schema, single-hour recompute.
// See .specs/2026-05-19-channels-rollup-design.md. Mirrors rf_rollup.go.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Message-length histogram bins: 0..512 bytes, 16-byte width -> 32 bins.
const (
	chMsgLenBinMin, chMsgLenBinWidth, chMsgLenBinCount = 0, 16, 32
)

// ensureChannelRollupTable creates the channels rollup tables. Idempotent.
func ensureChannelRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for channel_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS channel_rollup (
			hour TEXT NOT NULL,
			channel_hash TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			msg_count INTEGER NOT NULL DEFAULT 0,
			decrypted_count INTEGER NOT NULL DEFAULT 0,
			name TEXT,
			last_activity TEXT,
			msglen_sum INTEGER NOT NULL DEFAULT 0,
			msglen_count INTEGER NOT NULL DEFAULT 0,
			msglen_min INTEGER, msglen_max INTEGER,
			msglen_bins BLOB,
			PRIMARY KEY (hour, channel_hash, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_rollup_hour ON channel_rollup(hour)`,
		`CREATE TABLE IF NOT EXISTS channel_sender_rollup (
			hour TEXT NOT NULL,
			channel_hash TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			sender TEXT NOT NULL,
			msg_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, channel_hash, observer_idx, sender)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_sender_rollup_hour ON channel_sender_rollup(hour)`,
		`CREATE TABLE IF NOT EXISTS channel_rollup_tx (
			hour TEXT NOT NULL,
			channel_hash TEXT NOT NULL,
			distinct_tx INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, channel_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_rollup_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("channel_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}

var _ = json.Unmarshal // used by recompute (Task 2)
var _ = sql.ErrNoRows
var _ = time.Now
```

- [ ] **Step 4: run, verify PASS** — `cd cmd/server && go test . -run TestEnsureChannelRollupTable -v`.
- [ ] **Step 5: gofmt** — `cd cmd/server && gofmt -l channel_rollup.go channel_rollup_test.go` (empty).
- [ ] **Step 6: commit**

```bash
git add cmd/server/channel_rollup.go cmd/server/channel_rollup_test.go
git commit -m "feat(channels-rollup): schema + msg-length bin constants"
```

---

## Task 2: Single-hour recompute

**Files:** Modify `cmd/server/channel_rollup.go` (append); Test append.

- [ ] **Step 1: failing test** — append to `channel_rollup_test.go`:

```go
func TestRecomputeChannelRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Two type-5 (channel) transmissions in hour 2026-05-18T10.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',5,'{"channel_hash":"7","channel":"#test","sender":"alice","text":"hello"}'),
		       (2,'bb','h2','2026-05-18T10:30:00Z',5,'{"channel_hash":"7","channel":"#test","sender":"bob","text":"hi there"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp)
		VALUES (1,1,1779098400),(2,1,1779100200)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeChannelRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	var msgs, senders int
	rw.QueryRow(`SELECT COALESCE(SUM(msg_count),0) FROM channel_rollup WHERE hour=?`,
		"2026-05-18T10").Scan(&msgs)
	rw.QueryRow(`SELECT COUNT(DISTINCT sender) FROM channel_sender_rollup WHERE hour=?`,
		"2026-05-18T10").Scan(&senders)
	if msgs != 2 || senders != 2 {
		t.Fatalf("msg_count=%d senders=%d, want 2 and 2", msgs, senders)
	}
	var dtx int
	rw.QueryRow(`SELECT COALESCE(SUM(distinct_tx),0) FROM channel_rollup_tx WHERE hour=?`,
		"2026-05-18T10").Scan(&dtx)
	if dtx != 2 {
		t.Fatalf("distinct_tx=%d want 2", dtx)
	}
}
```

- [ ] **Step 2: run, verify FAIL** — `cd cmd/server && go test . -run TestRecomputeChannelRollupHour -v`.

- [ ] **Step 3: implement** — append to `cmd/server/channel_rollup.go` (delete the three `var _` lines from Task 1 — the imports are now used by real code):

```go
// chChanCell accumulates one (channel_hash, observer_idx) cell for an hour.
type chChanCell struct {
	msgCount, decryptedCount       int
	name                          string
	lastActivity                  string
	msglenSum, msglenCount         int
	msglenMin, msglenMax           int
	haveMsglen                     bool
	msglenBins                     []int
	senders                        map[string]int // sender -> msg count
	txSeen                         map[int]bool
}

func newChChanCell() *chChanCell {
	return &chChanCell{
		msglenBins: make([]int, chMsgLenBinCount),
		senders:    map[string]int{},
		txSeen:     map[int]bool{},
	}
}

// chDecodedGrp mirrors the decoded_json shape computeAnalyticsChannels reads.
type chDecodedGrp struct {
	Channel      string      `json:"channel"`
	ChannelHash  interface{} `json:"channelHash"`
	ChannelHash2 string      `json:"channel_hash"`
	Text         string      `json:"text"`
	Sender       string      `json:"sender"`
}

func chHashStr(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		return fmt.Sprintf("%v", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// recomputeChannelRollupHour rebuilds the channel rollup for one hour bucket
// ("2026-05-18T10") from raw type-5 transmissions. Idempotent. The read runs
// outside the write transaction and uses the indexed first_seen range.
func recomputeChannelRollupHour(rw *sql.DB, hour string) error {
	ht, err := time.Parse("2006-01-02T15", hour)
	if err != nil {
		return fmt.Errorf("channel recompute parse hour %q: %w", hour, err)
	}
	lo := ht.UTC().Format("2006-01-02T15:04:05Z")
	hi := ht.UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")

	rows, err := rw.Query(`
		SELECT t.id, t.first_seen, t.decoded_json, o.observer_idx
		FROM transmissions t
		LEFT JOIN observations o ON o.transmission_id = t.id
		WHERE t.payload_type = 5 AND t.first_seen >= ? AND t.first_seen < ?`, lo, hi)
	if err != nil {
		return fmt.Errorf("channel recompute scan: %w", err)
	}
	// cells keyed by (channel_hash, observer_idx)
	type ck struct {
		hash string
		obs  int
	}
	cells := map[ck]*chChanCell{}
	txByChan := map[string]map[int]bool{} // channel_hash -> set of tx ids
	for rows.Next() {
		var txID int
		var firstSeen, decodedJSON string
		var obsN sql.NullInt64
		if err := rows.Scan(&txID, &firstSeen, &decodedJSON, &obsN); err != nil {
			rows.Close()
			return fmt.Errorf("channel recompute row: %w", err)
		}
		obsIdx := -1
		if obsN.Valid {
			obsIdx = int(obsN.Int64)
		}
		var d chDecodedGrp
		if json.Unmarshal([]byte(decodedJSON), &d) != nil {
			continue
		}
		hash := chHashStr(d.ChannelHash)
		if hash == "" {
			hash = d.ChannelHash2
		}
		if hash == "" {
			hash = "?"
		}
		name := d.Channel
		if name == "" {
			name = "ch" + hash
		}
		encrypted := d.Text == "" && d.Sender == ""
		if name != "" && name != "ch"+hash && !channelNameMatchesHash(name, hash) {
			name = "ch" + hash
			encrypted = true
		}
		key := ck{hash, obsIdx}
		c := cells[key]
		if c == nil {
			c = newChChanCell()
			c.name = name
			cells[key] = c
		} else if isPlaceholderName(c.name) && !isPlaceholderName(name) {
			c.name = name
		}
		c.msgCount++
		c.lastActivity = firstSeen
		if !encrypted {
			c.decryptedCount++
		}
		if d.Sender != "" {
			c.senders[d.Sender]++
		}
		if d.Text != "" {
			n := len(d.Text)
			c.msglenSum += n
			c.msglenCount++
			if !c.haveMsglen || n < c.msglenMin {
				c.msglenMin = n
			}
			if !c.haveMsglen || n > c.msglenMax {
				c.msglenMax = n
			}
			c.haveMsglen = true
			c.msglenBins[rfBinIndex(n, chMsgLenBinMin, chMsgLenBinWidth, chMsgLenBinCount)]++
		}
		c.txSeen[txID] = true
		if txByChan[hash] == nil {
			txByChan[hash] = map[int]bool{}
		}
		txByChan[hash][txID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("channel recompute rows: %w", err)
	}

	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("channel recompute begin: %w", err)
	}
	defer tx.Rollback()
	for _, t := range []string{"channel_rollup", "channel_sender_rollup", "channel_rollup_tx"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE hour=?`, hour); err != nil {
			return fmt.Errorf("channel recompute delete %s: %w", t, err)
		}
	}
	for key, c := range cells {
		var mn, mx interface{}
		if c.haveMsglen {
			mn, mx = c.msglenMin, c.msglenMax
		}
		if _, err := tx.Exec(`INSERT INTO channel_rollup
			(hour,channel_hash,observer_idx,msg_count,decrypted_count,name,last_activity,
			 msglen_sum,msglen_count,msglen_min,msglen_max,msglen_bins)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			hour, key.hash, key.obs, c.msgCount, c.decryptedCount, c.name, c.lastActivity,
			c.msglenSum, c.msglenCount, mn, mx, rfPackBins(c.msglenBins)); err != nil {
			return fmt.Errorf("channel recompute insert rollup: %w", err)
		}
		for sender, cnt := range c.senders {
			if _, err := tx.Exec(`INSERT INTO channel_sender_rollup
				(hour,channel_hash,observer_idx,sender,msg_count) VALUES (?,?,?,?,?)`,
				hour, key.hash, key.obs, sender, cnt); err != nil {
				return fmt.Errorf("channel recompute insert sender: %w", err)
			}
		}
	}
	for hash, set := range txByChan {
		if _, err := tx.Exec(`INSERT INTO channel_rollup_tx(hour,channel_hash,distinct_tx)
			VALUES (?,?,?)`, hour, hash, len(set)); err != nil {
			return fmt.Errorf("channel recompute insert tx: %w", err)
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: run, verify PASS** — `cd cmd/server && go test . -run TestRecomputeChannelRollupHour -v`.
- [ ] **Step 5: gofmt + build** — `cd cmd/server && gofmt -l channel_rollup.go channel_rollup_test.go && go build ./...`.
- [ ] **Step 6: commit**

```bash
git add cmd/server/channel_rollup.go cmd/server/channel_rollup_test.go
git commit -m "feat(channels-rollup): single-hour recompute"
```

---

## Task 3: Backfill + watermark + maintenance

**Files:** Create `cmd/server/channel_rollup_maintain.go`; Test append.

This mirrors `rf_rollup_maintain.go` exactly, with `rf`→`channel`, the watermark keyed off `transmissions.id` (the channel rollup's unit is the transmission), and the touched-hours query over `transmissions` filtered to `payload_type=5`.

- [ ] **Step 1: failing test** — append to `channel_rollup_test.go`:

```go
func TestChannelRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',5,'{"channel_hash":"7","channel":"#t","sender":"a","text":"x"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp) VALUES (1,1,1779098400)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runChannelRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	var n int
	rw.QueryRow(`SELECT COALESCE(SUM(msg_count),0) FROM channel_rollup`).Scan(&n)
	if n != 1 {
		t.Fatalf("after run 1 msg_count=%d want 1", n)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (2,'bb','h2','2026-05-18T10:05:00Z',5,'{"channel_hash":"7","channel":"#t","sender":"b","text":"y"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp) VALUES (2,1,1779098700)`)
	if err := runChannelRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(SUM(msg_count),0) FROM channel_rollup`).Scan(&n)
	if n != 2 {
		t.Fatalf("after run 2 msg_count=%d want 2", n)
	}
}
```

- [ ] **Step 2: run, verify FAIL.**

- [ ] **Step 3: implement** — create `cmd/server/channel_rollup_maintain.go`:

```go
// channel_rollup_maintain.go — channels rollup backfill + maintenance.
// Mirrors rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

const chRollupWatermarkKey = "channel_rollup_last_tx_id"

var chRollupMaintMu sync.Mutex

func chRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM channel_rollup_meta WHERE key=?`,
		chRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("channel watermark read: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func chSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO channel_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		chRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("channel watermark set: %w", err)
	}
	return nil
}

// runChannelRollupMaintenance recomputes every hour with type-5 transmissions
// newer than the watermark, then advances the watermark.
func runChannelRollupMaintenance(rw *sql.DB) error {
	wm, err := chRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', first_seen)
		FROM transmissions WHERE payload_type = 5 AND id > ?`, wm)
	if err != nil {
		return fmt.Errorf("channel maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("channel maintenance scan hour: %w", err)
		}
		hours = append(hours, h)
	}
	rows.Close()
	for _, h := range hours {
		if err := recomputeChannelRollupHour(rw, h); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM transmissions WHERE payload_type=5`).Scan(&maxID); err != nil {
		return fmt.Errorf("channel maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return chSetRollupWatermark(rw, maxID.Int64)
	}
	return nil
}

func runChannelRollupMaintenanceGuarded(rw *sql.DB) {
	if !chRollupMaintMu.TryLock() {
		log.Printf("[channel-rollup] maintenance skipped — run in progress")
		return
	}
	defer chRollupMaintMu.Unlock()
	if err := runChannelRollupMaintenance(rw); err != nil {
		log.Printf("[channel-rollup] maintenance: %v", err)
	}
}

func backfillChannelRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[channel-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[channel-rollup] backfill open rw: %v", err)
		return
	}
	chRollupMaintMu.Lock()
	defer chRollupMaintMu.Unlock()
	start := time.Now()
	if err := runChannelRollupMaintenance(rw); err != nil {
		log.Printf("[channel-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[channel-rollup] backfill complete in %s", time.Since(start))
}

func chRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM channel_rollup_meta WHERE key=?`,
		chRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
```

> Note: `strftime('%Y-%m-%dT%H', first_seen)` on an RFC3339 text column produces `YYYY-MM-DDTHH`. SQLite's `strftime` parses RFC3339 text directly (no `unixepoch` modifier). The touched-hours query is small (only type-5 rows past the watermark); the per-hour recompute uses the indexed `first_seen` range (Task 2).

- [ ] **Step 4: run, verify PASS** — `cd cmd/server && go test . -run TestChannelRollupMaintenance -v`.
- [ ] **Step 5: gofmt + build + commit**

```bash
git add cmd/server/channel_rollup_maintain.go cmd/server/channel_rollup_test.go
git commit -m "feat(channels-rollup): backfill + maintenance + watermark"
```

---

## Task 4: Read path — `computeChannelsFromRollup`

**Files:** Create `cmd/server/channel_rollup_read.go`; Test append.

- [ ] **Step 1: failing test** — append to `channel_rollup_test.go`:

```go
func TestComputeChannelsFromRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json) VALUES
		(1,'aa','h1','2026-05-18T10:00:00Z',5,'{"channel_hash":"7","channel":"#test","sender":"alice","text":"hello"}'),
		(2,'bb','h2','2026-05-18T10:30:00Z',5,'{"channel_hash":"7","channel":"#test","sender":"bob","text":"hi"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp)
		VALUES (1,1,1779098400),(2,1,1779100200)`)
	rw, _ := cachedRW(db.path)
	if err := runChannelRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}
	res, err := computeChannelsFromRollup(db, "", win)
	if err != nil {
		t.Fatalf("computeChannelsFromRollup: %v", err)
	}
	for _, k := range []string{"activeChannels", "decryptable", "channels", "topSenders",
		"channelTimeline", "msgLengths"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if res["activeChannels"].(int) != 1 {
		t.Errorf("activeChannels=%v want 1", res["activeChannels"])
	}
	chans := res["channels"].([]map[string]interface{})
	if len(chans) != 1 || chans[0]["messages"].(int) != 2 || chans[0]["senders"].(int) != 2 {
		t.Errorf("channels wrong: %#v", chans)
	}
}
```

- [ ] **Step 2: run, verify FAIL.**

- [ ] **Step 3: implement** — create `cmd/server/channel_rollup_read.go`:

```go
// channel_rollup_read.go — channels analytics result assembly from the rollup.

package main

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// computeChannelsFromRollup builds the channels analytics result map from the
// channel_rollup tables. region "" = global; window zero -> default 24h.
func computeChannelsFromRollup(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
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

	// Per-channel aggregation.
	type chAgg struct {
		hash, name              string
		msgCount, decryptedCount int
		lastActivity            string
	}
	chRows, err := db.conn.Query(`
		SELECT channel_hash, SUM(msg_count), SUM(decrypted_count),
		       MAX(last_activity), MAX(name)
		FROM channel_rollup WHERE `+where+` GROUP BY channel_hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel rollup query: %w", err)
	}
	byHash := map[string]*chAgg{}
	for chRows.Next() {
		a := &chAgg{}
		var name sql.NullString
		var la sql.NullString
		if err := chRows.Scan(&a.hash, &a.msgCount, &a.decryptedCount, &la, &name); err != nil {
			chRows.Close()
			return nil, fmt.Errorf("channel rollup scan: %w", err)
		}
		a.name = name.String
		a.lastActivity = la.String
		byHash[a.hash] = a
	}
	chRows.Close()

	// Distinct senders per channel + top senders.
	senderByHash := map[string]int{}
	sndRows, err := db.conn.Query(`
		SELECT channel_hash, COUNT(DISTINCT sender)
		FROM channel_sender_rollup WHERE `+where+` GROUP BY channel_hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel sender count query: %w", err)
	}
	for sndRows.Next() {
		var h string
		var n int
		if err := sndRows.Scan(&h, &n); err != nil {
			sndRows.Close()
			return nil, fmt.Errorf("channel sender count scan: %w", err)
		}
		senderByHash[h] = n
	}
	sndRows.Close()

	topRows, err := db.conn.Query(`
		SELECT sender, SUM(msg_count) AS c
		FROM channel_sender_rollup WHERE `+where+`
		GROUP BY sender ORDER BY c DESC LIMIT 15`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel top-sender query: %w", err)
	}
	topSenders := make([]map[string]interface{}, 0, 15)
	for topRows.Next() {
		var name string
		var c int
		if err := topRows.Scan(&name, &c); err != nil {
			topRows.Close()
			return nil, fmt.Errorf("channel top-sender scan: %w", err)
		}
		topSenders = append(topSenders, map[string]interface{}{"name": name, "count": c})
	}
	topRows.Close()

	// Exact distinct-message count per channel (no-region only).
	exactTx := map[string]int{}
	if region == "" {
		txRows, err := db.conn.Query(`
			SELECT channel_hash, SUM(distinct_tx) FROM channel_rollup_tx
			WHERE hour >= ? AND hour <= ? GROUP BY channel_hash`, sinceHour, untilHour)
		if err != nil {
			return nil, fmt.Errorf("channel rollup_tx query: %w", err)
		}
		for txRows.Next() {
			var h string
			var n int
			if err := txRows.Scan(&h, &n); err != nil {
				txRows.Close()
				return nil, fmt.Errorf("channel rollup_tx scan: %w", err)
			}
			exactTx[h] = n
		}
		txRows.Close()
	}

	// Hourly timeline (by channel name).
	tlRows, err := db.conn.Query(`
		SELECT hour, MAX(name), SUM(msg_count)
		FROM channel_rollup WHERE `+where+` GROUP BY hour, channel_hash ORDER BY hour`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel timeline query: %w", err)
	}
	channelTimeline := make([]map[string]interface{}, 0)
	for tlRows.Next() {
		var hr string
		var nm sql.NullString
		var c int
		if err := tlRows.Scan(&hr, &nm, &c); err != nil {
			tlRows.Close()
			return nil, fmt.Errorf("channel timeline scan: %w", err)
		}
		channelTimeline = append(channelTimeline, map[string]interface{}{
			"hour": hr, "channel": nm.String, "count": c,
		})
	}
	tlRows.Close()

	// msgLengths histogram (sum the bin blobs).
	msglenBins := make([]int, chMsgLenBinCount)
	blobRows, err := db.conn.Query(`SELECT msglen_bins FROM channel_rollup WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("channel msglen query: %w", err)
	}
	for blobRows.Next() {
		var b []byte
		if err := blobRows.Scan(&b); err != nil {
			blobRows.Close()
			return nil, fmt.Errorf("channel msglen scan: %w", err)
		}
		rfAddBins(msglenBins, rfUnpackBins(b, chMsgLenBinCount))
	}
	blobRows.Close()

	// Assemble channels list.
	channels := make([]map[string]interface{}, 0, len(byHash))
	decryptable := 0
	for hash, a := range byHash {
		enc := a.decryptedCount == 0
		if !enc {
			decryptable++
		}
		msgs := a.msgCount
		if region == "" {
			if ex, ok := exactTx[hash]; ok {
				msgs = ex
			}
		}
		name := a.name
		if name == "" {
			name = "ch" + hash
		}
		channels = append(channels, map[string]interface{}{
			"hash": hash, "name": name, "messages": msgs,
			"senders": senderByHash[hash], "lastActivity": a.lastActivity,
			"encrypted": enc,
		})
	}
	sort.Slice(channels, func(i, j int) bool {
		return channels[i]["messages"].(int) > channels[j]["messages"].(int)
	})

	return map[string]interface{}{
		"activeChannels":  len(channels),
		"decryptable":     decryptable,
		"channels":        channels,
		"topSenders":      topSenders,
		"channelTimeline": channelTimeline,
		"msgLengths":      rfHistogramFromBins(msglenBins, chMsgLenBinMin, chMsgLenBinWidth),
	}, nil
}

var _ = time.Now
```

> `rfEffectiveWindow`, `rfWindowHourBounds`, `rfIntPlaceholders`, `rfAddBins`, `rfUnpackBins`, `rfHistogramFromBins` are reused from `rf_rollup_read.go`/`rf_rollup.go`. If `time` ends up unused, remove the import and the `var _ = time.Now` line.

- [ ] **Step 4: run, verify PASS** — `cd cmd/server && go test . -run TestComputeChannelsFromRollup -v`.
- [ ] **Step 5: gofmt + build + commit**

```bash
git add cmd/server/channel_rollup_read.go cmd/server/channel_rollup_test.go
git commit -m "feat(channels-rollup): read path computeChannelsFromRollup"
```

---

## Task 5: Wire `GetAnalyticsChannelsWithWindow` to the rollup

**Files:** Modify `cmd/server/store_channels.go`.

- [ ] **Step 1: failing test** — append to `channel_rollup_test.go`:

```go
func TestGetAnalyticsChannelsUsesRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',5,'{"channel_hash":"7","channel":"#t","sender":"a","text":"x"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp) VALUES (1,1,1779098400)`)
	rw, _ := cachedRW(db.path)
	if err := runChannelRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	ps := loadStore(t, db.path, 0)
	ps.analyticsSQLBackend = true
	res := ps.GetAnalyticsChannelsWithWindow("",
		TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"})
	if res["activeChannels"].(int) != 1 {
		t.Fatalf("activeChannels=%v want 1", res["activeChannels"])
	}
}
```

- [ ] **Step 2: run, verify FAIL.**

- [ ] **Step 3: implement** — in `cmd/server/store_channels.go`, change the cache-miss line of `GetAnalyticsChannelsWithWindow`. It currently is:

```go
	result := s.computeAnalyticsChannels(region, window)
```

Replace with:

```go
	var result map[string]interface{}
	if s.analyticsSQLBackend && s.db != nil && chRollupReady(s.db.conn) {
		r, err := computeChannelsFromRollup(s.db, region, window)
		if err != nil {
			log.Printf("[channel-rollup] read error, falling back to in-memory: %v", err)
			result = s.computeAnalyticsChannels(region, window)
		} else {
			result = r
		}
	} else {
		result = s.computeAnalyticsChannels(region, window)
	}
```

If `store_channels.go` does not already import `log`, add it. (Channels uses graceful fallback on a rollup error — unlike RF which returns 500 — because channel analytics are non-critical and a working in-memory answer beats an error page.)

- [ ] **Step 4: run, verify PASS** — `cd cmd/server && go test . -run TestGetAnalyticsChannelsUsesRollup -v`.
- [ ] **Step 5: build + full suite + gofmt + commit**

```bash
cd cmd/server && go build ./... && go test . -count=1 2>&1 | tail -6
```
```bash
git add cmd/server/store_channels.go cmd/server/channel_rollup_test.go
git commit -m "feat(channels-rollup): route GetAnalyticsChannelsWithWindow through the rollup"
```

---

## Task 6: Wire startup

**Files:** Modify `cmd/server/main.go`.

- [ ] **Step 1: schema ensure** — next to `ensureRFRollupTable(dbPath)` in `main.go`, add:

```go
	if err := ensureChannelRollupTable(dbPath); err != nil {
		log.Fatalf("ensureChannelRollupTable: %v", err)
	}
```

- [ ] **Step 2: backfill + maintenance** — in the existing `if cfg.PacketStore != nil && cfg.PacketStore.AnalyticsSQLBackend {` block (added for the RF rollup), add alongside the RF backfill/ticker:

```go
		go backfillChannelRollupAsync(dbPath)
		chRollupTicker := time.NewTicker(5 * time.Minute)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[channel-rollup] maintenance panic recovered: %v", r)
				}
			}()
			for range chRollupTicker.C {
				rw, err := cachedRW(dbPath)
				if err != nil {
					log.Printf("[channel-rollup] maintenance open rw: %v", err)
					continue
				}
				runChannelRollupMaintenanceGuarded(rw)
			}
		}()
```

- [ ] **Step 3: build + vet + full suite** — `cd cmd/server && go build ./... && go vet ./... && go test . -count=1 2>&1 | tail -8` — all green.
- [ ] **Step 4: gofmt + commit**

```bash
git add cmd/server/main.go
git commit -m "feat(channels-rollup): wire schema, backfill, maintenance into startup"
```

---

## Task 7: Frontend — `msgLengths` histogram

**Files:** Modify `public/analytics.js`.

The rollup returns `msgLengths` as `{bins:[{x,w,count}],min,max}` instead of `[]int`. Find where `analytics.js` renders the channels `msgLengths` (grep `msgLengths` in `public/analytics.js`).

- [ ] **Step 1:** `grep -n msgLengths public/analytics.js` — locate the renderer.
- [ ] **Step 2:** The old code treats `msgLengths` as a number array (e.g. builds a histogram client-side, or shows count/avg). Change it to consume the pre-binned `{bins,min,max}` shape: iterate `data.msgLengths.bins` (each `{x,w,count}`) to draw the bars directly, using `min`/`max` for the axis. If the old renderer computed its own bins from the raw array, replace that computation with direct use of `bins`. Keep the same chart/visual.
- [ ] **Step 3:** If `analytics.js` has an existing histogram helper used for RF (`snrValues` etc. are already `{bins,min,max}` shape from the in-memory path), reuse it for `msgLengths` — the shape now matches.
- [ ] **Step 4: commit**

```bash
git add public/analytics.js
git commit -m "feat(channels-rollup): render msgLengths as a pre-binned histogram"
```

> If `msgLengths` is not actually rendered anywhere in `analytics.js` (only computed/ignored), this task is a no-op — confirm with the grep and skip with a note.

---

## Task 8: Parity test

**Files:** Test append to `channel_rollup_test.go`.

- [ ] **Step 1: parity test** — append:

```go
func TestChannelRollupParity(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO observers(rowid,id,name,iata) VALUES
		(1,'o1','O1','SJC'),(2,'o2','O2','LAX')`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json) VALUES
		(1,'aa','h1','2026-05-18T10:00:00Z',5,'{"channel_hash":"7","channel":"#test","sender":"alice","text":"hello"}'),
		(2,'bb','h2','2026-05-18T10:30:00Z',5,'{"channel_hash":"7","channel":"#test","sender":"bob","text":"hey"}'),
		(3,'cc','h3','2026-05-18T11:00:00Z',5,'{"channel_hash":"9","channel":"#ping","sender":"alice","text":"yo"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp) VALUES
		(1,1,1779098400),(2,1,1779100200),(3,2,1779102000)`)
	rw, _ := cachedRW(db.path)
	if err := runChannelRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	store := loadStore(t, db.path, 0)
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}
	for _, region := range []string{"", "SJC"} {
		mem := store.computeAnalyticsChannels(region, win)
		roll, err := computeChannelsFromRollup(db, region, win)
		if err != nil {
			t.Fatalf("[region=%q] rollup: %v", region, err)
		}
		if fmt.Sprint(mem["activeChannels"]) != fmt.Sprint(roll["activeChannels"]) {
			t.Errorf("[region=%q] activeChannels: mem=%v rollup=%v",
				region, mem["activeChannels"], roll["activeChannels"])
		}
		if fmt.Sprint(mem["decryptable"]) != fmt.Sprint(roll["decryptable"]) {
			t.Errorf("[region=%q] decryptable: mem=%v rollup=%v",
				region, mem["decryptable"], roll["decryptable"])
		}
		memTotal := channelMsgTotal(mem)
		rollTotal := channelMsgTotal(roll)
		if memTotal != rollTotal {
			t.Errorf("[region=%q] total messages: mem=%d rollup=%d", region, memTotal, rollTotal)
		}
	}
}

func channelMsgTotal(res map[string]interface{}) int {
	total := 0
	if chans, ok := res["channels"].([]map[string]interface{}); ok {
		for _, c := range chans {
			if m, ok := c["messages"].(int); ok {
				total += m
			}
		}
	}
	return total
}
```

> Parity scope: `activeChannels`, `decryptable`, total messages — exact for global and region. `msgLengths` is intentionally a fixed-bin histogram (not compared to the in-memory raw array). `ensureChannelRollupTable` requires `fmt` in the test file — add it to the import block if missing.

- [ ] **Step 2: run** — `cd cmd/server && go test . -run TestChannelRollupParity -count=1 -v`. If a scalar diverges, the in-memory `computeAnalyticsChannels` is authoritative — fix the rollup code. Do not loosen the test.
- [ ] **Step 3: commit**

```bash
git add cmd/server/channel_rollup_test.go
git commit -m "test(channels-rollup): rollup vs in-memory parity"
```

---

## Task 9: Perf benchmark

**Files:** Test append to `channel_rollup_test.go`.

- [ ] **Step 1:** append:

```go
func TestChannelRollupPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	rw, _ := cachedRW(db.path)
	gen, err := rw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1779000000)
	for i := 1; i <= 200000; i++ {
		ts := base + int64(i)*120 // 200k msgs over ~278 days
		first := time.Unix(ts, 0).UTC().Format(time.RFC3339)
		dj := fmt.Sprintf(`{"channel_hash":"%d","channel":"#c%d","sender":"s%d","text":"msg"}`,
			i%8, i%8, i%500)
		if _, err := gen.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
			VALUES (?,?,?,?,5,?)`, i, "aa", fmt.Sprintf("h%d", i), first, dj); err != nil {
			t.Fatal(err)
		}
		if _, err := gen.Exec(`INSERT INTO observations(transmission_id,observer_idx,timestamp)
			VALUES (?,?,?)`, i, i%50, ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := gen.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := runChannelRollupMaintenance(rw); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	t0 := time.Now()
	if _, err := computeChannelsFromRollup(db, "", TimeWindow{
		Since: "2026-01-01T00:00:00Z", Until: "2027-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("full-history channels query took %s, want < 2s", d)
	}
}
```

- [ ] **Step 2: run** — `cd cmd/server && go test . -run TestChannelRollupPerf -count=1 -v -timeout 300s`. Must pass under 2s for the query.
- [ ] **Step 3: commit**

```bash
git add cmd/server/channel_rollup_test.go
git commit -m "test(channels-rollup): performance benchmark"
```

---

## Task 10: Full build, vet, suite

- [ ] **Step 1:** `cd cmd/server && go build ./...` — clean.
- [ ] **Step 2:** `cd cmd/server && go vet ./...` — clean.
- [ ] **Step 3:** `cd cmd/server && go test . -count=1 2>&1 | tail -12` — all pass.
- [ ] **Step 4:** `cd cmd/server && gofmt -l channel_rollup.go channel_rollup_maintain.go channel_rollup_read.go channel_rollup_test.go store_channels.go main.go` — empty.
- [ ] **Step 5: commit any fixes**

```bash
git add -A cmd/server/
git commit -m "chore(channels-rollup): finalize — build + tests + vet green"
```

---

## Rollout (post-merge, manual)

The `analyticsSqlBackend` flag is already enabled live (RF rollup uses it). Deploying this code → `ensureChannelRollupTable` creates the tables, `backfillChannelRollupAsync` populates them in the background (in-memory fallback serves channels meanwhile), the 5-min ticker keeps them fresh. Watch the deploy logs for `[channel-rollup] backfill complete` and absence of `SQLITE_BUSY` storms.

## Self-review notes

- **Spec coverage:** 3 tables (Task 1), recompute (Task 2), backfill/maintenance/watermark (Task 3), read path incl. exact no-region count via `channel_rollup_tx` + region-approx + topSenders + timeline + msgLengths histogram (Task 4), cache-wrapper wiring with in-memory fallback (Task 5), startup wiring (Task 6), `msgLengths` frontend (Task 7), parity (Task 8), perf (Task 9). Covered.
- **Reuse:** `rfBinIndex`/`rfPackBins`/`rfUnpackBins`/`rfAddBins`/`rfUnpackBins`/`rfHistogramFromBins`/`rfEffectiveWindow`/`rfWindowHourBounds`/`rfIntPlaceholders`/`rfRegionObserverIdxs` — all reused from the RF rollup, not duplicated.
- **Type consistency:** `chChanCell`, `chDecodedGrp`, `chAgg` are package-level / local-consistent. `recomputeChannelRollupHour(rw *sql.DB, hour string) error`, `computeChannelsFromRollup(db *DB, …) (map, error)`, `runChannelRollupMaintenance(rw *sql.DB) error`, `chRollupReady(rw *sql.DB) bool` — signatures consistent across tasks.
- **Concurrency:** recompute reads outside the write tx + indexed `first_seen` range + 50ms yield + guard mutex — the RF SQLITE_BUSY lessons applied from the start.
