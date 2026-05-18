package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/meshcore-analyzer/meshdecode"
	"github.com/meshcore-analyzer/sigvalidate"
)

// The route-type/payload-type constants, the Header/Path/AdvertFlags/
// TransportCodes structs, and the path/hash/sanitize helpers are shared with
// the ingestor via the meshdecode module — they decode identically in both
// binaries. They are re-exported here as aliases/consts so the rest of the
// server package (and its tests) keep using the original local names.
//
// The Payload struct and everything that constructs it (the per-payload
// decoders, decodePayload, DecodePacket, PayloadJSON, ValidateAdvert) stay
// local: the server and ingestor Payload structs have diverged. decodeHeader
// also stays local because it resolves payload-type names through the
// server-local payloadTypeNames map (see store.go).

// Route type constants (header bits 1-0).
const (
	RouteTransportFlood  = meshdecode.RouteTransportFlood
	RouteFlood           = meshdecode.RouteFlood
	RouteDirect          = meshdecode.RouteDirect
	RouteTransportDirect = meshdecode.RouteTransportDirect
)

// Payload type constants (header bits 5-2).
const (
	PayloadREQ        = meshdecode.PayloadREQ
	PayloadRESPONSE   = meshdecode.PayloadRESPONSE
	PayloadTXT_MSG    = meshdecode.PayloadTXT_MSG
	PayloadACK        = meshdecode.PayloadACK
	PayloadADVERT     = meshdecode.PayloadADVERT
	PayloadGRP_TXT    = meshdecode.PayloadGRP_TXT
	PayloadGRP_DATA   = meshdecode.PayloadGRP_DATA
	PayloadANON_REQ   = meshdecode.PayloadANON_REQ
	PayloadPATH       = meshdecode.PayloadPATH
	PayloadTRACE      = meshdecode.PayloadTRACE
	PayloadMULTIPART  = meshdecode.PayloadMULTIPART
	PayloadCONTROL    = meshdecode.PayloadCONTROL
	PayloadRAW_CUSTOM = meshdecode.PayloadRAW_CUSTOM
)

// Firmware-derived limits — see firmware/src/MeshCore.h:19,21.
const (
	maxPathSize      = meshdecode.MaxPathSize      // MAX_PATH_SIZE — total path bytes allowed
	maxPacketPayload = meshdecode.MaxPacketPayload // MAX_PACKET_PAYLOAD — max raw payload bytes
)

var routeTypeNames = meshdecode.RouteTypeNames

// Shared decoded-packet structs (field-identical in server and ingestor).
type (
	Header         = meshdecode.Header
	TransportCodes = meshdecode.TransportCodes
	Path           = meshdecode.Path
	AdvertFlags    = meshdecode.AdvertFlags
)

// isValidPathLen mirrors firmware Packet::isValidPathLen.
func isValidPathLen(pathByte byte) bool { return meshdecode.IsValidPathLen(pathByte) }

// decodePath decodes the path/hop bytes that follow the path byte.
func decodePath(pathByte byte, buf []byte, offset int) (Path, int, error) {
	return meshdecode.DecodePath(pathByte, buf, offset)
}

// isTransportRoute delegates to packetpath.IsTransportRoute via meshdecode.
func isTransportRoute(routeType int) bool { return meshdecode.IsTransportRoute(routeType) }

// ComputeContentHash computes the SHA-256-based content hash (first 16 hex chars).
func ComputeContentHash(rawHex string) string { return meshdecode.ComputeContentHash(rawHex) }

// sanitizeName strips non-printable characters (< 0x20 except tab/newline) and DEL.
func sanitizeName(s string) string { return meshdecode.SanitizeName(s) }

// advertRole returns the node role implied by the advert flags.
func advertRole(f *AdvertFlags) string { return meshdecode.AdvertRole(f) }

// epochToISO formats a Unix epoch (seconds) as an ISO-8601 UTC timestamp.
func epochToISO(epoch uint32) string { return meshdecode.EpochToISO(epoch) }

