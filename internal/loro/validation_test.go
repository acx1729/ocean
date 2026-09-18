package loro

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// bogusOp lets tests send ops the library does not know or that lack fields.
type bogusOp struct {
	Op string `json:"op"`
}

func (bogusOp) isOp() {}

func TestValidationLeavesDocUntouched(t *testing.T) {
	d := newDoc(t, 1)
	ids := page(t, d, "blk-root", "c0", "c1")
	root, c0, c1 := ids["blk-root"], ids["c0"], ids["c1"]
	g0 := mustApply(t, d, TreeCreate("g0", c0, -1, &TreeCreateInit{Content: "g0"})).Created["g0"]
	before, vv := shape(mustState(t, d)), mustVV(t, d)

	cases := []struct {
		name string
		ops  []Op
		want string // substring of the error message
	}{
		{"unknown node", []Op{NodeText("999@424242", "x")}, "does not exist"},
		{"bad op after good ops", []Op{MetaSet("title", "changed"), NodeText(c0, "ok"), NodeText("999@424242", "x")}, "op[2] node.text"},
		{"malformed tree id", []Op{NodeText("not-an-id", "x")}, "invalid tree id"},
		{"create index out of range", []Op{TreeCreate("n", root, 3, nil)}, "out of range"},
		{"move index out of range", []Op{TreeMove(c0, root, 2)}, "out of range"},
		{"negative index", []Op{TreeMove(c0, root, -2)}, "index"},
		{"move under itself", []Op{TreeMove(c0, c0, -1)}, "cycle"},
		{"move under descendant", []Op{TreeMove(root, g0, -1)}, "cycle"},
		{"move under placeholder descendant", []Op{TreeCreate("n", c0, -1, nil), TreeMove(c0, Placeholder("n"), -1)}, "cycle"},
		{"unknown parent", []Op{TreeCreate("n", "999@424242", -1, nil)}, "parent"},
		{"deleted in same batch", []Op{TreeDelete(c0), NodeText(c0, "x")}, "deleted"},
		{"child of deleted in same batch", []Op{TreeDelete(c0), NodeText(g0, "x")}, "deleted"},
		{"move into deleted subtree", []Op{TreeDelete(c0), TreeMove(c1, g0, -1)}, "deleted"},
		{"duplicate create id", []Op{TreeCreate("n", root, -1, nil), TreeCreate("n", root, -1, nil)}, "twice"},
		{"unknown placeholder", []Op{NodeText(Placeholder("nope"), "x")}, "does not exist"},
		{"empty create id", []Op{TreeCreate("", root, -1, nil)}, "empty"},
		{"reserved node key", []Op{NodeSet(c0, "id", "other")}, "dedicated"},
		{"reserved node key delete", []Op{NodeDel(c0, "content")}, "dedicated"},
		{"reserved meta key", []Op{MetaSet("props", 1)}, "dedicated"},
		{"empty key", []Op{NodePropSet(c0, "", 1)}, "empty"},
		{"empty subkey", []Op{NodePropMapSet(c0, "rel", "", true)}, "empty"},
		{"unknown op", []Op{bogusOp{Op: "tree.explode"}}, "unknown variant"},
		{"missing field", []Op{bogusOp{Op: "node.text"}}, "missing field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.Apply(tc.ops)
			if err == nil {
				t.Fatal("Apply succeeded")
			}
			var lerr *Error
			if !errors.As(err, &lerr) {
				t.Fatalf("error %T (%v) is not *Error", err, err)
			}
			if lerr.Code != CodeError {
				t.Errorf("Code = %d, want %d", lerr.Code, CodeError)
			}
			if errors.Is(err, ErrPartial) {
				t.Error("a rejected batch wrapped ErrPartial")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			s := mustState(t, d)
			if got := shape(s); got != before {
				t.Errorf("tree changed:\n%s", got)
			}
			if s.Meta.Title != "Fixture" {
				t.Errorf("title = %q, want Fixture", s.Meta.Title)
			}
			if got := mustVV(t, d); !bytes.Equal(got, vv) {
				t.Error("oplog version vector changed")
			}
		})
	}
}
