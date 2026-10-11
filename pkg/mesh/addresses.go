package mesh

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
)

// MemberRecord is what one environment member's takod reports about its
// mesh state through GET /v1/status. Every field is optional: a fresh node
// records nothing.
type MemberRecord struct {
	// PlatformAddress is the enrolled platform MeshIP. It always wins.
	PlatformAddress string
	// Recorded is node.mesh.address from the node's takod metadata (an
	// address with or without a prefix length).
	Recorded string
	// PublicKey is the member's WireGuard public key, used to tell its own
	// stale peer entries from addresses other environments or projects use.
	PublicKey string
	// PeerAddresses are the addresses configured on the member's interface.
	PeerAddresses []RecordedPeer
}

// RecordedPeer is one peer entry recorded on a member.
type RecordedPeer struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	PublicKey string `json:"publicKey,omitempty"`
}

// Resolution is the outcome of ResolveAddresses.
type Resolution struct {
	// Addresses maps each member to its address with the subnet prefix,
	// for example "10.210.0.2/24".
	Addresses map[string]string
	// Changed lists, sorted, the members whose resolved address differs
	// from their record (including members with no record).
	Changed []string
	// Warnings describe deterministic repairs such as duplicate records.
	Warnings []string
}

// IP returns the bare IP of a member's resolved address.
func (r Resolution) IP(member string) (string, bool) {
	address, ok := r.Addresses[member]
	if !ok {
		return "", false
	}
	ip, ok := parseMeshIPv4(address)
	if !ok {
		return "", false
	}
	return ipString(ip), true
}

// ResolveAddresses assigns stable mesh addresses to an environment's members.
//
//  1. Members are processed in sorted name order; an enrolled member uses its
//     platform address.
//  2. A member keeps its recorded address when it parses, lies inside
//     networkCIDR, and no earlier member took it. A later duplicate loses it
//     and one warning names both members.
//  3. Every other member gets the lowest host address of networkCIDR that is
//     not kept by a member and not listed in any member's recorded peers under
//     a public key outside the membership.
//
// The result depends only on the members and their records, never on an
// operation's target list, the declaration order of servers, or servers
// outside the environment. A fresh environment resolves to .1, .2, ... in
// sorted order, which is what positional addressing produced for sorted
// target lists, and recorded addresses are never renumbered.
func ResolveAddresses(networkCIDR string, subnetBits int, members []string, records map[string]MemberRecord) (Resolution, error) {
	_, network, err := net.ParseCIDR(strings.TrimSpace(networkCIDR))
	if err != nil {
		return Resolution{}, fmt.Errorf("invalid mesh CIDR: %w", err)
	}
	base4 := network.IP.To4()
	if base4 == nil {
		return Resolution{}, fmt.Errorf("mesh.networkCIDR must be IPv4")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return Resolution{}, fmt.Errorf("mesh.networkCIDR %s has no room for node addresses", networkCIDR)
	}
	if subnetBits <= 0 || subnetBits > 32 {
		subnetBits = 32
	}
	base := binary.BigEndian.Uint32(base4)
	broadcast := base + (uint32(1) << uint(32-ones)) - 1
	inside := func(ip uint32) bool { return ip > base && ip < broadcast }

	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	unique := sorted[:0]
	for i, name := range sorted {
		if i > 0 && sorted[i-1] == name {
			continue
		}
		unique = append(unique, name)
	}
	sorted = unique

	memberKeys := map[string]bool{}
	for _, name := range sorted {
		if key := strings.TrimSpace(records[name].PublicKey); key != "" {
			memberKeys[key] = true
		}
	}

	resolution := Resolution{Addresses: make(map[string]string, len(sorted))}
	owner := map[uint32]string{}
	assigned := map[string]uint32{}
	assign := func(name string, ip uint32) {
		assigned[name] = ip
		owner[ip] = name
		resolution.Addresses[name] = fmt.Sprintf("%s/%d", ipString(ip), subnetBits)
	}

	// Step 1: platform addresses.
	for _, name := range sorted {
		platform := strings.TrimSpace(records[name].PlatformAddress)
		if platform == "" {
			continue
		}
		ip, ok := parseMeshIPv4(platform)
		if !ok {
			return Resolution{}, fmt.Errorf("invalid platform mesh IP %q for %s", platform, name)
		}
		assign(name, ip)
	}
	// Step 2: recorded addresses, first sorted member wins a duplicate.
	for _, name := range sorted {
		if _, done := assigned[name]; done {
			continue
		}
		ip, ok := parseMeshIPv4(records[name].Recorded)
		if !ok || !inside(ip) {
			continue
		}
		if holder, taken := owner[ip]; taken {
			resolution.Warnings = append(resolution.Warnings, fmt.Sprintf("mesh address %s is recorded by both %s and %s; %s keeps it and %s gets a new address", ipString(ip), holder, name, holder, name))
			continue
		}
		assign(name, ip)
	}
	// Step 3: lowest free address for everyone else.
	used := map[uint32]bool{}
	for ip := range owner {
		used[ip] = true
	}
	for _, name := range sorted {
		for _, peer := range records[name].PeerAddresses {
			if memberKeys[strings.TrimSpace(peer.PublicKey)] {
				continue
			}
			if ip, ok := parseMeshIPv4(peer.Address); ok {
				used[ip] = true
			}
		}
	}
	next := base + 1
	for _, name := range sorted {
		if _, done := assigned[name]; done {
			continue
		}
		for next < broadcast && used[next] {
			next++
		}
		if next >= broadcast {
			return Resolution{}, fmt.Errorf("mesh.networkCIDR %s has no free address for %s", networkCIDR, name)
		}
		used[next] = true
		assign(name, next)
	}

	for _, name := range sorted {
		recorded, ok := parseMeshIPv4(records[name].Recorded)
		if !ok || recorded != assigned[name] {
			resolution.Changed = append(resolution.Changed, name)
		}
	}
	return resolution, nil
}

