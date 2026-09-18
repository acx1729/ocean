package loro

import (
	"fmt"
	"reflect"
	"testing"
)

func TestConcurrentMovesConverge(t *testing.T) {
	a := newDoc(t, 1)
	ids := page(t, a, "blk-root", "blk-x", "blk-y")
	mustApply(t, a,
		TreeCreate("blk-x1", ids["blk-x"], -1, &TreeCreateInit{Content: "blk-x1"}),
		TreeCreate("blk-y1", ids["blk-y"], -1, &TreeCreateInit{Content: "blk-y1"}),
	)
	b := clone(t, a, 2)

	mustApply(t, a, TreeMove(ids["blk-x"], ids["blk-y"], -1))
	mustApply(t, b, TreeMove(ids["blk-y"], ids["blk-x"], -1))
	exchange(t, a, b)

	sa, sb := mustState(t, a), mustState(t, b)
	if shape(sa) != shape(sb) {
		t.Fatalf("states differ:\n%s---\n%s", shape(sa), shape(sb))
	}
	seen := map[*Node]bool{}
	sa.Walk(func(n *Node, depth int) bool {
		if seen[n] {
			t.Fatalf("block %s visited twice", n.ID)
		}
		seen[n] = true
		if depth >= len(sa.ByTreeID) {
			t.Fatalf("depth %d at %s exceeds the node count: cycle", depth, n.ID)
		}
		return true
	})
	if len(seen) != 5 || len(sa.ByTreeID) != 5 {
		t.Errorf("visited %d blocks, indexed %d, want 5", len(seen), len(sa.ByTreeID))
	}
	x, y := sa.ByID["blk-x"], sa.ByID["blk-y"]
	xUnderY, yUnderX := x.Parent == y, y.Parent == x
	if xUnderY == yUnderX {
		t.Errorf("exactly one move must win: x under y = %v, y under x = %v", xUnderY, yUnderX)
	}
	for _, n := range sa.ByTreeID {
		if len(n.Path()) >= len(sa.ByTreeID) {
			t.Errorf("path of %s is too long: %v", n.ID, n.Path())
		}
	}
}

func TestConcurrentTextEditsMerge(t *testing.T) {
	a := newDoc(t, 1)
	ids := page(t, a, "blk-root", "blk-1")
	n := ids["blk-1"]
	mustApply(t, a, NodeText(n, "hello world"))
	b := clone(t, a, 2)

	mustApply(t, a, NodeText(n, "hello brave world"))
	mustApply(t, b, NodeText(n, "hello world!"))
	exchange(t, a, b)

	ca, cb := mustState(t, a).ByID["blk-1"].Content, mustState(t, b).ByID["blk-1"].Content
	if ca != cb {
		t.Errorf("contents differ: %q vs %q", ca, cb)
	}
	if ca != "hello brave world!" {
		t.Errorf("merged content = %q, want both insertions", ca)
	}
}

func TestConcurrentPropMapSetsMerge(t *testing.T) {
	a := newDoc(t, 1)
	ids := page(t, a, "blk-root", "blk-1")
	n := ids["blk-1"]
	mustApply(t, a, NodePropMapSet(n, "rel", "t0", true))
	b := clone(t, a, 2)

	mustApply(t, a, NodePropMapSet(n, "rel", "t1", true))
	mustApply(t, b, NodePropMapSet(n, "rel", "t2", true))
	exchange(t, a, b)
	want := map[string]any{"t0": true, "t1": true, "t2": true}
	for name, d := range map[string]*Doc{"a": a, "b": b} {
		if got := mustState(t, d).ByID["blk-1"].Props["rel"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s rel = %v, want %v", name, got, want)
		}
	}

	res := mustApply(t, a, NodePropMapDel(n, "rel", "t0"))
	if !res.Changed {
		t.Error("NodePropMapDel of an existing member produced no operation")
	}
	exchange(t, a, b)
	want = map[string]any{"t1": true, "t2": true}
	for name, d := range map[string]*Doc{"a": a, "b": b} {
		if got := mustState(t, d).ByID["blk-1"].Props["rel"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s rel after delete = %v, want %v", name, got, want)
		}
	}
}

