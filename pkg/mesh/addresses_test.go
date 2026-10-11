package mesh

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveAddresses(t *testing.T) {
	tests := []struct {
		name     string
		cidr     string
		members  []string
		records  map[string]MemberRecord
		want     map[string]string
		changed  []string
		warnings []string
	}{
		{
			name:    "fresh environment gets lowest addresses in sorted order",
			cidr:    "10.210.0.0/16",
			members: []string{"c", "a", "b"},
			want:    map[string]string{"a": "10.210.0.1/24", "b": "10.210.0.2/24", "c": "10.210.0.3/24"},
			changed: []string{"a", "b", "c"},
		},
		{
			name:    "recorded addresses are kept and never renumbered",
			cidr:    "10.210.0.0/16",
			members: []string{"a", "b", "c"},
			records: map[string]MemberRecord{
				"a": {Recorded: "10.210.0.3/24"},
				"b": {Recorded: "10.210.0.1/24"},
				"c": {Recorded: "10.210.0.2/24"},
			},
			want: map[string]string{"a": "10.210.0.3/24", "b": "10.210.0.1/24", "c": "10.210.0.2/24"},
		},
		{
			name:    "new member takes the lowest free address around kept ones",
			cidr:    "10.210.0.0/16",
			members: []string{"a", "b", "new"},
			records: map[string]MemberRecord{
				"a": {Recorded: "10.210.0.1/24"},
				"b": {Recorded: "10.210.0.3/24"},
			},
			want:    map[string]string{"a": "10.210.0.1/24", "b": "10.210.0.3/24", "new": "10.210.0.2/24"},
			changed: []string{"new"},
		},
		{
			name:    "duplicate record is repaired deterministically with a warning",
			cidr:    "10.210.0.0/16",
			members: []string{"b", "a"},
			records: map[string]MemberRecord{
				"a": {Recorded: "10.210.0.1/24"},
				"b": {Recorded: "10.210.0.1/24"},
			},
			want:     map[string]string{"a": "10.210.0.1/24", "b": "10.210.0.2/24"},
			changed:  []string{"b"},
			warnings: []string{"mesh address 10.210.0.1 is recorded by both a and b; a keeps it and b gets a new address"},
		},
		{
			name:    "record outside a changed CIDR is reassigned",
			cidr:    "10.220.0.0/16",
			members: []string{"a", "b"},
			records: map[string]MemberRecord{
				"a": {Recorded: "10.210.0.1/24"},
				"b": {Recorded: "10.220.0.5/24"},
			},
			want:    map[string]string{"a": "10.220.0.1/24", "b": "10.220.0.5/24"},
			changed: []string{"a"},
		},
		{
			name:    "addresses of foreign peers on a shared node are avoided",
			cidr:    "10.210.0.0/16",
			members: []string{"a", "b"},
			records: map[string]MemberRecord{
				"a": {PublicKey: "key-a", PeerAddresses: []RecordedPeer{
					{Name: "other-1", Address: "10.210.0.1/24", PublicKey: "foreign-1"},
					{Name: "a", Address: "10.210.0.2/24", PublicKey: "key-a"},
				}},
				"b": {PublicKey: "key-b"},
			},
			want:    map[string]string{"a": "10.210.0.2/24", "b": "10.210.0.3/24"},
			changed: []string{"a", "b"},
		},
		{
			name:    "platform address wins over a record",
			cidr:    "10.210.0.0/16",
			members: []string{"a", "b"},
			records: map[string]MemberRecord{
				"a": {PlatformAddress: "10.210.0.2", Recorded: "10.210.0.1/24"},
				"b": {Recorded: "10.210.0.2/24"},
			},
			want:     map[string]string{"a": "10.210.0.2/24", "b": "10.210.0.1/24"},
			changed:  []string{"a", "b"},
			warnings: []string{"mesh address 10.210.0.2 is recorded by both a and b; a keeps it and b gets a new address"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveAddresses(test.cidr, 24, test.members, test.records)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Addresses, test.want) {
				t.Fatalf("addresses = %v, want %v", got.Addresses, test.want)
			}
			if !reflect.DeepEqual(got.Changed, test.changed) {
				t.Fatalf("changed = %v, want %v", got.Changed, test.changed)
			}
			if !reflect.DeepEqual(got.Warnings, test.warnings) {
				t.Fatalf("warnings = %v, want %v", got.Warnings, test.warnings)
			}
		})
	}
}

// Pruned or out-of-environment servers are not members, so they cannot shift
// anyone's address: resolution over the same members and records is
// identical however the config was loaded.
func TestResolveAddressesIgnoresNonMembers(t *testing.T) {
	records := map[string]MemberRecord{"a": {Recorded: "10.210.0.4/24"}}
	withExtra := map[string]MemberRecord{"a": {Recorded: "10.210.0.4/24"}, "pruned": {Recorded: "10.210.0.1/24"}}
	first, err := ResolveAddresses("10.210.0.0/16", 24, []string{"a", "b"}, records)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveAddresses("10.210.0.0/16", 24, []string{"b", "a"}, withExtra)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Addresses, second.Addresses) {
		t.Fatalf("addresses differ: %v vs %v", first.Addresses, second.Addresses)
	}
	if ip, _ := first.IP("b"); ip != "10.210.0.1" {
		t.Fatalf("b = %s, want 10.210.0.1", ip)
	}
}

func TestResolveAddressesRejectsInvalidCIDR(t *testing.T) {
	if _, err := ResolveAddresses("not-a-cidr", 24, []string{"a"}, nil); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
	if _, err := ResolveAddresses("10.0.0.0/31", 24, []string{"a"}, nil); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("tiny CIDR err = %v", err)
	}
}

func TestMemberRecordFromStatus(t *testing.T) {
	node := map[string]any{"project": "demo", "environment": "production", "mesh": map[string]any{"address": "10.210.0.5/24", "publicKey": "node-key"}}
	peers := map[string]any{"peers": []any{map[string]any{"name": "x", "address": "10.210.0.9/24", "publicKey": "peer-key"}}}
	record := MemberRecordFromStatus(node, peers, "")
	if record.Recorded != "10.210.0.5/24" || record.PublicKey != "node-key" || len(record.PeerAddresses) != 1 || record.PeerAddresses[0].Address != "10.210.0.9/24" {
		t.Fatalf("record = %+v", record)
	}
	if project, env := RecordedNodeOwner(node); project != "demo" || env != "production" {
		t.Fatalf("owner = %s/%s", project, env)
	}
	if RecordedNodeAddress(nil) != "" {
		t.Fatal("empty node produced an address")
	}
}
