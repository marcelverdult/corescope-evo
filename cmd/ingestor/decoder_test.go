package main

import (
	"math"
	"strings"
	"testing"
)

func TestDecodeHeaderRoutTypes(t *testing.T) {
	tests := []struct {
		b    byte
		rt   int
		name string
	}{
		{0x00, 0, "TRANSPORT_FLOOD"},
		{0x01, 1, "FLOOD"},
		{0x02, 2, "DIRECT"},
		{0x03, 3, "TRANSPORT_DIRECT"},
	}
	for _, tt := range tests {
		h := decodeHeader(tt.b)
		if h.RouteType != tt.rt {
			t.Errorf("header 0x%02X: routeType=%d, want %d", tt.b, h.RouteType, tt.rt)
		}
		if h.RouteTypeName != tt.name {
			t.Errorf("header 0x%02X: routeTypeName=%s, want %s", tt.b, h.RouteTypeName, tt.name)
		}
	}
}

func TestDecodeHeaderPayloadTypes(t *testing.T) {
	// 0x11 = 0b00_0100_01 → routeType=1(FLOOD), payloadType=4(ADVERT), version=0
	h := decodeHeader(0x11)
	if h.RouteType != 1 {
		t.Errorf("0x11: routeType=%d, want 1", h.RouteType)
	}
	if h.PayloadType != 4 {
		t.Errorf("0x11: payloadType=%d, want 4", h.PayloadType)
	}
	if h.PayloadVersion != 0 {
		t.Errorf("0x11: payloadVersion=%d, want 0", h.PayloadVersion)
	}
	if h.RouteTypeName != "FLOOD" {
		t.Errorf("0x11: routeTypeName=%s, want FLOOD", h.RouteTypeName)
	}
	if h.PayloadTypeName != "ADVERT" {
		t.Errorf("0x11: payloadTypeName=%s, want ADVERT", h.PayloadTypeName)
	}
}

func TestDecodePathZeroHops(t *testing.T) {
	// 0x00: 0 hops, 1-byte hashes
	pkt, err := DecodePacket("0500" + strings.Repeat("00", 10))
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Path.HashCount != 0 {
		t.Errorf("hashCount=%d, want 0", pkt.Path.HashCount)
	}
	if pkt.Path.HashSize != 1 {
		t.Errorf("hashSize=%d, want 1", pkt.Path.HashSize)
	}
	if len(pkt.Path.Hops) != 0 {
		t.Errorf("hops=%d, want 0", len(pkt.Path.Hops))
	}
}

func TestDecodePath1ByteHashes(t *testing.T) {
	// 0x05: 5 hops, 1-byte hashes → 5 path bytes
	pkt, err := DecodePacket("0505" + "AABBCCDDEE" + strings.Repeat("00", 10))
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Path.HashCount != 5 {
		t.Errorf("hashCount=%d, want 5", pkt.Path.HashCount)
	}
	if pkt.Path.HashSize != 1 {
		t.Errorf("hashSize=%d, want 1", pkt.Path.HashSize)
	}
	if len(pkt.Path.Hops) != 5 {
		t.Fatalf("hops=%d, want 5", len(pkt.Path.Hops))
	}
	if pkt.Path.Hops[0] != "AA" {
		t.Errorf("hop[0]=%s, want AA", pkt.Path.Hops[0])
	}
	if pkt.Path.Hops[4] != "EE" {
		t.Errorf("hop[4]=%s, want EE", pkt.Path.Hops[4])
	}
}

func TestDecodePath2ByteHashes(t *testing.T) {
	// 0x45: 5 hops, 2-byte hashes
	pkt, err := DecodePacket("0545" + "AA11BB22CC33DD44EE55" + strings.Repeat("00", 10))
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Path.HashCount != 5 {
		t.Errorf("hashCount=%d, want 5", pkt.Path.HashCount)
	}
	if pkt.Path.HashSize != 2 {
		t.Errorf("hashSize=%d, want 2", pkt.Path.HashSize)
	}
	if pkt.Path.Hops[0] != "AA11" {
		t.Errorf("hop[0]=%s, want AA11", pkt.Path.Hops[0])
	}
}

func TestDecodePath3ByteHashes(t *testing.T) {
	// 0x8A: 10 hops, 3-byte hashes
	pkt, err := DecodePacket("058A" + strings.Repeat("AA11FF", 10) + strings.Repeat("00", 10))
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Path.HashCount != 10 {
		t.Errorf("hashCount=%d, want 10", pkt.Path.HashCount)
	}
	if pkt.Path.HashSize != 3 {
		t.Errorf("hashSize=%d, want 3", pkt.Path.HashSize)
	}
	if len(pkt.Path.Hops) != 10 {
		t.Errorf("hops=%d, want 10", len(pkt.Path.Hops))
	}
}

