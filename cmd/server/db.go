package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps a read-only connection to the MeshCore SQLite database.
type DB struct {
	conn *sql.DB
}

// OpenDB opens a read-only SQLite connection with WAL mode.
func OpenDB(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_busy_timeout=5000", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(4)
	conn.SetMaxIdleConns(2)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping failed: %w", err)
	}
	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// Node represents a row from the nodes table.
type Node struct {
	PublicKey   string   `json:"public_key"`
	Name       *string  `json:"name"`
	Role       *string  `json:"role"`
	Lat        *float64 `json:"lat"`
	Lon        *float64 `json:"lon"`
	LastSeen   *string  `json:"last_seen"`
	FirstSeen  *string  `json:"first_seen"`
	AdvertCount int     `json:"advert_count"`
}

// Observer represents a row from the observers table.
type Observer struct {
	ID            string  `json:"id"`
	Name          *string `json:"name"`
	IATA          *string `json:"iata"`
	LastSeen      *string `json:"last_seen"`
	FirstSeen     *string `json:"first_seen"`
	PacketCount   int     `json:"packet_count"`
	Model         *string `json:"model"`
	Firmware      *string `json:"firmware"`
	ClientVersion *string `json:"client_version"`
	Radio         *string `json:"radio"`
	BatteryMv     *int    `json:"battery_mv"`
	UptimeSecs    *int    `json:"uptime_secs"`
	NoiseFloor    *int    `json:"noise_floor"`
}

// Transmission represents a row from the transmissions table.
type Transmission struct {
	ID             int     `json:"id"`
	RawHex         *string `json:"raw_hex"`
	Hash           string  `json:"hash"`
	FirstSeen      string  `json:"first_seen"`
	RouteType      *int    `json:"route_type"`
	PayloadType    *int    `json:"payload_type"`
	PayloadVersion *int    `json:"payload_version"`
	DecodedJSON    *string `json:"decoded_json"`
	CreatedAt      *string `json:"created_at"`
}

// Observation (from packets_v view).
type Observation struct {
	ID           int      `json:"id"`
	RawHex       *string  `json:"raw_hex"`
	Timestamp    *string  `json:"timestamp"`
	ObserverID   *string  `json:"observer_id"`
	ObserverName *string  `json:"observer_name"`
	Direction    *string  `json:"direction"`
	SNR          *float64 `json:"snr"`
	RSSI         *float64 `json:"rssi"`
	Score        *int     `json:"score"`
	Hash         *string  `json:"hash"`
	RouteType    *int     `json:"route_type"`
	PayloadType  *int     `json:"payload_type"`
	PayloadVer   *int     `json:"payload_version"`
	PathJSON     *string  `json:"path_json"`
	DecodedJSON  *string  `json:"decoded_json"`
	CreatedAt    *string  `json:"created_at"`
}

// Stats holds system statistics.
type Stats struct {
	TotalPackets       int `json:"totalPackets"`
	TotalTransmissions int `json:"totalTransmissions"`
	TotalObservations  int `json:"totalObservations"`
	TotalNodes         int `json:"totalNodes"`
	TotalObservers     int `json:"totalObservers"`
	PacketsLastHour    int `json:"packetsLastHour"`
}

// GetStats returns aggregate counts.
func (db *DB) GetStats() (*Stats, error) {
	s := &Stats{}
	err := db.conn.QueryRow("SELECT COUNT(*) FROM transmissions").Scan(&s.TotalTransmissions)
	if err != nil {
		return nil, err
	}
	s.TotalPackets = s.TotalTransmissions

	db.conn.QueryRow("SELECT COUNT(*) FROM observations").Scan(&s.TotalObservations)
	db.conn.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&s.TotalNodes)
	db.conn.QueryRow("SELECT COUNT(*) FROM observers").Scan(&s.TotalObservers)

	oneHourAgo := time.Now().Add(-1 * time.Hour).Unix()
	db.conn.QueryRow("SELECT COUNT(*) FROM observations WHERE timestamp > ?", oneHourAgo).Scan(&s.PacketsLastHour)

	return s, nil
}

// GetRoleCounts returns count per role.
func (db *DB) GetRoleCounts() map[string]int {
	counts := map[string]int{}
	for _, role := range []string{"repeater", "room", "companion", "sensor"} {
		var c int
		db.conn.QueryRow("SELECT COUNT(*) FROM nodes WHERE role = ?", role).Scan(&c)
		counts[role+"s"] = c
	}
	return counts
}