// Payload is a generic decoded payload. Fields are populated depending on type.
type Payload struct {
	Type            string       `json:"type"`
	DestHash        string       `json:"destHash,omitempty"`
	SrcHash         string       `json:"srcHash,omitempty"`
	MAC             string       `json:"mac,omitempty"`
	EncryptedData   string       `json:"encryptedData,omitempty"`
	ExtraHash       string       `json:"extraHash,omitempty"`
	PubKey          string       `json:"pubKey,omitempty"`
	Timestamp       uint32       `json:"timestamp,omitempty"`
	TimestampISO    string       `json:"timestampISO,omitempty"`
	Signature       string       `json:"signature,omitempty"`
	SignatureValid  *bool        `json:"signatureValid,omitempty"`
	Flags           *AdvertFlags `json:"flags,omitempty"`
	Lat             *float64     `json:"lat,omitempty"`
	Lon             *float64     `json:"lon,omitempty"`
	Name            string       `json:"name,omitempty"`
	ChannelHash     int          `json:"channelHash,omitempty"`
	EphemeralPubKey string       `json:"ephemeralPubKey,omitempty"`
	PathData        string       `json:"pathData,omitempty"`
	Tag             uint32       `json:"tag,omitempty"`
	AuthCode        uint32       `json:"authCode,omitempty"`
	TraceFlags      *int         `json:"traceFlags,omitempty"`
	SNRValues       []float64    `json:"snrValues,omitempty"`
	RawHex          string       `json:"raw,omitempty"`
	Error           string       `json:"error,omitempty"`
}

// DecodedPacket is the full decoded result.
type DecodedPacket struct {
	Header         Header          `json:"header"`
	TransportCodes *TransportCodes `json:"transportCodes"`
	Path           Path            `json:"path"`
	Payload        Payload         `json:"payload"`
	Raw            string          `json:"raw"`
	Anomaly        string          `json:"anomaly,omitempty"`
}

func decodeHeader(b byte) Header {
	rt := int(b & 0x03)
	pt := int((b >> 2) & 0x0F)
	pv := int((b >> 6) & 0x03)

	rtName := routeTypeNames[rt]
	if rtName == "" {
		rtName = "UNKNOWN"
	}
	ptName := payloadTypeNames[pt]
	if ptName == "" {
		ptName = "UNKNOWN"
	}

	return Header{
		RouteType:       rt,
		RouteTypeName:   rtName,
		PayloadType:     pt,
		PayloadTypeName: ptName,
		PayloadVersion:  pv,
	}
}

func decodeEncryptedPayload(typeName string, buf []byte) Payload {
	if len(buf) < 4 {
		return Payload{Type: typeName, Error: "too short", RawHex: hex.EncodeToString(buf)}
	}
	return Payload{
		Type:          typeName,
		DestHash:      hex.EncodeToString(buf[0:1]),
		SrcHash:       hex.EncodeToString(buf[1:2]),
		MAC:           hex.EncodeToString(buf[2:4]),
		EncryptedData: hex.EncodeToString(buf[4:]),
	}
}

func decodeAck(buf []byte) Payload {
	if len(buf) < 4 {
		return Payload{Type: "ACK", Error: "too short", RawHex: hex.EncodeToString(buf)}
	}
	checksum := binary.LittleEndian.Uint32(buf[0:4])
	return Payload{
		Type:      "ACK",
		ExtraHash: fmt.Sprintf("%08x", checksum),
	}
}

