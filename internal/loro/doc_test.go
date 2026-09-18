package loro

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRoundTripSnapshot(t *testing.T) {
	a := newDoc(t, 1)
	res := mustApply(t, a,
		MetaSet("title", "Hello"),
		MetaSet("icon", "book"),
		MetaSet("format", "markdown"),
		MetaSet("journal_date", "2026-09-18"),
		MetaSet("type_id", "type-page"),
		MetaPropSet("prop-status", "draft"),
		MetaPropSet("prop-tags", map[string]bool{"tag-a": true}),
		TreeCreate("blk-root", "", -1, &TreeCreateInit{
			TypeID: "type-page", Content: "# Hello",
			CreatedAt: "2026-09-18T10:00:00Z", CreatedBy: "did:key:zA",
		}),
		TreeCreate("blk-child", Placeholder("blk-root"), -1, &TreeCreateInit{
			TypeID: "type-para", Content: "First paragraph",
			Props: map[string]any{
				"prop-num": 42,
				"prop-rel": map[string]bool{"target-1": true, "target-2": true},
			},
		}),
	)
	if !res.Changed {
		t.Error("Changed = false for a batch that created blocks")
	}
	if len(res.Created) != 2 || res.Created["blk-root"] == "" || res.Created["blk-child"] == "" {
		t.Fatalf("Created = %v, want tree ids for blk-root and blk-child", res.Created)
	}

	b, err := FromBytes(mustSnapshot(t, a))
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	defer b.Close()
	sa, sb := mustState(t, a), mustState(t, b)

	wantMeta := Meta{
		Title: "Hello", Icon: "book", Format: "markdown", JournalDate: "2026-09-18", TypeID: "type-page",
		Props: map[string]any{"prop-status": "draft", "prop-tags": map[string]any{"tag-a": true}},
	}
	for name, s := range map[string]*State{"source": sa, "restored": sb} {
		if !reflect.DeepEqual(s.Meta, wantMeta) {
			t.Errorf("%s Meta = %+v, want %+v", name, s.Meta, wantMeta)
		}
	}
	const wantShape = "blk-root \"# Hello\"\n  blk-child \"First paragraph\"\n"
	if got := shape(sa); got != wantShape {
		t.Errorf("source shape:\n%s\nwant:\n%s", got, wantShape)
	}
	if got := shape(sb); got != wantShape {
		t.Errorf("restored shape:\n%s\nwant:\n%s", got, wantShape)
	}

	root, child := sb.ByID["blk-root"], sb.ByID["blk-child"]
	if root == nil || child == nil {
		t.Fatalf("ByID = %v, want blk-root and blk-child", sb.ByID)
	}
	if child.Parent != root || child.Index != 0 || child.TypeID != "type-para" {
		t.Errorf("child = %+v, want parent blk-root, index 0, type-para", child)
	}
	if got := child.Props["prop-num"]; got != float64(42) {
		t.Errorf("prop-num = %v (%T), want 42", got, got)
	}
	if got, want := child.Props["prop-rel"], map[string]any{"target-1": true, "target-2": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("prop-rel = %v, want %v", got, want)
	}
	if root.CreatedAt != "2026-09-18T10:00:00Z" || root.CreatedBy != "did:key:zA" || root.TypeID != "type-page" {
		t.Errorf("root = %+v, want created_at/created_by/type_id preserved", root)
	}
	if root.Props == nil || len(root.Props) != 0 {
		t.Errorf("root Props = %v, want an empty (non-nil) props map created with the node", root.Props)
	}
	if sb.ByTreeID[res.Created["blk-root"]] != root || sb.ByTreeID[res.Created["blk-child"]] != child {
		t.Errorf("ByTreeID does not map Created tree ids to the blocks")
	}
	if root.TreeID != res.Created["blk-root"] {
		t.Errorf("root TreeID = %s, want %s", root.TreeID, res.Created["blk-root"])
	}

	// The update returned by Apply reproduces the state on its own.
	c, err := FromBytes(res.Update)
	if err != nil {
		t.Fatalf("FromBytes(update): %v", err)
	}
	defer c.Close()
	if got := shape(mustState(t, c)); got != wantShape {
		t.Errorf("shape from update:\n%s\nwant:\n%s", got, wantShape)
	}
}

