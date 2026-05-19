package main

import "testing"

func TestAnalyticsSQLBackendFlagWiring(t *testing.T) {
	db := setupTestDB(t)
	cfg := &PacketStoreConfig{AnalyticsSQLBackend: true}
	ps := NewPacketStore(db, cfg)
	if !ps.analyticsSQLBackend {
		t.Fatal("expected analyticsSQLBackend true when config sets it")
	}
}