func TestTransportCodes(t *testing.T) {
	// Route type 0 (TRANSPORT_FLOOD) should have transport codes
	hex := "1400" + "AABB" + "CCDD" + "1A" + strings.Repeat("00", 10)
	pkt, err := DecodePacket(hex)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Header.RouteType != 0 {
		t.Errorf("routeType=%d, want 0", pkt.Header.RouteType)
	}
	if pkt.TransportCodes == nil {
		t.Fatal("transportCodes should not be nil for TRANSPORT_FLOOD")
	}
	if pkt.TransportCodes.NextHop != "AABB" {
		t.Errorf("nextHop=%s, want AABB", pkt.TransportCodes.NextHop)
	}
	if pkt.TransportCodes.LastHop != "CCDD" {
		t.Errorf("lastHop=%s, want CCDD", pkt.TransportCodes.LastHop)
	}

	// Route type 1 (FLOOD) should NOT have transport codes
	pkt2, err := DecodePacket("0500" + strings.Repeat("00", 10))
	if err != nil {
		t.Fatal(err)
	}
	if pkt2.TransportCodes != nil {
		t.Error("FLOOD should not have transport codes")
	}
}

func TestDecodeAdvertFull(t *testing.T) {
	pubkey := strings.Repeat("AA", 32)
	timestamp := "78563412" // 0x12345678 LE
	signature := strings.Repeat("BB", 64)
	// flags: 0x92 = repeater(2) | hasLocation(0x10) | hasName(0x80)
	flags := "92"
	lat := "40933402" // ~37.0
	lon := "E0E6B8F8" // ~-122.1
	name := "546573744E6F6465" // "TestNode"

	hex := "1200" + pubkey + timestamp + signature + flags + lat + lon + name
	pkt, err := DecodePacket(hex)
	if err != nil {
		t.Fatal(err)
	}

	if pkt.Payload.Type != "ADVERT" {
		t.Errorf("type=%s, want ADVERT", pkt.Payload.Type)
	}
	if pkt.Payload.PubKey != strings.ToLower(pubkey) {
		t.Errorf("pubkey mismatch")
	}
	if pkt.Payload.Timestamp != 0x12345678 {
		t.Errorf("timestamp=%d, want %d", pkt.Payload.Timestamp, 0x12345678)
	}

	if pkt.Payload.Flags == nil {
		t.Fatal("flags should not be nil")
	}
	if pkt.Payload.Flags.Raw != 0x92 {
		t.Errorf("flags.raw=%d, want 0x92", pkt.Payload.Flags.Raw)
	}
	if pkt.Payload.Flags.Type != 2 {
		t.Errorf("flags.type=%d, want 2", pkt.Payload.Flags.Type)
	}
	if !pkt.Payload.Flags.Repeater {
		t.Error("flags.repeater should be true")
	}
	if pkt.Payload.Flags.Room {
		t.Error("flags.room should be false")
	}
	if !pkt.Payload.Flags.HasLocation {
		t.Error("flags.hasLocation should be true")
	}
	if !pkt.Payload.Flags.HasName {
		t.Error("flags.hasName should be true")
	}

	if pkt.Payload.Lat == nil {
		t.Fatal("lat should not be nil")
	}
	if math.Abs(*pkt.Payload.Lat-37.0) > 0.001 {
		t.Errorf("lat=%f, want ~37.0", *pkt.Payload.Lat)
	}
	if pkt.Payload.Lon == nil {
		t.Fatal("lon should not be nil")
	}
	if math.Abs(*pkt.Payload.Lon-(-122.1)) > 0.001 {
		t.Errorf("lon=%f, want ~-122.1", *pkt.Payload.Lon)
	}
	if pkt.Payload.Name != "TestNode" {
		t.Errorf("name=%s, want TestNode", pkt.Payload.Name)
	}
}

