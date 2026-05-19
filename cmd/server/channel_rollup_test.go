package main

import (
	"fmt"
	"testing"
)

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
		if channelMsgTotal(mem) != channelMsgTotal(roll) {
			t.Errorf("[region=%q] total messages: mem=%d rollup=%d",
				region, channelMsgTotal(mem), channelMsgTotal(roll))
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