func decodeAdvert(buf []byte, validateSignatures bool) Payload {
	if len(buf) < 100 {
		return Payload{Type: "ADVERT", Error: "too short for advert", RawHex: hex.EncodeToString(buf)}
	}

	pubKey := hex.EncodeToString(buf[0:32])
	timestamp := binary.LittleEndian.Uint32(buf[32:36])
	signature := hex.EncodeToString(buf[36:100])
	appdata := buf[100:]

	p := Payload{
		Type:         "ADVERT",
		PubKey:       pubKey,
		Timestamp:    timestamp,
		TimestampISO: fmt.Sprintf("%s", epochToISO(timestamp)),
		Signature:    signature,
	}

	if validateSignatures {
		valid, err := sigvalidate.ValidateAdvert(buf[0:32], buf[36:100], timestamp, appdata)
		if err != nil {
			f := false
			p.SignatureValid = &f
		} else {
			p.SignatureValid = &valid
		}
	}

	if len(appdata) > 0 {
		flags := appdata[0]
		advType := int(flags & 0x0F)
		hasFeat1 := flags&0x20 != 0
		hasFeat2 := flags&0x40 != 0
		p.Flags = &AdvertFlags{
			Raw:         int(flags),
			Type:        advType,
			Chat:        advType == 1,
			Repeater:    advType == 2,
			Room:        advType == 3,
			Sensor:      advType == 4,
			HasLocation: flags&0x10 != 0,
			HasFeat1:    hasFeat1,
			HasFeat2:    hasFeat2,
			HasName:     flags&0x80 != 0,
		}

		off := 1
		if p.Flags.HasLocation && len(appdata) >= off+8 {
			latRaw := int32(binary.LittleEndian.Uint32(appdata[off : off+4]))
			lonRaw := int32(binary.LittleEndian.Uint32(appdata[off+4 : off+8]))
			lat := float64(latRaw) / 1e6
			lon := float64(lonRaw) / 1e6
			p.Lat = &lat
			p.Lon = &lon
			off += 8
		}
		if hasFeat1 && len(appdata) >= off+2 {
			off += 2 // skip feat1 bytes (reserved for future use)
		}
		if hasFeat2 && len(appdata) >= off+2 {
			off += 2 // skip feat2 bytes (reserved for future use)
		}
		if p.Flags.HasName {
			name := string(appdata[off:])
			name = strings.TrimRight(name, "\x00")
			name = sanitizeName(name)
			// Firmware writes the node name into a 32-byte buffer
			// (MAX_ADVERT_DATA_SIZE, firmware/src/MeshCore.h:11). Truncate
			// here so adversarial on-wire adverts can't pollute Payload.Name
			// with bytes firmware would never emit.
			if len(name) > 32 {
				name = name[:32]
			}
			p.Name = name
		}
	}

	return p
}

func decodeGrpTxt(buf []byte) Payload {
	if len(buf) < 3 {
		return Payload{Type: "GRP_TXT", Error: "too short", RawHex: hex.EncodeToString(buf)}
	}
	return Payload{
		Type:          "GRP_TXT",
		ChannelHash:   int(buf[0]),
		MAC:           hex.EncodeToString(buf[1:3]),
		EncryptedData: hex.EncodeToString(buf[3:]),
	}
}

func decodeAnonReq(buf []byte) Payload {
	if len(buf) < 35 {
		return Payload{Type: "ANON_REQ", Error: "too short", RawHex: hex.EncodeToString(buf)}
	}
	return Payload{
		Type:            "ANON_REQ",
		DestHash:        hex.EncodeToString(buf[0:1]),
		EphemeralPubKey: hex.EncodeToString(buf[1:33]),
		MAC:             hex.EncodeToString(buf[33:35]),
		EncryptedData:   hex.EncodeToString(buf[35:]),
	}
}

func decodePathPayload(buf []byte) Payload {
	if len(buf) < 4 {
		return Payload{Type: "PATH", Error: "too short", RawHex: hex.EncodeToString(buf)}
	}
	return Payload{
		Type:     "PATH",
		DestHash: hex.EncodeToString(buf[0:1]),
		SrcHash:  hex.EncodeToString(buf[1:2]),
		MAC:      hex.EncodeToString(buf[2:4]),
		PathData: hex.EncodeToString(buf[4:]),
	}
}

func decodeTrace(buf []byte) Payload {
	if len(buf) < 9 {
		return Payload{Type: "TRACE", Error: "too short", RawHex: hex.EncodeToString(buf)}
	}
	tag := binary.LittleEndian.Uint32(buf[0:4])
	authCode := binary.LittleEndian.Uint32(buf[4:8])
	flags := int(buf[8])
	p := Payload{
		Type:       "TRACE",
		Tag:        tag,
		AuthCode:   authCode,
		TraceFlags: &flags,
	}
	if len(buf) > 9 {
		p.PathData = hex.EncodeToString(buf[9:])
	}
	return p
}

