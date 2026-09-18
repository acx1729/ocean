package loro

import (
	"fmt"
	"strings"
	"testing"
)

// newDoc returns a document that is closed when the test ends. A non-zero
// peer sets the peer id so that tree ids in failure messages are stable.
func newDoc(tb testing.TB, peer uint64) *Doc {
	tb.Helper()
	d := New()
	tb.Cleanup(d.Close)
	if peer != 0 {
		if err := d.SetPeerID(peer); err != nil {
			tb.Fatalf("SetPeerID(%d): %v", peer, err)
		}
	}
	return d
}

func mustApply(tb testing.TB, d *Doc, ops ...Op) *ApplyResult {
	tb.Helper()
	res, err := d.Apply(ops)
	if err != nil {
		tb.Fatalf("Apply: %v", err)
	}
	return res
}

func mustState(tb testing.TB, d *Doc) *State {
	tb.Helper()
	s, err := d.State()
	if err != nil {
		tb.Fatalf("State: %v", err)
	}
	return s
}

func mustVV(tb testing.TB, d *Doc) []byte {
	tb.Helper()
	vv, err := d.OplogVV()
	if err != nil {
		tb.Fatalf("OplogVV: %v", err)
	}
	return vv
}

func mustSnapshot(tb testing.TB, d *Doc) []byte {
	tb.Helper()
	snap, err := d.ExportSnapshot()
	if err != nil {
		tb.Fatalf("ExportSnapshot: %v", err)
	}
	return snap
}

// clone opens a second document holding the same state, with its own peer id.
func clone(tb testing.TB, src *Doc, peer uint64) *Doc {
	tb.Helper()
	d, err := FromBytes(mustSnapshot(tb, src))
	if err != nil {
		tb.Fatalf("FromBytes: %v", err)
	}
	tb.Cleanup(d.Close)
	if err := d.SetPeerID(peer); err != nil {
		tb.Fatalf("SetPeerID(%d): %v", peer, err)
	}
	return d
}

// pull imports into dst everything src has that dst lacks.
func pull(tb testing.TB, src, dst *Doc) {
	tb.Helper()
	delta, err := src.ExportUpdatesFrom(mustVV(tb, dst))
	if err != nil {
		tb.Fatalf("ExportUpdatesFrom: %v", err)
	}
	if err := dst.Import(delta); err != nil {
		tb.Fatalf("Import: %v", err)
	}
}

// exchange synchronizes two documents in both directions.
func exchange(tb testing.TB, a, b *Doc) {
	tb.Helper()
	pull(tb, a, b)
	pull(tb, b, a)
}

// shape renders the tree in pre-order, one block per line, for comparisons.
func shape(s *State) string {
	var sb strings.Builder
	s.Walk(func(n *Node, depth int) bool {
		fmt.Fprintf(&sb, "%s%s %q\n", strings.Repeat("  ", depth), n.ID, n.Content)
		return true
	})
	return sb.String()
}

// page builds the shared fixture: metadata plus a root block with the given
// children, each holding its block id as content. It returns the tree ids by
// block id.
func page(tb testing.TB, d *Doc, root string, children ...string) map[string]string {
	tb.Helper()
	ops := []Op{
		MetaSet("title", "Fixture"),
		MetaSet("format", "outliner"),
		TreeCreate(root, "", -1, &TreeCreateInit{TypeID: "type-page", Content: root}),
	}
	for _, c := range children {
		ops = append(ops, TreeCreate(c, Placeholder(root), -1, &TreeCreateInit{TypeID: "type-para", Content: c}))
	}
	return mustApply(tb, d, ops...).Created
}

// childIDs lists the block ids of n's children in order.
func childIDs(n *Node) []string {
	ids := make([]string, len(n.Children))
	for i, c := range n.Children {
		ids[i] = c.ID
	}
	return ids
}
