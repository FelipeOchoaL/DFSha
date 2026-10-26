package namenode

import "testing"

func TestPlaceSpreadsReplicasAndRacks(t *testing.T) {
	blocks := NewBlockMap()
	reg := NewRegistry(blocks, RegistryOptions{ReplicationFactor: 3})
	for _, n := range []PipelineNode{
		{ID: "dn1", Host: "h1", Port: 1, Rack: "A"},
		{ID: "dn2", Host: "h2", Port: 2, Rack: "A"},
		{ID: "dn3", Host: "h3", Port: 3, Rack: "B"},
		{ID: "dn4", Host: "h4", Port: 4, Rack: "B"},
	} {
		if _, err := reg.Register(n, 100); err != nil {
			t.Fatal(err)
		}
	}

	p0, err := reg.Place("/a.txt", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(p0) != 3 {
		t.Fatalf("pipeline len %d", len(p0))
	}
	seen := map[string]bool{}
	racks := map[string]bool{}
	for _, n := range p0 {
		if seen[n.ID] {
			t.Fatalf("duplicate node %s", n.ID)
		}
		seen[n.ID] = true
		racks[n.Rack] = true
	}
	if len(racks) < 2 {
		t.Fatalf("expected 2 racks, got %v", racks)
	}

	p1, err := reg.Place("/a.txt", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p1[0].ID == p0[0].ID {
		t.Fatalf("consecutive blocks started on the same primary %s", p0[0].ID)
	}
}

func TestPlaceFailsWithTooFewNodes(t *testing.T) {
	reg := NewRegistry(NewBlockMap(), RegistryOptions{})
	if _, err := reg.Register(PipelineNode{ID: "dn1", Host: "h", Port: 1, Rack: "A"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Place("/x", 0, 3); err == nil {
		t.Fatal("expected not enough datanodes")
	}
}