func TestContainerNamesMatchClientLayout(t *testing.T) {
	d := newDoc(t, 1)
	mustApply(t, d,
		MetaSet("title", "T"),
		MetaPropSet("k", "v"),
		TreeCreate("blk", "", -1, &TreeCreateInit{Content: "x"}),
	)
	raw, err := d.stateJSON()
	if err != nil {
		t.Fatalf("stateJSON: %v", err)
	}
	var got struct {
		Meta   map[string]json.RawMessage `json:"meta"`
		Blocks []struct {
			ID     string                     `json:"id"`
			Parent *string                    `json:"parent"`
			Index  int                        `json:"index"`
			FI     string                     `json:"fractional_index"`
			Meta   map[string]json.RawMessage `json:"meta"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("state JSON: %v\n%s", err, raw)
	}
	if string(got.Meta["title"]) != `"T"` {
		t.Errorf("meta.title = %s, want \"T\"", got.Meta["title"])
	}
	if p := got.Meta["props"]; len(p) == 0 || p[0] != '{' {
		t.Errorf("meta.props = %s, want a nested map", p)
	}
	if len(got.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(got.Blocks))
	}
	blk := got.Blocks[0]
	if blk.Parent != nil || blk.Index != 0 || blk.FI == "" || blk.ID == "" {
		t.Errorf("block envelope = %+v, want root at index 0 with a fractional index", blk)
	}
	if string(blk.Meta["id"]) != `"blk"` {
		t.Errorf("node id = %s, want \"blk\"", blk.Meta["id"])
	}
	if p := blk.Meta["props"]; len(p) == 0 || p[0] != '{' {
		t.Errorf("node props = %s, want a nested map", p)
	}
	if c := blk.Meta["content"]; string(c) != `"x"` {
		t.Errorf("node content = %s, want the text \"x\"", c)
	}
}

func TestExportUpdatesFromReturnsDelta(t *testing.T) {
	a := newDoc(t, 1)
	ids := page(t, a, "blk-root", "blk-1")
	base := mustSnapshot(t, a)
	b, err := FromBytes(base)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	defer b.Close()
	vv := mustVV(t, b)

	mustApply(t, a,
		NodeText(ids["blk-1"], "edited"),
		TreeCreate("blk-2", ids["blk-root"], -1, &TreeCreateInit{Content: "blk-2"}),
	)
	delta, err := a.ExportUpdatesFrom(vv)
	if err != nil {
		t.Fatalf("ExportUpdatesFrom(vv): %v", err)
	}
	all, err := a.ExportUpdatesFrom(nil)
	if err != nil {
		t.Fatalf("ExportUpdatesFrom(nil): %v", err)
	}
	if len(delta) >= len(all) {
		t.Errorf("delta is %d bytes, full history %d bytes: the delta must be smaller", len(delta), len(all))
	}

	// Alone, the delta stays pending: its dependencies are missing.
	c := newDoc(t, 3)
	if err := c.Import(delta); err != nil {
		t.Fatalf("Import(delta) into empty doc: %v", err)
	}
	if n := len(mustState(t, c).ByTreeID); n != 0 {
		t.Errorf("delta alone materialized %d blocks, want 0", n)
	}

	if err := b.Import(delta); err != nil {
		t.Fatalf("Import(delta): %v", err)
	}
	sa, sb := mustState(t, a), mustState(t, b)
	if shape(sa) != shape(sb) {
		t.Errorf("states differ after importing the delta:\n%s---\n%s", shape(sa), shape(sb))
	}
	if sb.ByID["blk-1"].Content != "edited" {
		t.Errorf("blk-1 content = %q, want edited", sb.ByID["blk-1"].Content)
	}

	// Once the base arrives the pending delta is applied too.
	if err := c.Import(base); err != nil {
		t.Fatalf("Import(base): %v", err)
	}
	if got := shape(mustState(t, c)); got != shape(sa) {
		t.Errorf("late base import did not release the pending delta:\n%s", got)
	}
}

func TestCheckUpdateHeader(t *testing.T) {
	a := newDoc(t, 1)
	res := mustApply(t, a, MetaSet("title", "T"), TreeCreate("blk", "", -1, &TreeCreateInit{Content: "hello"}))
	shallow, err := a.ExportShallowSnapshot()
	if err != nil {
		t.Fatalf("ExportShallowSnapshot: %v", err)
	}
	all, err := a.ExportUpdatesFrom(nil)
	if err != nil {
		t.Fatalf("ExportUpdatesFrom: %v", err)
	}
	good := map[string][]byte{"update": res.Update, "all updates": all, "snapshot": mustSnapshot(t, a), "shallow snapshot": shallow}
	for name, blob := range good {
		if err := CheckUpdateHeader(blob); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}

	badMagic := append([]byte(nil), res.Update...)
	badMagic[0] ^= 0xff
	bad := map[string][]byte{
		"nil":        nil,
		"empty":      {},
		"text":       []byte("hello, definitely not a loro blob"),
		"zeros":      make([]byte, 64),
		"header cut": res.Update[:10],
		"bad magic":  badMagic,
	}
	for name, blob := range bad {
		err := CheckUpdateHeader(blob)
		if err == nil {
			t.Errorf("%s accepted", name)
			continue
		}
		var lerr *Error
		if !errors.As(err, &lerr) || lerr.Code != CodeError {
			t.Errorf("%s: error %v is not a CodeError *Error", name, err)
		}
	}

	// Every truncation is handled without crashing; anything shorter than the
	// 22-byte header is rejected.
	rejected := 0
	for n := 0; n < len(res.Update); n++ {
		if err := CheckUpdateHeader(res.Update[:n]); err != nil {
			rejected++
		} else if n < 22 {
			t.Errorf("%d-byte prefix accepted", n)
		}
	}
	t.Logf("truncation sweep: %d of %d proper prefixes rejected", rejected, len(res.Update))

	// Import runs the full check: a truncated body is rejected without effect.
	d := newDoc(t, 2)
	if err := d.Import(res.Update[:len(res.Update)-3]); err == nil {
		t.Error("Import accepted a truncated update")
	}
	if n := len(mustState(t, d).ByTreeID); n != 0 {
		t.Errorf("truncated import created %d blocks", n)
	}
}

func TestImportRejectsGarbage(t *testing.T) {
	d := newDoc(t, 1)
	page(t, d, "blk-root")
	before, vv := shape(mustState(t, d)), mustVV(t, d)
	for name, bad := range map[string][]byte{"nil": nil, "text": []byte("nonsense"), "ff": bytes.Repeat([]byte{0xff}, 100)} {
		err := d.Import(bad)
		var lerr *Error
		if !errors.As(err, &lerr) || lerr.Code != CodeError {
			t.Errorf("Import(%s) = %v, want a CodeError *Error", name, err)
		}
		if errors.Is(err, ErrPartial) {
			t.Errorf("Import(%s) wrapped ErrPartial", name)
		}
	}
	if _, err := FromBytes([]byte("nonsense")); err == nil {
		t.Error("FromBytes(garbage) succeeded")
	}
	if got := shape(mustState(t, d)); got != before {
		t.Errorf("tree changed by rejected imports:\n%s", got)
	}
	if got := mustVV(t, d); !bytes.Equal(got, vv) {
		t.Error("oplog version changed by rejected imports")
	}
}

func TestShallowSnapshot(t *testing.T) {
	a := newDoc(t, 1)
	ids := page(t, a, "blk-root", "blk-1", "blk-2")
	for i := 0; i < 200; i++ {
		mustApply(t, a, NodeText(ids["blk-1"], fmt.Sprintf("revision %d of the paragraph", i)))
	}
	full := mustSnapshot(t, a)
	shallow, err := a.ExportShallowSnapshot()
	if err != nil {
		t.Fatalf("ExportShallowSnapshot: %v", err)
	}
	if len(shallow) >= len(full) {
		t.Errorf("shallow snapshot is %d bytes, full snapshot %d: history was not dropped", len(shallow), len(full))
	}

	b, err := FromBytes(shallow)
	if err != nil {
		t.Fatalf("FromBytes(shallow): %v", err)
	}
	defer b.Close()
	if err := b.SetPeerID(2); err != nil {
		t.Fatalf("SetPeerID: %v", err)
	}
	if got, want := shape(mustState(t, b)), shape(mustState(t, a)); got != want {
		t.Fatalf("shallow doc shape:\n%s\nwant:\n%s", got, want)
	}

	// Later updates flow in both directions on top of the shallow history.
	mustApply(t, a, NodeText(ids["blk-2"], "changed on a"))
	mustApply(t, b, TreeCreate("blk-3", ids["blk-root"], 0, &TreeCreateInit{Content: "blk-3"}))
	exchange(t, a, b)
	sa, sb := mustState(t, a), mustState(t, b)
	if shape(sa) != shape(sb) {
		t.Errorf("states differ:\n%s---\n%s", shape(sa), shape(sb))
	}
	if sb.ByID["blk-2"].Content != "changed on a" {
		t.Errorf("blk-2 on shallow doc = %q", sb.ByID["blk-2"].Content)
	}
	if got := childIDs(sa.Roots[0]); !reflect.DeepEqual(got, []string{"blk-3", "blk-1", "blk-2"}) {
		t.Errorf("children = %v, want blk-3 first", got)
	}
}

func TestManyTextUpdatesConverge(t *testing.T) {
	a := newDoc(t, 1)
	ids := page(t, a, "blk-root", "blk-1")
	var last string
	for i := 0; i < 1000; i++ {
		last = fmt.Sprintf("Paragraph %d: the quick brown fox jumps over %d lazy dogs.", i, i%7)
		mustApply(t, a, NodeText(ids["blk-1"], last))
	}
	if got := mustState(t, a).ByID["blk-1"].Content; got != last {
		t.Errorf("content = %q, want %q", got, last)
	}
	b := clone(t, a, 2)
	if got := mustState(t, b).ByID["blk-1"].Content; got != last {
		t.Errorf("restored content = %q, want %q", got, last)
	}
}

func TestKeyOps(t *testing.T) {
	d := newDoc(t, 1)
	ids := page(t, d, "blk-root", "blk-1")
	n := ids["blk-1"]
	mustApply(t, d,
		MetaSet("title", "A"), MetaSet("icon", "i"), MetaDel("icon"),
		MetaPropSet("p1", 1), MetaPropSet("p2", "two"), MetaPropDel("p1"),
		NodeSet(n, "portal_doc_id", "doc-9"), NodeSet(n, "type_id", "type-x"),
		NodeSet(n, "created_by", "did:key:z"), NodeDel(n, "created_by"),
		NodePropSet(n, "num", 3.5), NodePropSet(n, "obj", map[string]any{"a": 1, "b": false}),
		NodePropSet(n, "gone", true), NodePropDel(n, "gone"),
		NodePropMapSet(n, "rel", "t1", true), NodePropMapSet(n, "rel", "t2", true), NodePropMapDel(n, "rel", "t1"),
	)
	s := mustState(t, d)
	if s.Meta.Title != "A" || s.Meta.Icon != "" {
		t.Errorf("Meta = %+v, want title A and no icon", s.Meta)
	}
	if want := map[string]any{"p2": "two"}; !reflect.DeepEqual(s.Meta.Props, want) {
		t.Errorf("Meta.Props = %v, want %v", s.Meta.Props, want)
	}
	blk := s.ByID["blk-1"]
	if blk.PortalDocID != "doc-9" || blk.TypeID != "type-x" || blk.CreatedBy != "" {
		t.Errorf("block = %+v, want portal doc-9, type-x, no created_by", blk)
	}
	want := map[string]any{
		"num": 3.5,
		"obj": map[string]any{"a": float64(1), "b": false},
		"rel": map[string]any{"t2": true},
	}
	if !reflect.DeepEqual(blk.Props, want) {
		t.Errorf("Props = %v, want %v", blk.Props, want)
	}

	// Deleting from a nested map that does not exist is a no-op.
	res := mustApply(t, d, NodePropMapDel(n, "nothing", "x"))
	if res.Changed {
		t.Error("deleting from an absent nested map produced an operation")
	}
}

func TestApplyResultChanged(t *testing.T) {
	d := newDoc(t, 1)
	res := mustApply(t, d)
	if res.Changed || len(res.Created) != 0 {
		t.Errorf("empty batch: Changed = %v, Created = %v", res.Changed, res.Created)
	}
	if err := CheckUpdateHeader(res.Update); err != nil {
		t.Errorf("the update of an empty batch is not a valid blob: %v", err)
	}
	mustApply(t, d, MetaSet("title", "x"))
	if res := mustApply(t, d, MetaSet("title", "x")); res.Changed {
		t.Error("setting a key to its current value reported Changed")
	}
	if res := mustApply(t, d, MetaSet("title", "y")); !res.Changed {
		t.Error("changing a key did not report Changed")
	}
}

func TestCloseIsIdempotentAndBlocksUse(t *testing.T) {
	d := New()
	d.Close()
	d.Close()
	if _, err := d.State(); !errors.Is(err, ErrClosed) {
		t.Errorf("State after Close = %v, want ErrClosed", err)
	}
	if _, err := d.Apply(nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Apply after Close = %v, want ErrClosed", err)
	}
	if err := d.Import([]byte{1}); !errors.Is(err, ErrClosed) {
		t.Errorf("Import after Close = %v, want ErrClosed", err)
	}
	if _, err := d.ExportSnapshot(); !errors.Is(err, ErrClosed) {
		t.Errorf("ExportSnapshot after Close = %v, want ErrClosed", err)
	}
	if err := d.SetPeerID(1); !errors.Is(err, ErrClosed) {
		t.Errorf("SetPeerID after Close = %v, want ErrClosed", err)
	}
	var zero Doc
	zero.Close()
	if _, err := zero.OplogVV(); !errors.Is(err, ErrClosed) {
		t.Errorf("zero Doc OplogVV = %v, want ErrClosed", err)
	}
}

func TestErrorWrapping(t *testing.T) {
	partial := &Error{Code: CodePartial, Msg: "boom"}
	if !errors.Is(partial, ErrPartial) {
		t.Error("a CodePartial error does not wrap ErrPartial")
	}
	if got := partial.Error(); got != "loro: boom (document partially modified)" {
		t.Errorf("partial message = %q", got)
	}
	rejected := &Error{Code: CodeError, Msg: "nope"}
	if errors.Is(rejected, ErrPartial) {
		t.Error("a CodeError error wraps ErrPartial")
	}
	if got := rejected.Error(); got != "loro: nope" {
		t.Errorf("rejected message = %q", got)
	}
	if Version() != abiVersion {
		t.Errorf("Version() = %d, want %d", Version(), abiVersion)
	}
}