// PacketQuery holds filter params for packet listing.
type PacketQuery struct {
	Limit    int
	Offset   int
	Type     *int
	Route    *int
	Observer string
	Hash     string
	Since    string
	Until    string
	Region   string
	Node     string
	Order    string // ASC or DESC
}

// PacketResult wraps paginated packet list.
type PacketResult struct {
	Packets []map[string]interface{} `json:"packets"`
	Total   int                      `json:"total"`
}

// QueryPackets returns paginated, filtered packets from packets_v view.
func (db *DB) QueryPackets(q PacketQuery) (*PacketResult, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Order == "" {
		q.Order = "DESC"
	}

	where, args := db.buildPacketWhere(q)
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM packets_v %s", w)
	if err := db.conn.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, err
	}

	querySQL := fmt.Sprintf("SELECT id, raw_hex, timestamp, observer_id, observer_name, direction, snr, rssi, score, hash, route_type, payload_type, payload_version, path_json, decoded_json, created_at FROM packets_v %s ORDER BY timestamp %s LIMIT ? OFFSET ?", w, q.Order)
	args = append(args, q.Limit, q.Offset)

	rows, err := db.conn.Query(querySQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	packets := make([]map[string]interface{}, 0)
	for rows.Next() {
		p := scanPacketRow(rows)
		if p != nil {
			packets = append(packets, p)
		}
	}

	return &PacketResult{Packets: packets, Total: total}, nil
}

// QueryGroupedPackets groups by hash (transmissions).
func (db *DB) QueryGroupedPackets(q PacketQuery) (*PacketResult, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	where, args := db.buildPacketWhere(q)
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}

	qry := fmt.Sprintf(`SELECT hash, COUNT(*) as count, COUNT(DISTINCT observer_id) as observer_count,
		MAX(timestamp) as latest, MIN(observer_id) as observer_id, MIN(observer_name) as observer_name,
		MIN(path_json) as path_json, MIN(payload_type) as payload_type, MIN(route_type) as route_type,
		MIN(raw_hex) as raw_hex, MIN(decoded_json) as decoded_json, MIN(snr) as snr, MIN(rssi) as rssi
		FROM packets_v %s GROUP BY hash ORDER BY latest DESC LIMIT ? OFFSET ?`, w)
	args = append(args, q.Limit, q.Offset)

	rows, err := db.conn.Query(qry, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	packets := make([]map[string]interface{}, 0)
	for rows.Next() {
		var hash, latest, observerID, observerName, pathJSON, rawHex, decodedJSON sql.NullString
		var count, observerCount int
		var payloadType, routeType sql.NullInt64
		var snr, rssi sql.NullFloat64
		if err := rows.Scan(&hash, &count, &observerCount, &latest, &observerID, &observerName, &pathJSON, &payloadType, &routeType, &rawHex, &decodedJSON, &snr, &rssi); err != nil {
			continue
		}
		p := map[string]interface{}{
			"hash":              nullStr(hash),
			"count":             count,
			"observer_count":    observerCount,
			"observation_count": count,
			"latest":            nullStr(latest),
			"first_seen":        nullStr(latest),
			"observer_id":       nullStr(observerID),
			"observer_name":     nullStr(observerName),
			"path_json":         nullStr(pathJSON),
			"payload_type":      nullInt(payloadType),
			"route_type":        nullInt(routeType),
			"raw_hex":           nullStr(rawHex),
			"decoded_json":      nullStr(decodedJSON),
			"snr":               nullFloat(snr),
			"rssi":              nullFloat(rssi),
		}
		packets = append(packets, p)
	}

	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(DISTINCT hash) FROM packets_v %s", w)
	baseArgs := args[:len(args)-2] // remove LIMIT/OFFSET
	db.conn.QueryRow(countSQL, baseArgs...).Scan(&total)

	return &PacketResult{Packets: packets, Total: total}, nil
}

