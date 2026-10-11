package takod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/mesh"
)

// One WireGuard interface per node carries the mesh of every project and
// environment that deploys to it. Each (project, environment) stores its own
// peer set and takod renders the union, so a deploy of one environment never
// drops another environment's peers (D7 of scope-leases-and-mesh-to-environment).
//
// meshPeerSetsMu serializes storing, migrating, and rendering peer sets, and
// legacy mesh applies that are folded into them.
var meshPeerSetsMu sync.Mutex

const legacyPeerSetKey = "_legacy"

// MeshPeerSetRequest is PUT /v1/mesh/peer-sets.
type MeshPeerSetRequest struct {
	Project     string               `json:"project"`
	Environment string               `json:"environment"`
	NetworkCIDR string               `json:"networkCIDR"`
	SubnetBits  int                  `json:"subnetBits"`
	Config      mesh.WireGuardConfig `json:"config"`
	Node        mesh.Node            `json:"node"`
	Peers       []mesh.Node          `json:"peers,omitempty"`
}

// MeshPeerSet is one stored (project, environment) peer set.
type MeshPeerSet struct {
	MeshPeerSetRequest
	UpdatedAt time.Time `json:"updatedAt"`
}

// MeshPeerSetResponse reports the rendered union.
type MeshPeerSetResponse struct {
	Applied bool         `json:"applied"`
	Sets    int          `json:"sets"`
	Peers   int          `json:"peers"`
	Status  *mesh.Status `json:"status,omitempty"`
}

// MeshPeerSetSummary is one set as reported by GET /v1/status.
type MeshPeerSetSummary struct {
	Project     string      `json:"project"`
	Environment string      `json:"environment"`
	Node        mesh.Node   `json:"node"`
	Peers       []mesh.Node `json:"peers,omitempty"`
}

// MeshPeerSetConflictError is a union that cannot be rendered. Nothing was
// stored or applied.
type MeshPeerSetConflictError struct {
	Message string
}

func (e *MeshPeerSetConflictError) Error() string { return e.Message }

// staleSetHint names the remedy for a set left behind by an environment that
// no longer uses this server: deploys only contact members, so only an
// explicit prune removes it.
const staleSetHint = ", or, if that environment no longer lists this server, run 'tako mesh prune -e <environment>' in its project"

func meshPeerSetsDir(dataDir string) string {
	return filepath.Join(dataDir, "mesh", "peer-sets")
}

func meshPeerSetPath(dataDir, project, environment string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data directory is required")
	}
	if project == legacyPeerSetKey && environment == legacyPeerSetKey {
		return filepath.Join(meshPeerSetsDir(dataDir), legacyPeerSetKey+".json"), nil
	}
	if !isSafeProjectName(project) {
		return "", fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(environment) {
		return "", fmt.Errorf("invalid environment name")
	}
	return filepath.Join(meshPeerSetsDir(dataDir), project, environment+".json"), nil
}

func setKey(project, environment string) string {
	return project + "/" + environment
}

func describeSet(set MeshPeerSet) string {
	if set.Project == legacyPeerSetKey {
		return "an untagged legacy mesh"
	}
	return set.Project + "/" + set.Environment
}

// loadMeshPeerSetsLocked returns every stored set keyed by project/env.
func loadMeshPeerSetsLocked(dataDir string) (map[string]MeshPeerSet, error) {
	sets := map[string]MeshPeerSet{}
	files, err := filepath.Glob(filepath.Join(meshPeerSetsDir(dataDir), "*", "*.json"))
	if err != nil {
		return nil, err
	}
	legacy := filepath.Join(meshPeerSetsDir(dataDir), legacyPeerSetKey+".json")
	if _, err := os.Stat(legacy); err == nil {
		files = append(files, legacy)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read mesh peer set %s: %w", file, err)
		}
		var set MeshPeerSet
		if err := json.Unmarshal(data, &set); err != nil {
			return nil, fmt.Errorf("parse mesh peer set %s: %w", file, err)
		}
		sets[setKey(set.Project, set.Environment)] = set
	}
	return sets, nil
}

func writeMeshPeerSetLocked(dataDir string, set MeshPeerSet) error {
	path, err := meshPeerSetPath(dataDir, set.Project, set.Environment)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0600)
}

