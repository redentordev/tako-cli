package reconcile

import (
	"slices"
	"testing"
)

func TestAggregateActualStateByServerCombinesReplicas(t *testing.T) {
	nodeAWeb := &ActualService{
		Name:       "web",
		Image:      "demo/web:1",
		Replicas:   1,
		Containers: []string{"a1"},
	}
	actualByServer := map[string]map[string]*ActualService{
		"node-a": {
			"web": nodeAWeb,
		},
		"node-b": {
			"web": {
				Name:       "web",
				Image:      "demo/web:1",
				Replicas:   2,
				Containers: []string{"b1", "b2"},
			},
			"worker": {
				Name:       "worker",
				Image:      "demo/worker:1",
				Replicas:   1,
				Containers: []string{"w1"},
			},
		},
	}

	aggregate := AggregateActualStateByServer(actualByServer)

	if got := aggregate["web"].Replicas; got != 3 {
		t.Fatalf("web replicas = %d, want 3", got)
	}
	if !slices.Equal(aggregate["web"].Containers, []string{"a1", "b1", "b2"}) {
		t.Fatalf("unexpected web containers: %#v", aggregate["web"].Containers)
	}
	if got := aggregate["worker"].Replicas; got != 1 {
		t.Fatalf("worker replicas = %d, want 1", got)
	}

	aggregate["web"].Containers[0] = "mutated"
	if nodeAWeb.Containers[0] != "a1" {
		t.Fatalf("aggregate aliased node state: %#v", nodeAWeb.Containers)
	}
}