func TestSiblingOrdering(t *testing.T) {
	d := newDoc(t, 1)
	ids := page(t, d, "blk-root", "c0", "c1", "c2", "c3", "c4")
	root := ids["blk-root"]
	mustApply(t, d, TreeMove(ids["c3"], root, 0), TreeMove(ids["c1"], root, -1))

	s := mustState(t, d)
	if got, want := childIDs(s.Roots[0]), []string{"c3", "c0", "c2", "c4", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("children = %v, want %v", got, want)
	}
	for i, c := range s.Roots[0].Children {
		if c.Index != i {
			t.Errorf("%s Index = %d, want %d", c.ID, c.Index, i)
		}
		if c.FractionalIndex == "" {
			t.Errorf("%s has no fractional index", c.ID)
		}
		if i > 0 && s.Roots[0].Children[i-1].FractionalIndex >= c.FractionalIndex {
			t.Errorf("fractional index not increasing at %d: %q >= %q", i, s.Roots[0].Children[i-1].FractionalIndex, c.FractionalIndex)
		}
	}

	if res := mustApply(t, d, TreeMove(ids["c0"], root, 1)); res.Changed {
		t.Error("moving a block to its current position produced an operation")
	}
	// Within one parent the last valid index is len-1; a create accepts len.
	mustApply(t, d, TreeMove(ids["c3"], root, 4))
	mustApply(t, d, TreeCreate("c5", root, 5, &TreeCreateInit{Content: "c5"}))
	if got, want := childIDs(mustState(t, d).Roots[0]), []string{"c0", "c2", "c4", "c1", "c3", "c5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("children = %v, want %v", got, want)
	}
}

func TestPlaceholderReferences(t *testing.T) {
	d := newDoc(t, 1)
	res := mustApply(t, d,
		TreeCreate("u1", "", -1, nil),
		NodeText(Placeholder("u1"), "text of u1"),
		NodePropSet(Placeholder("u1"), "k", "v"),
		TreeCreate("u2", Placeholder("u1"), 0, &TreeCreateInit{Content: "u2"}),
		TreeCreate("u3", Placeholder("u1"), 0, &TreeCreateInit{Content: "u3"}),
		TreeMove(Placeholder("u2"), "", -1),
		NodeSet(Placeholder("u3"), "type_id", "type-z"),
	)
	if len(res.Created) != 3 {
		t.Fatalf("Created = %v, want 3 entries", res.Created)
	}
	s := mustState(t, d)
	const want = "u1 \"text of u1\"\n  u3 \"u3\"\nu2 \"u2\"\n"
	if got := shape(s); got != want {
		t.Errorf("shape:\n%s\nwant:\n%s", got, want)
	}
	if got := s.ByID["u1"].Props["k"]; got != "v" {
		t.Errorf("u1 prop k = %v, want v", got)
	}
	if got := s.ByID["u3"].TypeID; got != "type-z" {
		t.Errorf("u3 type = %q, want type-z", got)
	}
	for id, tid := range res.Created {
		if s.ByID[id] == nil || s.ByID[id].TreeID != tid {
			t.Errorf("Created[%s] = %s does not match the state", id, tid)
		}
	}
}

func TestDeleteRemovesSubtree(t *testing.T) {
	d := newDoc(t, 1)
	ids := page(t, d, "blk-root", "c0", "c1")
	g0 := mustApply(t, d, TreeCreate("g0", ids["c0"], -1, &TreeCreateInit{Content: "g0"})).Created["g0"]

	res := mustApply(t, d, TreeDelete(ids["c0"]))
	if !res.Changed {
		t.Error("delete produced no operation")
	}
	s := mustState(t, d)
	if got, want := shape(s), "blk-root \"blk-root\"\n  c1 \"c1\"\n"; got != want {
		t.Errorf("shape after delete:\n%s\nwant:\n%s", got, want)
	}
	if _, ok := s.ByTreeID[g0]; ok {
		t.Error("descendant of a deleted block is still listed")
	}
	for _, node := range []string{ids["c0"], g0} {
		if _, err := d.Apply([]Op{NodeText(node, "x")}); err == nil {
			t.Errorf("deleted block %s could still be edited", node)
		}
	}
	// A block re-created under a live parent reuses nothing of the deleted one.
	b := clone(t, d, 2)
	if got := shape(mustState(t, b)); got != shape(s) {
		t.Errorf("restored shape:\n%s", got)
	}
}

func TestWalkAndPath(t *testing.T) {
	d := newDoc(t, 1)
	ids := page(t, d, "r", "a", "b")
	mustApply(t, d,
		TreeCreate("a1", ids["a"], -1, nil),
		TreeCreate("a11", Placeholder("a1"), -1, nil),
	)
	s := mustState(t, d)
	var order []string
	s.Walk(func(n *Node, depth int) bool {
		order = append(order, fmt.Sprintf("%d:%s", depth, n.ID))
		return n.ID != "a1" // skip a1's subtree
	})
	if want := []string{"0:r", "1:a", "2:a1", "1:b"}; !reflect.DeepEqual(order, want) {
		t.Errorf("Walk order = %v, want %v", order, want)
	}
	if got, want := s.ByID["a11"].Path(), []string{"r", "a", "a1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Path(a11) = %v, want %v", got, want)
	}
	if got := s.ByID["r"].Path(); len(got) != 0 {
		t.Errorf("Path(root) = %v, want empty", got)
	}
}
