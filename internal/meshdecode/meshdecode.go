// Package meshdecode holds the MeshCore packet-decoding logic that is
// byte-for-byte identical between the corescope server and ingestor modules.
//
// Only structurally shared, behavior-identical code lives here. Anything that
// constructs or returns a Payload value is deliberately kept local to each
// module: the server and ingestor Payload structs have diverged (the ingestor
// carries extra channel-decryption and telemetry fields), so the per-payload
// decoders, DecodePacket, PayloadJSON and ValidateAdvert cannot be shared
// without changing behavior. decodeHeader is also kept local because it
// resolves payload-type names through a module-local map that differs between
// the two binaries.
package meshdecode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/meshcore-analyzer/packetpath"
)

// Route type constants (header bits 1-0).
const (
	RouteTransportFlood  = 0
	RouteFlood           = 1
	RouteDirect          = 2
	RouteTransportDirect = 3
)

// Payload type constants (header bits 5-2).
const (
	PayloadREQ        = 0x00
	PayloadRESPONSE   = 0x01
	PayloadTXT_MSG    = 0x02
	PayloadACK        = 0x03
	PayloadADVERT     = 0x04
	PayloadGRP_TXT    = 0x05
	PayloadGRP_DATA   = 0x06
	PayloadANON_REQ   = 0x07
	PayloadPATH       = 0x08
	PayloadTRACE      = 0x09
	PayloadMULTIPART  = 0x0A
	PayloadCONTROL    = 0x0B
	PayloadRAW_CUSTOM = 0x0F
)

// RouteTypeNames maps the route-type bits to their firmware-standard name.
var RouteTypeNames = map[int]string{
	0: "TRANSPORT_FLOOD",
	1: "FLOOD",
	2: "DIRECT",
	3: "TRANSPORT_DIRECT",
}

// Header is the decoded packet header.
type Header struct {
	RouteType       int    `json:"routeType"`
	RouteTypeName   string `json:"routeTypeName"`
	PayloadType     int    `json:"payloadType"`
	PayloadTypeName string `json:"payloadTypeName"`
	PayloadVersion  int    `json:"payloadVersion"`
}

// TransportCodes are present on TRANSPORT_FLOOD and TRANSPORT_DIRECT routes.
type TransportCodes struct {
	Code1 string `json:"code1"`
	Code2 string `json:"code2"`
}

// Path holds decoded path/hop information.
type Path struct {
	HashSize      int      `json:"hashSize"`
	HashCount     int      `json:"hashCount"`
	Hops          []string `json:"hops"`
	HopsCompleted *int     `json:"hopsCompleted,omitempty"`
}

// AdvertFlags holds decoded advert flag bits.
type AdvertFlags struct {
	Raw         int  `json:"raw"`
	Type        int  `json:"type"`
	Chat        bool `json:"chat"`
	Repeater    bool `json:"repeater"`
	Room        bool `json:"room"`
	Sensor      bool `json:"sensor"`
	HasLocation bool `json:"hasLocation"`
	HasFeat1    bool `json:"hasFeat1"`
	HasFeat2    bool `json:"hasFeat2"`
	HasName     bool `json:"hasName"`
}

// Firmware-derived limits — see firmware/src/MeshCore.h:19,21.
const (
	MaxPathSize      = 64  // MAX_PATH_SIZE — total path bytes allowed
	MaxPacketPayload = 184 // MAX_PACKET_PAYLOAD — max raw payload bytes
)

// IsValidPathLen mirrors firmware Packet::isValidPathLen
// (firmware/src/Packet.cpp:13-18). hash_size==4 is reserved; total path bytes
// must fit within MAX_PATH_SIZE.
func IsValidPathLen(pathByte byte) bool {
	hashCount := int(pathByte & 0x3F)
	hashSize := int(pathByte>>6) + 1
	if hashSize == 4 {
		return false // reserved
	}
	return hashCount*hashSize <= MaxPathSize
}

