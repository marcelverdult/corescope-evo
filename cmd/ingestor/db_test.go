package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(".", "testdata")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, t.Name()+".db")
	// Clean up any previous test DB
	os.Remove(p)
	os.Remove(p + "-wal")
	os.Remove(p + "-shm")
	t.Cleanup(func() {
		os.Remove(p)
		os.Remove(p + "-wal")
		os.Remove(p + "-shm")
	})
	return p
}

func TestOpenStore(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Verify tables exist
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}

	expected := []string{"nodes", "observations", "observers", "transmissions"}
	for _, e := range expected {
		found := false
		for _, tbl := range tables {
			if tbl == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing table %s, got %v", e, tables)
		}
	}
}

func TestInsertTransmission(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	snr := 5.5
	rssi := -100.0
	data := &PacketData{
		RawHex:         "0A00D69FD7A5A7475DB07337749AE61FA53A4788E976",
		Timestamp:      "2026-03-25T00:00:00Z",
		ObserverID:     "obs1",
		Hash:           "abcdef1234567890",
		RouteType:      2,
		PayloadType:    2,
		PayloadVersion: 0,
		PathJSON:       "[]",
		DecodedJSON:    `{"type":"TXT_MSG"}`,
		SNR:            &snr,
		RSSI:           &rssi,
	}

	if err := s.InsertTransmission(data); err != nil {
		t.Fatal(err)
	}

	// Verify transmission was inserted
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM transmissions").Scan(&count)
	if count != 1 {
		t.Errorf("transmissions count=%d, want 1", count)
	}

	// Verify observation was inserted
	s.db.QueryRow("SELECT COUNT(*) FROM observations").Scan(&count)
	if count != 1 {
		t.Errorf("observations count=%d, want 1", count)
	}

	// Verify hash dedup: same hash should not create new transmission
	if err := s.InsertTransmission(data); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow("SELECT COUNT(*) FROM transmissions").Scan(&count)
	if count != 1 {
		t.Errorf("transmissions count after dedup=%d, want 1", count)
	}
}

func TestUpsertNode(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	lat := 37.0
	lon := -122.0
	if err := s.UpsertNode("aabbccdd", "TestNode", "repeater", &lat, &lon, "2026-03-25T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	var name, role string
	s.db.QueryRow("SELECT name, role FROM nodes WHERE public_key = 'aabbccdd'").Scan(&name, &role)
	if name != "TestNode" {
		t.Errorf("name=%s, want TestNode", name)
	}
	if role != "repeater" {
		t.Errorf("role=%s, want repeater", role)
	}

	// Upsert again — should update
	if err := s.UpsertNode("aabbccdd", "UpdatedNode", "repeater", &lat, &lon, "2026-03-25T01:00:00Z"); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow("SELECT name FROM nodes WHERE public_key = 'aabbccdd'").Scan(&name)
	if name != "UpdatedNode" {
		t.Errorf("after upsert name=%s, want UpdatedNode", name)
	}

	// Verify advert_count incremented
	var count int
	s.db.QueryRow("SELECT advert_count FROM nodes WHERE public_key = 'aabbccdd'").Scan(&count)
	if count != 2 {
		t.Errorf("advert_count=%d, want 2", count)
	}
}

func TestUpsertObserver(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertObserver("obs1", "Observer1", "SJC"); err != nil {
		t.Fatal(err)
	}

	var name, iata string
	s.db.QueryRow("SELECT name, iata FROM observers WHERE id = 'obs1'").Scan(&name, &iata)
	if name != "Observer1" {
		t.Errorf("name=%s, want Observer1", name)
	}
	if iata != "SJC" {
		t.Errorf("iata=%s, want SJC", iata)
	}
}

func TestInsertTransmissionWithObserver(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Insert observer first
	if err := s.UpsertObserver("obs1", "Observer1", "SJC"); err != nil {
		t.Fatal(err)
	}

	data := &PacketData{
		RawHex:      "0A00D69F",
		Timestamp:   "2026-03-25T00:00:00Z",
		ObserverID:  "obs1",
		Hash:        "test1234567890ab",
		RouteType:   2,
		PayloadType: 2,
		PathJSON:    "[]",
		DecodedJSON: `{"type":"TXT_MSG"}`,
	}
	if err := s.InsertTransmission(data); err != nil {
		t.Fatal(err)
	}

	// Verify observer_idx was resolved
	var observerIdx *int64
	s.db.QueryRow("SELECT observer_idx FROM observations LIMIT 1").Scan(&observerIdx)
	if observerIdx == nil {
		t.Error("observer_idx should be set when observer exists")
	}
}

func TestEndToEndIngest(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Simulate full pipeline: decode + insert
	rawHex := "120046D62DE27D4C5194D7821FC5A34A45565DCC2537B300B9AB6275255CEFB65D840CE5C169C94C9AED39E8BCB6CB6EB0335497A198B33A1A610CD3B03D8DCFC160900E5244280323EE0B44CACAB8F02B5B38B91CFA18BD067B0B5E63E94CFC85F758A8530B9240933402E0E6B8F84D5252322D52"

	decoded, err := DecodePacket(rawHex)
	if err != nil {
		t.Fatal(err)
	}

	msg := &MQTTPacketMessage{
		Raw: rawHex,
	}
	pktData := BuildPacketData(msg, decoded, "obs1", "SJC")
	if err := s.InsertTransmission(pktData); err != nil {
		t.Fatal(err)
	}

	// Process advert node upsert
	if decoded.Payload.Type == "ADVERT" && decoded.Payload.PubKey != "" {
		ok, _ := ValidateAdvert(&decoded.Payload)
		if ok {
			role := advertRole(decoded.Payload.Flags)
			err := s.UpsertNode(decoded.Payload.PubKey, decoded.Payload.Name, role, decoded.Payload.Lat, decoded.Payload.Lon, pktData.Timestamp)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	// Verify node was created
	var nodeName string
	err = s.db.QueryRow("SELECT name FROM nodes WHERE public_key = ?", decoded.Payload.PubKey).Scan(&nodeName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nodeName, "MRR2-R") {
		t.Errorf("node name=%s, want MRR2-R", nodeName)
	}
}

func TestSchemaCompatibility(t *testing.T) {
	s, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Verify column names match what Node.js expects
	expectedTxCols := []string{"id", "raw_hex", "hash", "first_seen", "route_type", "payload_type", "payload_version", "decoded_json", "created_at"}
	rows, _ := s.db.Query("PRAGMA table_info(transmissions)")
	var txCols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt *string
		var pk int
		rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		txCols = append(txCols, name)
	}
	rows.Close()

	for _, e := range expectedTxCols {
		found := false
		for _, c := range txCols {
			if c == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("transmissions missing column %s, got %v", e, txCols)
		}
	}

	// Verify observations columns
	expectedObsCols := []string{"id", "transmission_id", "observer_idx", "direction", "snr", "rssi", "score", "path_json", "timestamp"}
	rows, _ = s.db.Query("PRAGMA table_info(observations)")
	var obsCols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt *string
		var pk int
		rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		obsCols = append(obsCols, name)
	}
	rows.Close()

	for _, e := range expectedObsCols {
		found := false
		for _, c := range obsCols {
			if c == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("observations missing column %s, got %v", e, obsCols)
		}
	}
}