// RecordedNodeAddress extracts node.mesh.address from the node document of a
// takod status response.
func RecordedNodeAddress(node map[string]any) string {
	record := decodeNodeRecord(node)
	return strings.TrimSpace(record.Mesh.Address)
}

// RecordedNodeOwner returns the project and environment that last wrote the
// node document of a takod status response.
func RecordedNodeOwner(node map[string]any) (string, string) {
	record := decodeNodeRecord(node)
	return strings.TrimSpace(record.Project), strings.TrimSpace(record.Environment)
}

// MemberRecordFromStatus builds a MemberRecord from the node and peers
// documents of a takod status response. The member's public key comes from
// node.mesh.publicKey unless publicKey is non-empty.
func MemberRecordFromStatus(node map[string]any, peers map[string]any, publicKey string) MemberRecord {
	nodeRecord := decodeNodeRecord(node)
	record := MemberRecord{Recorded: strings.TrimSpace(nodeRecord.Mesh.Address), PublicKey: strings.TrimSpace(publicKey)}
	if record.PublicKey == "" {
		record.PublicKey = strings.TrimSpace(nodeRecord.Mesh.PublicKey)
	}
	if len(peers) > 0 {
		var document struct {
			Peers []RecordedPeer `json:"peers"`
		}
		if data, err := json.Marshal(peers); err == nil && json.Unmarshal(data, &document) == nil {
			record.PeerAddresses = document.Peers
		}
	}
	return record
}

type nodeRecord struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Mesh        struct {
		Address   string `json:"address"`
		PublicKey string `json:"publicKey"`
	} `json:"mesh"`
}

func decodeNodeRecord(node map[string]any) nodeRecord {
	var record nodeRecord
	if len(node) == 0 {
		return record
	}
	if data, err := json.Marshal(node); err == nil {
		_ = json.Unmarshal(data, &record)
	}
	return record
}

func parseMeshIPv4(value string) (uint32, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	ip := net.ParseIP(value)
	if ip == nil {
		parsed, _, err := net.ParseCIDR(value)
		if err != nil {
			return 0, false
		}
		ip = parsed
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(ip4), true
}

func ipString(ip uint32) string {
	out := make(net.IP, 4)
	binary.BigEndian.PutUint32(out, ip)
	return out.String()
}