func TestDecodeAdvertTypeEnums(t *testing.T) {
	makeAdvert := func(flagsByte byte) *DecodedPacket {
		hex := "1200" + strings.Repeat("AA", 32) + "00000000" + strings.Repeat("BB", 64) +
			strings.ToUpper(string([]byte{hexDigit(flagsByte>>4), hexDigit(flagsByte & 0x0f)}))
		pkt, err := DecodePacket(hex)
		if err != nil {
			t.Fatal(err)
		}
		return pkt
	}

	// type 1 = chat/companion
	p1 := makeAdvert(0x01)
	if p1.Payload.Flags.Type != 1 {
		t.Errorf("type 1: flags.type=%d", p1.Payload.Flags.Type)
	}
	if !p1.Payload.Flags.Chat {
		t.Error("type 1: chat should be true")
	}

	// type 2 = repeater
	p2 := makeAdvert(0x02)
	if !p2.Payload.Flags.Repeater {
		t.Error("type 2: repeater should be true")
	}

	// type 3 = room
	p3 := makeAdvert(0x03)
	if !p3.Payload.Flags.Room {
		t.Error("type 3: room should be true")
	}

	// type 4 = sensor
	p4 := makeAdvert(0x04)
	if !p4.Payload.Flags.Sensor {
		t.Error("type 4: sensor should be true")
	}
}

func hexDigit(v byte) byte {
	v = v & 0x0f
	if v < 10 {
		return '0' + v
	}
	return 'a' + v - 10
}

func TestDecodeAdvertNoLocationNoName(t *testing.T) {
	hex := "1200" + strings.Repeat("CC", 32) + "00000000" + strings.Repeat("DD", 64) + "02"
	pkt, err := DecodePacket(hex)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Payload.Flags.HasLocation {
		t.Error("hasLocation should be false")
	}
	if pkt.Payload.Flags.HasName {
		t.Error("hasName should be false")
	}
	if pkt.Payload.Lat != nil {
		t.Error("lat should be nil")
	}
	if pkt.Payload.Name != "" {
		t.Errorf("name should be empty, got %s", pkt.Payload.Name)
	}
}

func TestGoldenFixtureTxtMsg(t *testing.T) {
	pkt, err := DecodePacket("0A00D69FD7A5A7475DB07337749AE61FA53A4788E976")
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Header.PayloadType != PayloadTXT_MSG {
		t.Errorf("payloadType=%d, want %d", pkt.Header.PayloadType, PayloadTXT_MSG)
	}
	if pkt.Header.RouteType != RouteDirect {
		t.Errorf("routeType=%d, want %d", pkt.Header.RouteType, RouteDirect)
	}
	if pkt.Path.HashCount != 0 {
		t.Errorf("hashCount=%d, want 0", pkt.Path.HashCount)
	}
	if pkt.Payload.DestHash != "d6" {
		t.Errorf("destHash=%s, want d6", pkt.Payload.DestHash)
	}
	if pkt.Payload.SrcHash != "9f" {
		t.Errorf("srcHash=%s, want 9f", pkt.Payload.SrcHash)
	}
}

func TestGoldenFixtureAdvert(t *testing.T) {
	rawHex := "120046D62DE27D4C5194D7821FC5A34A45565DCC2537B300B9AB6275255CEFB65D840CE5C169C94C9AED39E8BCB6CB6EB0335497A198B33A1A610CD3B03D8DCFC160900E5244280323EE0B44CACAB8F02B5B38B91CFA18BD067B0B5E63E94CFC85F758A8530B9240933402E0E6B8F84D5252322D52"
	pkt, err := DecodePacket(rawHex)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Payload.Type != "ADVERT" {
		t.Errorf("type=%s, want ADVERT", pkt.Payload.Type)
	}
	if pkt.Payload.PubKey != "46d62de27d4c5194d7821fc5a34a45565dcc2537b300b9ab6275255cefb65d84" {
		t.Errorf("pubKey mismatch: %s", pkt.Payload.PubKey)
	}
	if pkt.Payload.Flags == nil || !pkt.Payload.Flags.Repeater {
		t.Error("should be repeater")
	}
	if math.Abs(*pkt.Payload.Lat-37.0) > 0.001 {
		t.Errorf("lat=%f, want ~37.0", *pkt.Payload.Lat)
	}
	if pkt.Payload.Name != "MRR2-R" {
		t.Errorf("name=%s, want MRR2-R", pkt.Payload.Name)
	}
}

func TestGoldenFixtureUnicodeAdvert(t *testing.T) {
	rawHex := "120073CFF971E1CB5754A742C152B2D2E0EB108A19B246D663ED8898A72C4A5AD86EA6768E66694B025EDF6939D5C44CFF719C5D5520E5F06B20680A83AD9C2C61C3227BBB977A85EE462F3553445FECF8EDD05C234ECE217272E503F14D6DF2B1B9B133890C923CDF3002F8FDC1F85045414BF09F8CB3"
	pkt, err := DecodePacket(rawHex)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Payload.Type != "ADVERT" {
		t.Errorf("type=%s, want ADVERT", pkt.Payload.Type)
	}
	if !pkt.Payload.Flags.Repeater {
		t.Error("should be repeater")
	}
	// Name contains emoji: PEAK🌳
	if !strings.HasPrefix(pkt.Payload.Name, "PEAK") {
		t.Errorf("name=%s, expected to start with PEAK", pkt.Payload.Name)
	}
}