func (db *DB) buildPacketWhere(q PacketQuery) ([]string, []interface{}) {
	var where []string
	var args []interface{}

	if q.Type != nil {
		where = append(where, "payload_type = ?")
		args = append(args, *q.Type)
	}
	if q.Route != nil {
		where = append(where, "route_type = ?")
		args = append(args, *q.Route)
	}
	if q.Observer != "" {
		where = append(where, "observer_id = ?")
		args = append(args, q.Observer)
	}
	if q.Hash != "" {
		where = append(where, "hash = ?")
		args = append(args, strings.ToLower(q.Hash))
	}
	if q.Since != "" {
		where = append(where, "timestamp > ?")
		args = append(args, q.Since)
	}
	if q.Until != "" {
		where = append(where, "timestamp < ?")
		args = append(args, q.Until)
	}
	if q.Region != "" {
		where = append(where, "observer_id IN (SELECT id FROM observers WHERE iata = ?)")
		args = append(args, q.Region)
	}
	if q.Node != "" {
		pk := db.resolveNodePubkey(q.Node)
		where = append(where, "decoded_json LIKE ?")
		args = append(args, "%"+pk+"%")
	}
	return where, args
}

func (db *DB) resolveNodePubkey(nodeIDOrName string) string {
	var pk string
	err := db.conn.QueryRow("SELECT public_key FROM nodes WHERE public_key = ? OR name = ? LIMIT 1", nodeIDOrName, nodeIDOrName).Scan(&pk)
	if err != nil {
		return nodeIDOrName
	}
	return pk
}

// GetPacketByID fetches a single packet/observation.
func (db *DB) GetPacketByID(id int) (map[string]interface{}, error) {
	rows, err := db.conn.Query("SELECT id, raw_hex, timestamp, observer_id, observer_name, direction, snr, rssi, score, hash, route_type, payload_type, payload_version, path_json, decoded_json, created_at FROM packets_v WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		return scanPacketRow(rows), nil
	}
	return nil, nil
}

// GetTransmissionByID fetches from transmissions table.
func (db *DB) GetTransmissionByID(id int) (map[string]interface{}, error) {
	var txID int
	var rawHex, hash, firstSeen, decodedJSON, createdAt sql.NullString
	var routeType, payloadType, payloadVersion sql.NullInt64
	err := db.conn.QueryRow("SELECT id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, created_at FROM transmissions WHERE id = ?", id).
		Scan(&txID, &rawHex, &hash, &firstSeen, &routeType, &payloadType, &payloadVersion, &decodedJSON, &createdAt)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"id":              txID,
		"raw_hex":         nullStr(rawHex),
		"hash":            nullStr(hash),
		"first_seen":      nullStr(firstSeen),
		"timestamp":       nullStr(firstSeen),
		"route_type":      nullInt(routeType),
		"payload_type":    nullInt(payloadType),
		"payload_version": nullInt(payloadVersion),
		"decoded_json":    nullStr(decodedJSON),
		"created_at":      nullStr(createdAt),
	}, nil
}

// GetPacketByHash fetches a transmission by content hash.
func (db *DB) GetPacketByHash(hash string) (map[string]interface{}, error) {
	var txID int
	var rawHex, h, firstSeen, decodedJSON, createdAt sql.NullString
	var routeType, payloadType, payloadVersion sql.NullInt64
	err := db.conn.QueryRow("SELECT id, raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, created_at FROM transmissions WHERE hash = ?", strings.ToLower(hash)).
		Scan(&txID, &rawHex, &h, &firstSeen, &routeType, &payloadType, &payloadVersion, &decodedJSON, &createdAt)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"id":              txID,
		"raw_hex":         nullStr(rawHex),
		"hash":            nullStr(h),
		"first_seen":      nullStr(firstSeen),
		"timestamp":       nullStr(firstSeen),
		"route_type":      nullInt(routeType),
		"payload_type":    nullInt(payloadType),
		"payload_version": nullInt(payloadVersion),
		"decoded_json":    nullStr(decodedJSON),
		"created_at":      nullStr(createdAt),
	}, nil
}

// GetObservationsForHash returns all observations for a given hash.
func (db *DB) GetObservationsForHash(hash string) ([]map[string]interface{}, error) {
	rows, err := db.conn.Query(`SELECT id, raw_hex, timestamp, observer_id, observer_name, direction, snr, rssi, score, hash, route_type, payload_type, payload_version, path_json, decoded_json, created_at
		FROM packets_v WHERE hash = ? ORDER BY timestamp DESC`, strings.ToLower(hash))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]map[string]interface{}, 0)
	for rows.Next() {
		p := scanPacketRow(rows)
		if p != nil {
			result = append(result, p)
		}
	}
	return result, nil
}

