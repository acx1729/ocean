package loro

import (
	"fmt"
	"testing"
)

// BenchmarkNodeText measures one Apply with a single NodeText op on a block
// whose content changes every iteration.
func BenchmarkNodeText(b *testing.B) {
	d := newDoc(b, 1)
	ids := page(b, d, "blk-root", "blk-1")
	n := ids["blk-1"]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		text := fmt.Sprintf("Paragraph %d: the quick brown fox jumps over the lazy dog.", i)
		if _, err := d.Apply([]Op{NodeText(n, text)}); err != nil {
			b.Fatal(err)
		}
	}
}

func create500Ops() []Op {
	ops := make([]Op, 0, 501)
	ops = append(ops, TreeCreate("blk-root", "", -1, &TreeCreateInit{TypeID: "type-page", Content: "# Page"}))
	for j := 0; j < 500; j++ {
		ops = append(ops, TreeCreate(fmt.Sprintf("blk-%03d", j), Placeholder("blk-root"), -1, &TreeCreateInit{
			TypeID:  "type-para",
			Content: "A paragraph of Markdown text with a [link](https://example.com).",
			Props:   map[string]any{"prop-order": j},
		}))
	}
	return ops
}

// BenchmarkApplyCreate500 measures one Apply creating 500 blocks in a fresh
// document.
func BenchmarkApplyCreate500(b *testing.B) {
	ops := create500Ops()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := New()
		if _, err := d.Apply(ops); err != nil {
			b.Fatal(err)
		}
		d.Close()
	}
}

// BenchmarkState500 measures State on a 501-block document.
func BenchmarkState500(b *testing.B) {
	d := newDoc(b, 1)
	mustApply(b, d, create500Ops()...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.State(); err != nil {
			b.Fatal(err)
		}
	}
}