func TestDecodePacketTooShort(t *testing.T) {
	_, err := DecodePacket("FF")
	if err == nil {
		t.Error("expected error for 1-byte packet")
	}
}

func TestDecodePacketInvalidHex(t *testing.T) {
	_, err := DecodePacket("ZZZZ")
	if err == nil {
		t.Error("expected error for invalid hex")
	}
}

func TestComputeContentHash(t *testing.T) {
	hash := ComputeContentHash("0A00D69FD7A5A7475DB07337749AE61FA53A4788E976")
	if len(hash) != 16 {
		t.Errorf("hash length=%d, want 16", len(hash))
	}
	// Same content with different path should produce same hash
	// (path bytes are stripped, only header + payload hashed)

	// Verify consistency
	hash2 := ComputeContentHash("0A00D69FD7A5A7475DB07337749AE61FA53A4788E976")
	if hash != hash2 {
		t.Error("content hash not deterministic")
	}
}

func TestValidateAdvert(t *testing.T) {
	goodPk := strings.Repeat("aa", 32)

	// Good advert
	good := &Payload{PubKey: goodPk, Flags: &AdvertFlags{Repeater: true}}
	ok, _ := ValidateAdvert(good)
	if !ok {
		t.Error("good advert should validate")
	}

	// Nil
	ok, _ = ValidateAdvert(nil)
	if ok {
		t.Error("nil should fail")
	}

	// Error payload
	ok, _ = ValidateAdvert(&Payload{Error: "bad"})
	if ok {
		t.Error("error payload should fail")
	}

	// Short pubkey
	ok, _ = ValidateAdvert(&Payload{PubKey: "aa"})
	if ok {
		t.Error("short pubkey should fail")
	}

	// All-zero pubkey
	ok, _ = ValidateAdvert(&Payload{PubKey: strings.Repeat("0", 64)})
	if ok {
		t.Error("all-zero pubkey should fail")
	}

	// Invalid lat
	badLat := 999.0
	ok, _ = ValidateAdvert(&Payload{PubKey: goodPk, Lat: &badLat})
	if ok {
		t.Error("invalid lat should fail")
	}

	// Invalid lon
	badLon := -999.0
	ok, _ = ValidateAdvert(&Payload{PubKey: goodPk, Lon: &badLon})
	if ok {
		t.Error("invalid lon should fail")
	}

	// Control chars in name
	ok, _ = ValidateAdvert(&Payload{PubKey: goodPk, Name: "test\x00name"})
	if ok {
		t.Error("control chars in name should fail")
	}

	// Name too long
	ok, _ = ValidateAdvert(&Payload{PubKey: goodPk, Name: strings.Repeat("x", 65)})
	if ok {
		t.Error("long name should fail")
	}
}

func TestDecodeFloodAdvert5Hops(t *testing.T) {
	// From test-decoder.js Test 1
	raw := "11451000D818206D3AAC152C8A91F89957E6D30CA51F36E28790228971C473B755F244F718754CF5EE4A2FD58D944466E42CDED140C66D0CC590183E32BAF40F112BE8F3F2BDF6012B4B2793C52F1D36F69EE054D9A05593286F78453E56C0EC4A3EB95DDA2A7543FCCC00B939CACC009278603902FC12BCF84B706120526F6F6620536F6C6172"
	pkt, err := DecodePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Header.RouteTypeName != "FLOOD" {
		t.Errorf("route=%s, want FLOOD", pkt.Header.RouteTypeName)
	}
	if pkt.Header.PayloadTypeName != "ADVERT" {
		t.Errorf("payload=%s, want ADVERT", pkt.Header.PayloadTypeName)
	}
	if pkt.Path.HashSize != 2 {
		t.Errorf("hashSize=%d, want 2", pkt.Path.HashSize)
	}
	if pkt.Path.HashCount != 5 {
		t.Errorf("hashCount=%d, want 5", pkt.Path.HashCount)
	}
	if pkt.Path.Hops[0] != "1000" {
		t.Errorf("hop[0]=%s, want 1000", pkt.Path.Hops[0])
	}
	if pkt.Path.Hops[1] != "D818" {
		t.Errorf("hop[1]=%s, want D818", pkt.Path.Hops[1])
	}
	if pkt.TransportCodes != nil {
		t.Error("FLOOD should have no transport codes")
	}
}
