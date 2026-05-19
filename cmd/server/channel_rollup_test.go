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

func TestRecomputeChannelRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureChannelRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
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