// DecodePath decodes the path/hop bytes that follow the path byte.
func DecodePath(pathByte byte, buf []byte, offset int) (Path, int, error) {
	hashSize := int(pathByte>>6) + 1
	hashCount := int(pathByte & 0x3F)
	// Exact mirror of firmware Packet::isValidPathLen (Packet.cpp:13-18).
	// hash_size==4 is reserved and is rejected by firmware regardless of
	// hash_count, so we must reject 0xC0 etc even on zero-hop packets —
	// firmware never emits them, so an on-wire pathByte with the upper
	// 2 bits set to 11 is by definition malformed/adversarial.
	if !IsValidPathLen(pathByte) {
		return Path{}, 0, fmt.Errorf("invalid path encoding: pathByte 0x%02X (hash_size=%d hash_count=%d) violates firmware validity (Packet.cpp:13-18, MAX_PATH_SIZE=%d)", pathByte, hashSize, hashCount, MaxPathSize)
	}
	totalBytes := hashSize * hashCount
	hops := make([]string, 0, hashCount)

	for i := 0; i < hashCount; i++ {
		start := offset + i*hashSize
		end := start + hashSize
		if end > len(buf) {
			break
		}
		hops = append(hops, strings.ToUpper(hex.EncodeToString(buf[start:end])))
	}

	return Path{
		HashSize:  hashSize,
		HashCount: hashCount,
		Hops:      hops,
	}, totalBytes, nil
}

// IsTransportRoute delegates to packetpath.IsTransportRoute.
func IsTransportRoute(routeType int) bool {
	return packetpath.IsTransportRoute(routeType)
}

// ComputeContentHash computes the SHA-256-based content hash (first 16 hex chars).
// It hashes the payload-type nibble + payload (skipping path bytes) to produce a
// route-independent identifier for the same logical packet. For TRACE packets,
// path_len is included in the hash to match firmware behavior.
func ComputeContentHash(rawHex string) string {
	buf, err := hex.DecodeString(rawHex)
	if err != nil || len(buf) < 2 {
		if len(rawHex) >= 16 {
			return rawHex[:16]
		}
		return rawHex
	}

	headerByte := buf[0]
	offset := 1
	if IsTransportRoute(int(headerByte & 0x03)) {
		offset += 4
	}
	if offset >= len(buf) {
		if len(rawHex) >= 16 {
			return rawHex[:16]
		}
		return rawHex
	}
	pathByte := buf[offset]
	offset++
	hashSize := int((pathByte>>6)&0x3) + 1
	hashCount := int(pathByte & 0x3F)
	pathBytes := hashSize * hashCount

	payloadStart := offset + pathBytes
	if payloadStart > len(buf) {
		if len(rawHex) >= 16 {
			return rawHex[:16]
		}
		return rawHex
	}

	payload := buf[payloadStart:]

	// Hash payload-type byte only (bits 2-5 of header), not the full header.
	// Firmware: SHA256(payload_type + [path_len for TRACE] + payload)
	// Using the full header caused different hashes for the same logical packet
	// when route type or version bits differed. See issue #786.
	payloadType := (headerByte >> 2) & 0x0F
	// Pre-size the buffer to its final length (1 type byte + 2 path_len bytes
	// for TRACE + payload) so the appends below don't reallocate.
	knownLen := 1 + len(payload)
	if int(payloadType) == PayloadTRACE {
		knownLen += 2
	}
	toHash := make([]byte, 0, knownLen)
	toHash = append(toHash, payloadType)
	if int(payloadType) == PayloadTRACE {
		// Firmware uses uint16_t path_len (2 bytes, little-endian)
		toHash = append(toHash, pathByte, 0x00)
	}
	toHash = append(toHash, payload...)

	h := sha256.Sum256(toHash)
	return hex.EncodeToString(h[:])[:16]
}

// SanitizeName strips non-printable characters (< 0x20 except tab/newline) and DEL.
func SanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		if c == '\t' || c == '\n' || (c >= 0x20 && c != 0x7f) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// AdvertRole returns the node role implied by the advert flags.
func AdvertRole(f *AdvertFlags) string {
	if f.Repeater {
		return "repeater"
	}
	if f.Room {
		return "room"
	}
	if f.Sensor {
		return "sensor"
	}
	return "companion"
}

// EpochToISO formats a Unix epoch (seconds) as an ISO-8601 UTC timestamp.
func EpochToISO(epoch uint32) string {
	t := time.Unix(int64(epoch), 0)
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