// GetNodes returns filtered, paginated node list.
func (db *DB) GetNodes(limit, offset int, role, search, before, lastHeard, sortBy, region string) ([]map[string]interface{}, int, map[string]int, error) {
	var where []string
	var args []interface{}

	if role != "" {
		where = append(where, "role = ?")
		args = append(args, role)
	}
	if search != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+search+"%")
	}
	if before != "" {
		where = append(where, "first_seen <= ?")
		args = append(args, before)
	}
	if lastHeard != "" {
		durations := map[string]int64{
			"1h": 3600000, "6h": 21600000, "24h": 86400000,
			"7d": 604800000, "30d": 2592000000,
		}
		if ms, ok := durations[lastHeard]; ok {
			since := time.Now().Add(-time.Duration(ms) * time.Millisecond).Format(time.RFC3339)
			where = append(where, "last_seen > ?")
			args = append(args, since)
		}
	}

	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}

	sortMap := map[string]string{
		"name": "name ASC", "lastSeen": "last_seen DESC", "packetCount": "advert_count DESC",
	}
	order := "last_seen DESC"
	if s, ok := sortMap[sortBy]; ok {
		order = s
	}

	if limit <= 0 {
		limit = 50
	}

	var total int
	db.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM nodes %s", w), args...).Scan(&total)

	querySQL := fmt.Sprintf("SELECT public_key, name, role, lat, lon, last_seen, first_seen, advert_count FROM nodes %s ORDER BY %s LIMIT ? OFFSET ?", w, order)
	qArgs := append(args, limit, offset)

	rows, err := db.conn.Query(querySQL, qArgs...)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()

	nodes := make([]map[string]interface{}, 0)
	for rows.Next() {
		n := scanNodeRow(rows)
		if n != nil {
			nodes = append(nodes, n)
		}
	}

	counts := db.GetRoleCounts()
	return nodes, total, counts, nil
}