func removeMeshPeerSetLocked(dataDir, project, environment string) error {
	path, err := meshPeerSetPath(dataDir, project, environment)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// legacyPeersDocument is mesh/peers.json as written by PUT /v1/metadata.
type legacyPeersDocument struct {
	Project     string      `json:"project"`
	Environment string      `json:"environment"`
	NetworkCIDR string      `json:"networkCIDR"`
	Interface   string      `json:"interface"`
	ListenPort  int         `json:"listenPort"`
	Peers       []mesh.Node `json:"peers"`
}

type legacyNodeDocument struct {
	Node string `json:"node"`
	Host string `json:"host"`
	Mesh struct {
		Address      string `json:"address"`
		SubnetBits   int    `json:"subnetBits"`
		NATTraversal bool   `json:"natTraversal"`
		PublicKey    string `json:"publicKey"`
		NetworkCIDR  string `json:"networkCIDR"`
		Interface    string `json:"interface"`
		ListenPort   int    `json:"listenPort"`
	} `json:"mesh"`
}

func readLegacyMeshDocuments(dataDir string) (*legacyPeersDocument, *legacyNodeDocument) {
	var peers *legacyPeersDocument
	if data, err := os.ReadFile(filepath.Join(dataDir, "mesh", "peers.json")); err == nil {
		var doc legacyPeersDocument
		if json.Unmarshal(data, &doc) == nil {
			peers = &doc
		}
	}
	var node *legacyNodeDocument
	if data, err := os.ReadFile(filepath.Join(dataDir, "node.json")); err == nil {
		var doc legacyNodeDocument
		if json.Unmarshal(data, &doc) == nil {
			node = &doc
		}
	}
	return peers, node
}

// migrateLegacyMeshLocked converts mesh/peers.json into a peer set the first
// time this takod handles peer sets: a tagged file becomes the set it names,
// an untagged one becomes the legacy set. It runs once, marked by the
// peer-sets directory.
func migrateLegacyMeshLocked(dataDir string) error {
	dir := meshPeerSetsDir(dataDir)
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	peers, node := readLegacyMeshDocuments(dataDir)
	if peers == nil || node == nil || len(peers.Peers) == 0 || strings.TrimSpace(node.Mesh.Address) == "" {
		return nil
	}
	set := legacySetFromDocuments(peers, node)
	set.Peers = peers.Peers
	set.UpdatedAt = time.Now().UTC()
	return writeMeshPeerSetLocked(dataDir, set)
}

func legacySetFromDocuments(peers *legacyPeersDocument, node *legacyNodeDocument) MeshPeerSet {
	set := MeshPeerSet{}
	set.Project, set.Environment = legacyPeerSetKey, legacyPeerSetKey
	if peers != nil && isSafeProjectName(peers.Project) && isSafeRuntimeName(peers.Environment) {
		set.Project, set.Environment = peers.Project, peers.Environment
	}
	if peers != nil {
		set.NetworkCIDR = peers.NetworkCIDR
		set.Config.Interface = peers.Interface
		set.Config.ListenPort = peers.ListenPort
	}
	if node != nil {
		if set.NetworkCIDR == "" {
			set.NetworkCIDR = node.Mesh.NetworkCIDR
		}
		if set.Config.Interface == "" {
			set.Config.Interface = node.Mesh.Interface
		}
		if set.Config.ListenPort == 0 {
			set.Config.ListenPort = node.Mesh.ListenPort
		}
		set.SubnetBits = node.Mesh.SubnetBits
		set.Config.NATTraversal = node.Mesh.NATTraversal
		set.Node = mesh.Node{Name: node.Node, Host: node.Host, Address: node.Mesh.Address, PublicKey: node.Mesh.PublicKey}
	}
	set.Config.Enabled = true
	return set
}

// meshUnion is the rendered interface for a set of peer sets.
type meshUnion struct {
	node   mesh.Node
	config mesh.WireGuardConfig
	peers  []mesh.Node
}

type unionPeer struct {
	peer  mesh.Node
	owner MeshPeerSet
}

func meshIP(address string) string {
	address = strings.TrimSpace(address)
	if ip, _, err := net.ParseCIDR(address); err == nil {
		return ip.String()
	}
	if ip := net.ParseIP(address); ip != nil {
		return ip.String()
	}
	return address
}

func isSelfPeer(set MeshPeerSet, peer mesh.Node) bool {
	if key := strings.TrimSpace(set.Node.PublicKey); key != "" && strings.TrimSpace(peer.PublicKey) == key {
		return true
	}
	return peer.Name == set.Node.Name && meshIP(peer.Address) == meshIP(set.Node.Address)
}

func otherPeers(set MeshPeerSet) []mesh.Node {
	var peers []mesh.Node
	for _, peer := range set.Peers {
		if !isSelfPeer(set, peer) {
			peers = append(peers, peer)
		}
	}
	return peers
}

// renderMeshUnion applies the D7 union rules:
//   - peers are deduplicated by public key;
//   - one address held by two keys, or one key holding two addresses, is a
//     conflict;
//   - sets with peers other than the node itself must agree on networkCIDR,
//     subnetBits, interface, listenPort and the node's own address;
//   - sets without other peers add no peers and impose no interface settings;
//     when no set has peers, latest (the most recently written set) decides.
func renderMeshUnion(sets map[string]MeshPeerSet, latest string) (*meshUnion, error) {
	keys := make([]string, 0, len(sets))
	for key := range sets {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var multi []MeshPeerSet
	for _, key := range keys {
		if len(otherPeers(sets[key])) > 0 {
			multi = append(multi, sets[key])
		}
	}
	base, ok := sets[latest]
	if len(multi) > 0 {
		base = multi[0]
		for _, set := range multi[1:] {
			if set.NetworkCIDR != base.NetworkCIDR || set.SubnetBits != base.SubnetBits || set.Config.Interface != base.Config.Interface || set.Config.ListenPort != base.Config.ListenPort {
				return nil, &MeshPeerSetConflictError{Message: fmt.Sprintf("mesh settings of %s (%s/%d, %s:%d) differ from %s (%s/%d, %s:%d) on this shared node; multi-node environments that share a server need the same mesh: block", describeSet(set), set.NetworkCIDR, set.SubnetBits, set.Config.Interface, set.Config.ListenPort, describeSet(base), base.NetworkCIDR, base.SubnetBits, base.Config.Interface, base.Config.ListenPort)}
			}
			if meshIP(set.Node.Address) != meshIP(base.Node.Address) {
				return nil, &MeshPeerSetConflictError{Message: fmt.Sprintf("%s addresses this node (%s) as %s but %s addresses it as %s; redeploy the environment that owns the stale address"+staleSetHint, describeSet(set), set.Node.Name, meshIP(set.Node.Address), describeSet(base), meshIP(base.Node.Address))}
			}
		}
	} else if !ok {
		return nil, nil
	}

	selfKey := strings.TrimSpace(base.Node.PublicKey)
	byKey := map[string]unionPeer{}
	byAddress := map[string]unionPeer{}
	selfAddress := meshIP(base.Node.Address)
	var ordered []string
	for _, set := range multi {
		for _, peer := range otherPeers(set) {
			key := strings.TrimSpace(peer.PublicKey)
			if key == "" {
				return nil, &MeshPeerSetConflictError{Message: fmt.Sprintf("peer %s of %s has no public key", peer.Name, describeSet(set))}
			}
			if selfKey != "" && key == selfKey {
				continue
			}
			address := meshIP(peer.Address)
			if address == selfAddress {
				return nil, &MeshPeerSetConflictError{Message: fmt.Sprintf("mesh address %s of peer %s (%s) is this node's own address (%s); redeploy the environment that owns the stale address"+staleSetHint, address, peer.Name, describeSet(set), base.Node.Name)}
			}
			if existing, seen := byKey[key]; seen {
				if meshIP(existing.peer.Address) != address {
					return nil, &MeshPeerSetConflictError{Message: fmt.Sprintf("WireGuard key of %s (%s) is at %s, but %s (%s) lists it at %s; redeploy the environment that owns the stale address"+staleSetHint, existing.peer.Name, describeSet(existing.owner), meshIP(existing.peer.Address), peer.Name, describeSet(set), address)}
				}
				continue
			}
			if existing, taken := byAddress[address]; taken {
				return nil, &MeshPeerSetConflictError{Message: fmt.Sprintf("mesh address %s is held by %s (%s) and %s (%s) with different keys; redeploy the environment that owns the stale address"+staleSetHint, address, existing.peer.Name, describeSet(existing.owner), peer.Name, describeSet(set))}
			}
			entry := unionPeer{peer: peer, owner: set}
			byKey[key] = entry
			byAddress[address] = entry
			ordered = append(ordered, key)
		}
	}

	// Peer names are comments plus the self filter of the renderer. Names
	// from different projects can collide with each other or with this
	// node's name, so a colliding peer is qualified by its set.
	names := map[string]int{base.Node.Name: 1}
	for _, key := range ordered {
		names[byKey[key].peer.Name]++
	}
	peers := make([]mesh.Node, 0, len(ordered))
	for _, key := range ordered {
		entry := byKey[key]
		peer := entry.peer
		if names[peer.Name] > 1 || peer.Name == base.Node.Name {
			peer.Name = entry.owner.Project + "." + entry.owner.Environment + "." + peer.Name
		}
		peers = append(peers, peer)
	}
	config := base.Config
	config.Enabled = true
	return &meshUnion{node: base.Node, config: config, peers: peers}, nil
}

func validateMeshPeerSetRequest(req MeshPeerSetRequest) error {
	if !isSafeProjectName(req.Project) {
		return fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return fmt.Errorf("invalid environment name")
	}
	if _, _, err := net.ParseCIDR(strings.TrimSpace(req.NetworkCIDR)); err != nil {
		return fmt.Errorf("invalid mesh networkCIDR: %w", err)
	}
	if req.SubnetBits <= 0 || req.SubnetBits > 32 {
		return fmt.Errorf("invalid mesh subnetBits")
	}
	req.Config.Enabled = true
	return validateMeshApplyRequest(MeshApplyRequest{Config: req.Config, Node: req.Node, Peers: req.Peers})
}

// PutMeshPeerSet stores one environment's peer set and renders the union. A
// conflicting union stores nothing and leaves the interface unchanged.
func PutMeshPeerSet(ctx context.Context, dataDir string, req MeshPeerSetRequest) (*MeshPeerSetResponse, error) {
	if err := validateMeshPeerSetRequest(req); err != nil {
		return nil, err
	}
	meshPeerSetsMu.Lock()
	defer meshPeerSetsMu.Unlock()
	if err := migrateLegacyMeshLocked(dataDir); err != nil {
		return nil, fmt.Errorf("migrate legacy mesh peers: %w", err)
	}
	sets, err := loadMeshPeerSetsLocked(dataDir)
	if err != nil {
		return nil, err
	}
	// The untagged legacy set is dropped on the first peer-set write.
	delete(sets, setKey(legacyPeerSetKey, legacyPeerSetKey))
	set := MeshPeerSet{MeshPeerSetRequest: req, UpdatedAt: time.Now().UTC()}
	set.Config.Enabled = true
	return storeAndRenderLocked(ctx, dataDir, sets, set, true)
}

func storeAndRenderLocked(ctx context.Context, dataDir string, sets map[string]MeshPeerSet, set MeshPeerSet, dropLegacy bool) (*MeshPeerSetResponse, error) {
	key := setKey(set.Project, set.Environment)
	previous, hadPrevious := sets[key]
	sets[key] = set
	union, err := renderMeshUnion(sets, key)
	if err != nil {
		return nil, err
	}
	if err := writeMeshPeerSetLocked(dataDir, set); err != nil {
		return nil, fmt.Errorf("store mesh peer set: %w", err)
	}
	status, err := applyMeshConfig(ctx, union.node, union.peers, union.config, false)
	if err != nil {
		// Keep the stored sets equal to what the interface renders.
		if hadPrevious {
			_ = writeMeshPeerSetLocked(dataDir, previous)
		} else {
			_ = removeMeshPeerSetLocked(dataDir, set.Project, set.Environment)
		}
		return nil, err
	}
	if dropLegacy {
		_ = removeMeshPeerSetLocked(dataDir, legacyPeerSetKey, legacyPeerSetKey)
	}
	return &MeshPeerSetResponse{Applied: true, Sets: len(sets), Peers: len(union.peers), Status: status}, nil
}

// DeleteMeshPeerSet removes one environment's set and re-renders the union of
// the remaining sets. Removing the last set leaves the interface as it is.
func DeleteMeshPeerSet(ctx context.Context, dataDir, project, environment string) (*MeshPeerSetResponse, error) {
	if _, err := meshPeerSetPath(dataDir, project, environment); err != nil {
		return nil, err
	}
	meshPeerSetsMu.Lock()
	defer meshPeerSetsMu.Unlock()
	if err := migrateLegacyMeshLocked(dataDir); err != nil {
		return nil, fmt.Errorf("migrate legacy mesh peers: %w", err)
	}
	sets, err := loadMeshPeerSetsLocked(dataDir)
	if err != nil {
		return nil, err
	}
	key := setKey(project, environment)
	removed, existed := sets[key]
	if !existed {
		return &MeshPeerSetResponse{Applied: false, Sets: len(sets)}, nil
	}
	delete(sets, key)
	latest := ""
	for candidate, set := range sets {
		if latest == "" || set.UpdatedAt.After(sets[latest].UpdatedAt) {
			latest = candidate
		}
	}
	union, err := renderMeshUnion(sets, latest)
	if err != nil {
		return nil, err
	}
	if err := removeMeshPeerSetLocked(dataDir, project, environment); err != nil {
		return nil, fmt.Errorf("remove mesh peer set: %w", err)
	}
	if union == nil {
		return &MeshPeerSetResponse{Applied: false, Sets: 0}, nil
	}
	status, err := applyMeshConfig(ctx, union.node, union.peers, union.config, false)
	if err != nil {
		_ = writeMeshPeerSetLocked(dataDir, removed)
		return nil, err
	}
	return &MeshPeerSetResponse{Applied: true, Sets: len(sets), Peers: len(union.peers), Status: status}, nil
}

// Legacy metadata tags. An older CLI sends PUT /v1/metadata and then POST
// /v1/mesh/apply with the same peer list. Another environment's metadata
// write can land between the two, so the apply cannot rely on the tags of
// whatever peers.json holds by then. WriteMetadata remembers the tags of
// every peers document under meshPeerSetsMu, keyed by the data directory and
// the peer list, and the apply looks its own peer list up.
const maxLegacyMetadataTags = 64

var (
	legacyMetadataTags  = map[string]legacyPeersDocument{}
	legacyMetadataOrder []string
)

func legacyPeersFingerprint(dataDir string, peers []mesh.Node) string {
	entries := make([]string, 0, len(peers))
	for _, peer := range peers {
		entries = append(entries, strings.TrimSpace(peer.PublicKey)+"@"+meshIP(peer.Address))
	}
	sort.Strings(entries)
	return dataDir + "\x00" + strings.Join(entries, ",")
}

// rememberLegacyMetadataLocked records the tags of a peers document written
// through PUT /v1/metadata. Untagged or unparsable documents are ignored.
func rememberLegacyMetadataLocked(dataDir string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	var document legacyPeersDocument
	if json.Unmarshal(data, &document) != nil || !isSafeProjectName(document.Project) || !isSafeRuntimeName(document.Environment) {
		return
	}
	key := legacyPeersFingerprint(dataDir, document.Peers)
	document.Peers = nil
	if _, seen := legacyMetadataTags[key]; !seen {
		legacyMetadataOrder = append(legacyMetadataOrder, key)
		if len(legacyMetadataOrder) > maxLegacyMetadataTags {
			delete(legacyMetadataTags, legacyMetadataOrder[0])
			legacyMetadataOrder = legacyMetadataOrder[1:]
		}
	}
	legacyMetadataTags[key] = document
}

// legacyApplyTagsLocked returns the peers document whose tags name the set a
// legacy mesh apply belongs to: the metadata write with the same peer list,
// else peers.json when it still lists exactly these peers, else nil (an
// untagged legacy set).
func legacyApplyTagsLocked(dataDir string, onDisk *legacyPeersDocument, peers []mesh.Node) *legacyPeersDocument {
	key := legacyPeersFingerprint(dataDir, peers)
	if document, ok := legacyMetadataTags[key]; ok {
		return &document
	}
	if onDisk != nil && legacyPeersFingerprint(dataDir, onDisk.Peers) == key {
		return onDisk
	}
	return nil
}

// reconcileLegacyMeshApply handles POST /v1/mesh/apply from a CLI that
// predates peer sets on a takod that has them. The request is stored as the
// set named by the tags of the metadata the same CLI wrote with the same
// peer list through PUT /v1/metadata, and the union is rendered under the
// usual checks.
func reconcileLegacyMeshApply(ctx context.Context, dataDir string, request MeshApplyRequest) (*MeshApplyResponse, error) {
	meshPeerSetsMu.Lock()
	defer meshPeerSetsMu.Unlock()
	if err := migrateLegacyMeshLocked(dataDir); err != nil {
		return nil, fmt.Errorf("migrate legacy mesh peers: %w", err)
	}
	sets, err := loadMeshPeerSetsLocked(dataDir)
	if err != nil {
		return nil, err
	}
	onDisk, node := readLegacyMeshDocuments(dataDir)
	set := legacySetFromDocuments(legacyApplyTagsLocked(dataDir, onDisk, request.Peers), node)
	set.Config = request.Config
	set.Config.Enabled = true
	set.Node = request.Node
	set.Peers = request.Peers
	// node.json may already belong to another environment; the request's
	// own address carries this set's network and prefix.
	if network := networkFromAddress(request.Node.Address); network != "" && set.NetworkCIDR == "" {
		set.NetworkCIDR = network
	}
	if prefix := prefixFromAddress(request.Node.Address); prefix > 0 {
		set.SubnetBits = prefix
	}
	set.UpdatedAt = time.Now().UTC()
	response, err := storeAndRenderLocked(ctx, dataDir, sets, set, false)
	if err != nil {
		return nil, err
	}
	return &MeshApplyResponse{Applied: response.Applied, Status: response.Status}, nil
}

func networkFromAddress(address string) string {
	if _, network, err := net.ParseCIDR(strings.TrimSpace(address)); err == nil {
		return network.String()
	}
	return ""
}

func prefixFromAddress(address string) int {
	if _, network, err := net.ParseCIDR(strings.TrimSpace(address)); err == nil {
		ones, _ := network.Mask.Size()
		return ones
	}
	return 0
}

// meshPeerSetSummaries reports the stored sets for GET /v1/status, or nil
// before the first peer-set write.
func meshPeerSetSummaries(dataDir string) []MeshPeerSetSummary {
	meshPeerSetsMu.Lock()
	defer meshPeerSetsMu.Unlock()
	if _, err := os.Stat(meshPeerSetsDir(dataDir)); err != nil {
		return nil
	}
	sets, err := loadMeshPeerSetsLocked(dataDir)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(sets))
	for key := range sets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	summaries := make([]MeshPeerSetSummary, 0, len(keys))
	for _, key := range keys {
		set := sets[key]
		summaries = append(summaries, MeshPeerSetSummary{Project: set.Project, Environment: set.Environment, Node: set.Node, Peers: set.Peers})
	}
	return summaries
}

func (s *Server) handleMeshPeerSets(w http.ResponseWriter, r *http.Request) {
	if s.installation != nil {
		http.Error(w, "enrolled-node mesh topology is owned by platform membership", http.StatusForbidden)
		return
	}
	var (
		response *MeshPeerSetResponse
		err      error
	)
	switch r.Method {
	case http.MethodPut:
		if r.URL.Path != "/v1/mesh/peer-sets" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer r.Body.Close()
		var request MeshPeerSetRequest
		if decodeErr := decodeJSONRequest(w, r, &request); decodeErr != nil {
			http.Error(w, "invalid JSON body: "+decodeErr.Error(), http.StatusBadRequest)
			return
		}
		if validateErr := validateMeshPeerSetRequest(request); validateErr != nil {
			http.Error(w, validateErr.Error(), http.StatusBadRequest)
			return
		}
		if !s.requireFreeDisk(w, "/etc/wireguard") {
			return
		}
		response, err = PutMeshPeerSet(r.Context(), s.dataDir, request)
	case http.MethodDelete:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/mesh/peer-sets/"), "/")
		if len(parts) != 2 {
			http.Error(w, "expected /v1/mesh/peer-sets/<project>/<environment>", http.StatusBadRequest)
			return
		}
		response, err = DeleteMeshPeerSet(r.Context(), s.dataDir, parts[0], parts[1])
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		status := http.StatusBadGateway
		var conflict *MeshPeerSetConflictError
		if errors.As(err, &conflict) {
			status = http.StatusConflict
		} else if strings.HasPrefix(err.Error(), "invalid ") {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}