func decodePayload(payloadType int, buf []byte, validateSignatures bool) Payload {
	switch payloadType {
	case PayloadREQ:
		return decodeEncryptedPayload("REQ", buf)
	case PayloadRESPONSE:
		return decodeEncryptedPayload("RESPONSE", buf)
	case PayloadTXT_MSG:
		return decodeEncryptedPayload("TXT_MSG", buf)
	case PayloadACK:
		return decodeAck(buf)
	case PayloadADVERT:
		return decodeAdvert(buf, validateSignatures)
	case PayloadGRP_TXT:
		return decodeGrpTxt(buf)
	case PayloadANON_REQ:
		return decodeAnonReq(buf)
	case PayloadPATH:
		return decodePathPayload(buf)
	case PayloadTRACE:
		return decodeTrace(buf)
	default:
		return Payload{Type: "UNKNOWN", RawHex: hex.EncodeToString(buf)}
	}
}

// maxDecodeHexLen caps the hex-string length DecodePacket will accept before
// allocating via hex.DecodeString. A MeshCore packet is at most a few hundred
// bytes; 16 KB of hex (8 KB decoded) is a generous ceiling that bounds memory
// use even if a caller bypasses the HTTP-layer body limit.
const maxDecodeHexLen = 16 << 10 // 16 KB

// DecodePacket decodes a hex-encoded MeshCore packet.
func DecodePacket(hexString string, validateSignatures bool) (*DecodedPacket, error) {
	hexString = strings.ReplaceAll(hexString, " ", "")
	hexString = strings.ReplaceAll(hexString, "\n", "")
	hexString = strings.ReplaceAll(hexString, "\r", "")

	if len(hexString) > maxDecodeHexLen {
		return nil, fmt.Errorf("hex input too large (max %d chars)", maxDecodeHexLen)
	}

	buf, err := hex.DecodeString(hexString)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %w", err)
	}
	if len(buf) < 2 {
		return nil, fmt.Errorf("packet too short (need at least header + pathLength)")
	}

	header := decodeHeader(buf[0])
	offset := 1

	var tc *TransportCodes
	if isTransportRoute(header.RouteType) {
		if len(buf) < offset+4 {
			return nil, fmt.Errorf("packet too short for transport codes")
		}
		tc = &TransportCodes{
			Code1: strings.ToUpper(hex.EncodeToString(buf[offset : offset+2])),
			Code2: strings.ToUpper(hex.EncodeToString(buf[offset+2 : offset+4])),
		}
		offset += 4
	}

	if offset >= len(buf) {
		return nil, fmt.Errorf("packet too short (no path byte)")
	}
	pathByte := buf[offset]
	offset++

	path, bytesConsumed, decodeErr := decodePath(pathByte, buf, offset)
	if decodeErr != nil {
		return nil, decodeErr
	}
	offset += bytesConsumed

	// Bounds check — see cmd/ingestor/decoder.go for full rationale (#1211).
	if offset > len(buf) {
		return nil, fmt.Errorf("packet path length (%d bytes claimed by pathByte 0x%02X) exceeds buffer (%d bytes)", bytesConsumed, pathByte, len(buf))
	}

	payloadBuf := buf[offset:]
	// Firmware caps payload at MAX_PACKET_PAYLOAD=184 (firmware/src/MeshCore.h:19).
	// Anything larger cannot be a valid wire packet — drop it.
	if len(payloadBuf) > maxPacketPayload {
		return nil, fmt.Errorf("packet payload (%d bytes) exceeds firmware MAX_PACKET_PAYLOAD=%d (MeshCore.h:19)", len(payloadBuf), maxPacketPayload)
	}
	payload := decodePayload(header.PayloadType, payloadBuf, validateSignatures)

	// TRACE packets store hop IDs in the payload (buf[9:]) rather than the header
	// path field. Firmware always sends TRACE as DIRECT (route_type 2 or 3);
	// FLOOD-routed TRACEs are anomalous but handled gracefully (parsed, but
	// flagged). The TRACE flags byte (payload offset 8) encodes path_sz in
	// bits 0-1 as a power-of-two exponent: hash_bytes = 1 << path_sz.
	// NOT the header path byte's hash_size bits. The header path contains SNR
	// bytes — one per hop that actually forwarded.
	// We expose hopsCompleted (count of SNR bytes) so consumers can distinguish
	// how far the trace got vs the full intended route.
	var anomaly string
	if header.PayloadType == PayloadTRACE && payload.PathData != "" {
		// Flag anomalous routing — firmware only sends TRACE as DIRECT
		if header.RouteType != RouteDirect && header.RouteType != RouteTransportDirect {
			anomaly = "TRACE packet with non-DIRECT routing (expected DIRECT or TRANSPORT_DIRECT)"
		}
		// The header path hops count represents SNR entries = completed hops
		hopsCompleted := path.HashCount
		// Extract per-hop SNR from header path bytes (int8, quarter-dB encoding)
		if hopsCompleted > 0 && len(path.Hops) >= hopsCompleted {
			snrVals := make([]float64, 0, hopsCompleted)
			for i := 0; i < hopsCompleted; i++ {
				b, err := hex.DecodeString(path.Hops[i])
				if err == nil && len(b) == 1 {
					snrVals = append(snrVals, float64(int8(b[0]))/4.0)
				}
			}
			if len(snrVals) > 0 {
				payload.SNRValues = snrVals
			}
		}
		pathBytes, err := hex.DecodeString(payload.PathData)
		if err == nil && payload.TraceFlags != nil {
			// path_sz from flags byte is a power-of-two exponent per firmware:
			// hash_bytes = 1 << (flags & 0x03)
			pathSz := 1 << (*payload.TraceFlags & 0x03)
			hops := make([]string, 0, len(pathBytes)/pathSz)
			for i := 0; i+pathSz <= len(pathBytes); i += pathSz {
				hops = append(hops, strings.ToUpper(hex.EncodeToString(pathBytes[i:i+pathSz])))
			}
			path.Hops = hops
			path.HashCount = len(hops)
			path.HashSize = pathSz
			path.HopsCompleted = &hopsCompleted
		}
	}

	// Zero-hop direct packets have hash_count=0 (lower 6 bits of pathByte),
	// which makes the generic formula yield a bogus hashSize. Reset to 0
	// (unknown) so API consumers get correct data. We mask with 0x3F to check
	// only hash_count, matching the JS frontend approach — the upper hash_size
	// bits are meaningless when there are no hops. Skip TRACE packets — they
	// use hashSize to parse hops from the payload above.
	if (header.RouteType == RouteDirect || header.RouteType == RouteTransportDirect) && pathByte&0x3F == 0 && header.PayloadType != PayloadTRACE {
		path.HashSize = 0
	}

	return &DecodedPacket{
		Header:         header,
		TransportCodes: tc,
		Path:           path,
		Payload:        payload,
		Raw:            strings.ToUpper(hexString),
		Anomaly:        anomaly,
	}, nil
}