// SearchNodes searches nodes by name or pubkey prefix.
func (db *DB) SearchNodes(query string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.conn.Query(`SELECT public_key, name, role, lat, lon, last_seen, first_seen, advert_count
		FROM nodes WHERE name LIKE ? OR public_key LIKE ? ORDER BY last_seen DESC LIMIT ?`,
		"%"+query+"%", query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]map[string]interface{}, 0)
	for rows.Next() {
		n := scanNodeRow(rows)
		if n != nil {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// GetNodeByPubkey returns a single node.
func (db *DB) GetNodeByPubkey(pubkey string) (map[string]interface{}, error) {
	rows, err := db.conn.Query("SELECT public_key, name, role, lat, lon, last_seen, first_seen, advert_count FROM nodes WHERE public_key = ?", pubkey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if rows.Next() {
		return scanNodeRow(rows), nil
	}
	return nil, nil
}

// GetRecentPacketsForNode returns recent packets referencing a node.
func (db *DB) GetRecentPacketsForNode(pubkey string, name string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	pk := "%" + pubkey + "%"
	np := "%" + name + "%"
	rows, err := db.conn.Query(`SELECT id, raw_hex, timestamp, observer_id, observer_name, direction, snr, rssi, score, hash, route_type, payload_type, payload_version, path_json, decoded_json, created_at
		FROM packets_v WHERE decoded_json LIKE ? OR decoded_json LIKE ?
		ORDER BY timestamp DESC LIMIT ?`, pk, np, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	packets := make([]map[string]interface{}, 0)
	for rows.Next() {
		p := scanPacketRow(rows)
		if p != nil {
			packets = append(packets, p)
		}
	}
	return packets, nil
}

// GetObservers returns all observers sorted by last_seen DESC.
func (db *DB) GetObservers() ([]Observer, error) {
	rows, err := db.conn.Query("SELECT id, name, iata, last_seen, first_seen, packet_count, model, firmware, client_version, radio, battery_mv, uptime_secs, noise_floor FROM observers ORDER BY last_seen DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var observers []Observer
	for rows.Next() {
		var o Observer
		if err := rows.Scan(&o.ID, &o.Name, &o.IATA, &o.LastSeen, &o.FirstSeen, &o.PacketCount, &o.Model, &o.Firmware, &o.ClientVersion, &o.Radio, &o.BatteryMv, &o.UptimeSecs, &o.NoiseFloor); err != nil {
			continue
		}
		observers = append(observers, o)
	}
	return observers, nil
}

// GetObserverByID returns a single observer.
func (db *DB) GetObserverByID(id string) (*Observer, error) {
	var o Observer
	err := db.conn.QueryRow("SELECT id, name, iata, last_seen, first_seen, packet_count, model, firmware, client_version, radio, battery_mv, uptime_secs, noise_floor FROM observers WHERE id = ?", id).
		Scan(&o.ID, &o.Name, &o.IATA, &o.LastSeen, &o.FirstSeen, &o.PacketCount, &o.Model, &o.Firmware, &o.ClientVersion, &o.Radio, &o.BatteryMv, &o.UptimeSecs, &o.NoiseFloor)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// GetObserverIdsForRegion returns observer IDs for given IATA codes.
func (db *DB) GetObserverIdsForRegion(regionParam string) ([]string, error) {
	if regionParam == "" {
		return nil, nil
	}
	codes := strings.Split(regionParam, ",")
	placeholders := make([]string, len(codes))
	args := make([]interface{}, len(codes))
	for i, c := range codes {
		placeholders[i] = "?"
		args[i] = strings.TrimSpace(c)
	}
	rows, err := db.conn.Query(fmt.Sprintf("SELECT id FROM observers WHERE iata IN (%s)", strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

// GetDistinctIATAs returns all distinct IATA codes from observers.
func (db *DB) GetDistinctIATAs() ([]string, error) {
	rows, err := db.conn.Query("SELECT DISTINCT iata FROM observers WHERE iata IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var code string
		rows.Scan(&code)
		codes = append(codes, code)
	}
	return codes, nil
}

// GetNodeHealth returns health info for a node (observers, stats, recent packets).
func (db *DB) GetNodeHealth(pubkey string) (map[string]interface{}, error) {
	node, err := db.GetNodeByPubkey(pubkey)
	if err != nil || node == nil {
		return nil, err
	}

	name := ""
	if n, ok := node["name"]; ok && n != nil {
		name = fmt.Sprintf("%v", n)
	}

	pk := "%" + pubkey + "%"
	np := "%" + name + "%"
	whereClause := "decoded_json LIKE ? OR decoded_json LIKE ?"
	if name == "" {
		whereClause = "decoded_json LIKE ?"
		np = pk
	}

	todayStart := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)

	// Observers
	observerSQL := fmt.Sprintf(`SELECT observer_id, observer_name, AVG(snr) as avgSnr, AVG(rssi) as avgRssi, COUNT(*) as packetCount
		FROM packets_v WHERE (%s) AND observer_id IS NOT NULL GROUP BY observer_id ORDER BY packetCount DESC`, whereClause)
	oRows, err := db.conn.Query(observerSQL, pk, np)
	if err != nil {
		return nil, err
	}
	defer oRows.Close()

	observers := make([]map[string]interface{}, 0)
	for oRows.Next() {
		var obsID, obsName sql.NullString
		var avgSnr, avgRssi sql.NullFloat64
		var pktCount int
		oRows.Scan(&obsID, &obsName, &avgSnr, &avgRssi, &pktCount)
		observers = append(observers, map[string]interface{}{
			"observer_id":   nullStr(obsID),
			"observer_name": nullStr(obsName),
			"avgSnr":        nullFloat(avgSnr),
			"avgRssi":       nullFloat(avgRssi),
			"packetCount":   pktCount,
		})
	}

	// Stats
	var packetsToday, totalPackets int
	db.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM packets_v WHERE (%s) AND timestamp > ?", whereClause), pk, np, todayStart).Scan(&packetsToday)
	db.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM packets_v WHERE (%s)", whereClause), pk, np).Scan(&totalPackets)

	var avgSnr sql.NullFloat64
	db.conn.QueryRow(fmt.Sprintf("SELECT AVG(snr) FROM packets_v WHERE (%s)", whereClause), pk, np).Scan(&avgSnr)

	var lastHeard sql.NullString
	db.conn.QueryRow(fmt.Sprintf("SELECT MAX(timestamp) FROM packets_v WHERE (%s)", whereClause), pk, np).Scan(&lastHeard)

	// Avg hops
	hRows, _ := db.conn.Query(fmt.Sprintf("SELECT path_json FROM packets_v WHERE (%s) AND path_json IS NOT NULL", whereClause), pk, np)
	totalHops, hopCount := 0, 0
	if hRows != nil {
		defer hRows.Close()
		for hRows.Next() {
			var pj sql.NullString
			hRows.Scan(&pj)
			if pj.Valid {
				var hops []interface{}
				if json.Unmarshal([]byte(pj.String), &hops) == nil {
					totalHops += len(hops)
					hopCount++
				}
			}
		}
	}
	avgHops := 0
	if hopCount > 0 {
		avgHops = int(math.Round(float64(totalHops) / float64(hopCount)))
	}

	// Recent packets
	recentPackets, _ := db.GetRecentPacketsForNode(pubkey, name, 20)

	return map[string]interface{}{
		"node":      node,
		"observers": observers,
		"stats": map[string]interface{}{
			"totalTransmissions": totalPackets,
			"totalObservations":  totalPackets,
			"totalPackets":       totalPackets,
			"packetsToday":       packetsToday,
			"avgSnr":             nullFloat(avgSnr),
			"avgHops":            avgHops,
			"lastHeard":          nullStr(lastHeard),
		},
		"recentPackets": recentPackets,
	}, nil
}

// GetNetworkStatus returns overall network health status.
func (db *DB) GetNetworkStatus(healthThresholds HealthThresholds) (map[string]interface{}, error) {
	rows, err := db.conn.Query("SELECT public_key, name, role, last_seen FROM nodes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UnixMilli()
	active, degraded, silent, total := 0, 0, 0, 0
	roleCounts := map[string]int{}

	for rows.Next() {
		var pk string
		var name, role, lastSeen sql.NullString
		rows.Scan(&pk, &name, &role, &lastSeen)
		total++
		r := "unknown"
		if role.Valid {
			r = role.String
		}
		roleCounts[r]++

		age := int64(math.MaxInt64)
		if lastSeen.Valid {
			if t, err := time.Parse(time.RFC3339, lastSeen.String); err == nil {
				age = now - t.UnixMilli()
			} else if t, err := time.Parse("2006-01-02 15:04:05", lastSeen.String); err == nil {
				age = now - t.UnixMilli()
			}
		}
		degradedMs, silentMs := healthThresholds.GetHealthMs(r)
		if age < int64(degradedMs) {
			active++
		} else if age < int64(silentMs) {
			degraded++
		} else {
			silent++
		}
	}

	return map[string]interface{}{
		"total": total, "active": active, "degraded": degraded, "silent": silent,
		"roleCounts": roleCounts,
	}, nil
}

// GetTraces returns observations for a hash.
func (db *DB) GetTraces(hash string) ([]map[string]interface{}, error) {
	rows, err := db.conn.Query(`SELECT observer_id, observer_name, timestamp, snr, rssi, path_json
		FROM packets_v WHERE hash = ? ORDER BY timestamp ASC`, strings.ToLower(hash))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var traces []map[string]interface{}
	for rows.Next() {
		var obsID, obsName, ts, pathJSON sql.NullString
		var snr, rssi sql.NullFloat64
		rows.Scan(&obsID, &obsName, &ts, &snr, &rssi, &pathJSON)
		traces = append(traces, map[string]interface{}{
			"observer":      nullStr(obsID),
			"observer_name": nullStr(obsName),
			"time":          nullStr(ts),
			"snr":           nullFloat(snr),
			"rssi":          nullFloat(rssi),
			"path_json":     nullStr(pathJSON),
		})
	}
	if traces == nil {
		traces = make([]map[string]interface{}, 0)
	}
	return traces, nil
}

// GetChannels returns channel list from GRP_TXT packets.
func (db *DB) GetChannels() ([]map[string]interface{}, error) {
	rows, err := db.conn.Query(`SELECT decoded_json, timestamp FROM packets_v WHERE payload_type = 5 ORDER BY timestamp ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	channelMap := map[string]map[string]interface{}{}
	for rows.Next() {
		var dj, ts sql.NullString
		rows.Scan(&dj, &ts)
		if !dj.Valid {
			continue
		}
		var decoded map[string]interface{}
		if json.Unmarshal([]byte(dj.String), &decoded) != nil {
			continue
		}
		dtype, _ := decoded["type"].(string)
		if dtype != "CHAN" {
			continue
		}
		channelName, _ := decoded["channel"].(string)
		if channelName == "" {
			channelName = "unknown"
		}
		key := channelName

		ch, exists := channelMap[key]
		if !exists {
			ch = map[string]interface{}{
				"hash": key, "name": channelName,
				"lastMessage": nil, "lastSender": nil,
				"messageCount": 0, "lastActivity": nullStr(ts),
			}
			channelMap[key] = ch
		}
		ch["messageCount"] = ch["messageCount"].(int) + 1
		if ts.Valid {
			ch["lastActivity"] = ts.String
		}
		if text, ok := decoded["text"].(string); ok && text != "" {
			idx := strings.Index(text, ": ")
			if idx > 0 {
				ch["lastMessage"] = text[idx+2:]
			} else {
				ch["lastMessage"] = text
			}
			if sender, ok := decoded["sender"].(string); ok {
				ch["lastSender"] = sender
			}
		}
	}

	channels := make([]map[string]interface{}, 0, len(channelMap))
	for _, ch := range channelMap {
		channels = append(channels, ch)
	}
	return channels, nil
}

// GetChannelMessages returns messages for a specific channel.
func (db *DB) GetChannelMessages(channelHash string, limit, offset int) ([]map[string]interface{}, int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.conn.Query(`SELECT id, hash, decoded_json, timestamp, observer_id, observer_name, snr, path_json
		FROM packets_v WHERE payload_type = 5 ORDER BY timestamp ASC`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	type msg struct {
		Data    map[string]interface{}
		Repeats int
	}
	msgMap := map[string]*msg{}
	var msgOrder []string

	for rows.Next() {
		var pktID int
		var pktHash, dj, ts, obsID, obsName, pathJSON sql.NullString
		var snr sql.NullFloat64
		rows.Scan(&pktID, &pktHash, &dj, &ts, &obsID, &obsName, &snr, &pathJSON)
		if !dj.Valid {
			continue
		}
		var decoded map[string]interface{}
		if json.Unmarshal([]byte(dj.String), &decoded) != nil {
			continue
		}
		dtype, _ := decoded["type"].(string)
		if dtype != "CHAN" {
			continue
		}
		ch, _ := decoded["channel"].(string)
		if ch == "" {
			ch = "unknown"
		}
		if ch != channelHash {
			continue
		}

		text, _ := decoded["text"].(string)
		sender, _ := decoded["sender"].(string)
		if sender == "" && text != "" {
			idx := strings.Index(text, ": ")
			if idx > 0 && idx < 50 {
				sender = text[:idx]
			}
		}

		dedupeKey := fmt.Sprintf("%s:%s", sender, nullStr(pktHash))

		if existing, ok := msgMap[dedupeKey]; ok {
			existing.Repeats++
		} else {
			displaySender := sender
			displayText := text
			if text != "" {
				idx := strings.Index(text, ": ")
				if idx > 0 && idx < 50 {
					displaySender = text[:idx]
					displayText = text[idx+2:]
				}
			}

			var hops int
			if pathJSON.Valid {
				var h []interface{}
				if json.Unmarshal([]byte(pathJSON.String), &h) == nil {
					hops = len(h)
				}
			}

			senderTs, _ := decoded["sender_timestamp"]
			m := &msg{
				Data: map[string]interface{}{
					"sender":           displaySender,
					"text":             displayText,
					"timestamp":        nullStr(ts),
					"sender_timestamp": senderTs,
					"packetId":         pktID,
					"packetHash":       nullStr(pktHash),
					"repeats":          1,
					"observers":        []string{},
					"hops":             hops,
					"snr":              nullFloat(snr),
				},
				Repeats: 1,
			}
			if obsName.Valid {
				m.Data["observers"] = []string{obsName.String}
			} else if obsID.Valid {
				m.Data["observers"] = []string{obsID.String}
			}
			msgMap[dedupeKey] = m
			msgOrder = append(msgOrder, dedupeKey)
		}
	}

	total := len(msgOrder)
	// Return latest messages (tail)
	start := total - limit - offset
	if start < 0 {
		start = 0
	}
	end := total - offset
	if end < 0 {
		end = 0
	}
	if end > total {
		end = total
	}

	messages := make([]map[string]interface{}, 0)
	for i := start; i < end; i++ {
		key := msgOrder[i]
		m := msgMap[key]
		m.Data["repeats"] = m.Repeats
		messages = append(messages, m.Data)
	}

	return messages, total, nil
}

// GetTimestamps returns packet timestamps since a given time.
func (db *DB) GetTimestamps(since string) ([]string, error) {
	rows, err := db.conn.Query("SELECT timestamp FROM packets_v WHERE timestamp > ? ORDER BY timestamp ASC", since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var timestamps []string
	for rows.Next() {
		var ts string
		rows.Scan(&ts)
		timestamps = append(timestamps, ts)
	}
	if timestamps == nil {
		timestamps = []string{}
	}
	return timestamps, nil
}

// GetNodeCountsForPacket returns observation count for a hash.
func (db *DB) GetObservationCount(hash string) int {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM packets_v WHERE hash = ?", strings.ToLower(hash)).Scan(&count)
	return count
}

// GetNewTransmissionsSince returns new transmissions after a given ID for WebSocket polling.
func (db *DB) GetNewTransmissionsSince(lastID int, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.conn.Query(`SELECT t.id, t.raw_hex, t.hash, t.first_seen, t.route_type, t.payload_type, t.payload_version, t.decoded_json
		FROM transmissions t WHERE t.id > ? ORDER BY t.id ASC LIMIT ?`, lastID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id int
		var rawHex, hash, firstSeen, decodedJSON sql.NullString
		var routeType, payloadType, payloadVersion sql.NullInt64
		rows.Scan(&id, &rawHex, &hash, &firstSeen, &routeType, &payloadType, &payloadVersion, &decodedJSON)
		result = append(result, map[string]interface{}{
			"id":              id,
			"raw_hex":         nullStr(rawHex),
			"hash":            nullStr(hash),
			"first_seen":      nullStr(firstSeen),
			"route_type":      nullInt(routeType),
			"payload_type":    nullInt(payloadType),
			"payload_version": nullInt(payloadVersion),
			"decoded_json":    nullStr(decodedJSON),
		})
	}
	return result, nil
}

// GetMaxTransmissionID returns the current max ID for polling.
func (db *DB) GetMaxTransmissionID() int {
	var maxID int
	db.conn.QueryRow("SELECT COALESCE(MAX(id), 0) FROM transmissions").Scan(&maxID)
	return maxID
}

// --- Helpers ---

func scanPacketRow(rows *sql.Rows) map[string]interface{} {
	var id int
	var rawHex, ts, obsID, obsName, direction, hash, pathJSON, decodedJSON, createdAt sql.NullString
	var snr, rssi sql.NullFloat64
	var score, routeType, payloadType, payloadVersion sql.NullInt64

	if err := rows.Scan(&id, &rawHex, &ts, &obsID, &obsName, &direction, &snr, &rssi, &score, &hash, &routeType, &payloadType, &payloadVersion, &pathJSON, &decodedJSON, &createdAt); err != nil {
		return nil
	}
	return map[string]interface{}{
		"id":              id,
		"raw_hex":         nullStr(rawHex),
		"timestamp":       nullStr(ts),
		"observer_id":     nullStr(obsID),
		"observer_name":   nullStr(obsName),
		"direction":       nullStr(direction),
		"snr":             nullFloat(snr),
		"rssi":            nullFloat(rssi),
		"score":           nullInt(score),
		"hash":            nullStr(hash),
		"route_type":      nullInt(routeType),
		"payload_type":    nullInt(payloadType),
		"payload_version": nullInt(payloadVersion),
		"path_json":       nullStr(pathJSON),
		"decoded_json":    nullStr(decodedJSON),
		"created_at":      nullStr(createdAt),
	}
}

func scanNodeRow(rows *sql.Rows) map[string]interface{} {
	var pk string
	var name, role, lastSeen, firstSeen sql.NullString
	var lat, lon sql.NullFloat64
	var advertCount int

	if err := rows.Scan(&pk, &name, &role, &lat, &lon, &lastSeen, &firstSeen, &advertCount); err != nil {
		return nil
	}
	return map[string]interface{}{
		"public_key":   pk,
		"name":         nullStr(name),
		"role":         nullStr(role),
		"lat":          nullFloat(lat),
		"lon":          nullFloat(lon),
		"last_seen":    nullStr(lastSeen),
		"first_seen":   nullStr(firstSeen),
		"advert_count": advertCount,
	}
}

func nullStr(ns sql.NullString) interface{} {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func nullFloat(nf sql.NullFloat64) interface{} {
	if nf.Valid {
		return nf.Float64
	}
	return nil
}

func nullInt(ni sql.NullInt64) interface{} {
	if ni.Valid {
		return int(ni.Int64)
	}
	return nil
}