// PayloadJSON serializes the payload to JSON for DB storage.
func PayloadJSON(p *Payload) string {
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ValidateAdvert checks decoded advert data before DB insertion.
func ValidateAdvert(p *Payload) (bool, string) {
	if p == nil || p.Error != "" {
		reason := "null advert"
		if p != nil {
			reason = p.Error
		}
		return false, reason
	}

	pk := p.PubKey
	if len(pk) < 16 {
		return false, fmt.Sprintf("pubkey too short (%d hex chars)", len(pk))
	}
	allZero := true
	for _, c := range pk {
		if c != '0' {
			allZero = false
			break
		}
	}
	if allZero {
		return false, "pubkey is all zeros"
	}

	if p.Lat != nil {
		if math.IsInf(*p.Lat, 0) || math.IsNaN(*p.Lat) || *p.Lat < -90 || *p.Lat > 90 {
			return false, fmt.Sprintf("invalid lat: %f", *p.Lat)
		}
	}
	if p.Lon != nil {
		if math.IsInf(*p.Lon, 0) || math.IsNaN(*p.Lon) || *p.Lon < -180 || *p.Lon > 180 {
			return false, fmt.Sprintf("invalid lon: %f", *p.Lon)
		}
	}

	if p.Name != "" {
		for _, c := range p.Name {
			if (c >= 0x00 && c <= 0x08) || c == 0x0b || c == 0x0c || (c >= 0x0e && c <= 0x1f) || c == 0x7f {
				return false, "name contains control characters"
			}
		}
		if len(p.Name) > 64 {
			return false, fmt.Sprintf("name too long (%d chars)", len(p.Name))
		}
	}

	if p.Flags != nil {
		role := advertRole(p.Flags)
		validRoles := map[string]bool{"repeater": true, "companion": true, "room": true, "sensor": true}
		if !validRoles[role] {
			return false, fmt.Sprintf("unknown role: %s", role)
		}
	}

	return true, ""
}
