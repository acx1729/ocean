package loro

// Op is one structured operation of a Doc.Apply batch. Ops are built with the
// constructors in this package and serialized to the JSON the Rust side
// expects; the concrete types are not part of the API.
//
// Node references ("node" and "parentTreeID" parameters) are Loro tree ids in
// the form "<counter>@<peer>" as reported by State and ApplyResult.Created,
// or the Placeholder of a block created earlier in the same batch. An empty
// parent means the tree root. Indexes count siblings from 0; -1 appends.
type Op interface {
	isOp()
}

// Placeholder returns the reference "$<uuid>" that later ops of the same
// batch use to address the block a preceding TreeCreate op creates.
func Placeholder(uuid string) string {
	return "$" + uuid
}

type metaOp struct {
	Op    string `json:"op"`
	Key   string `json:"key"`
	Value any    `json:"value,omitempty"`
}

func (metaOp) isOp() {}

// MetaSet sets a key of the "meta" map to a plain value (objects and arrays
// are stored as values, not containers). The key "props" is managed by
// MetaPropSet and MetaPropDel and is rejected here.
func MetaSet(key string, value any) Op {
	return metaOp{Op: "meta.set", Key: key, Value: value}
}

// MetaDel deletes a key of the "meta" map.
func MetaDel(key string) Op {
	return metaOp{Op: "meta.del", Key: key}
}

// MetaPropSet sets a property in the page's "meta.props" map, creating the
// map on demand. An object whose values are all true (a relation set) is
// stored as a nested map so that concurrent additions merge; any other value
// is stored as a plain value.
func MetaPropSet(key string, value any) Op {
	return metaOp{Op: "meta.props.set", Key: key, Value: value}
}

// MetaPropDel deletes a property from "meta.props".
func MetaPropDel(key string) Op {
	return metaOp{Op: "meta.props.del", Key: key}
}

// TreeCreateInit holds the optional initial data of a created block.
type TreeCreateInit struct {
	// TypeID is stored in the node's "type_id" key when non-empty.
	TypeID string
	// CreatedAt is an RFC 3339 timestamp stored in "created_at" when non-empty.
	CreatedAt string
	// CreatedBy is the author DID stored in "created_by" when non-empty.
	CreatedBy string
	// Content is the initial Markdown source of the block's "content" text.
	Content string
	// Props are the initial properties. A value that is an object whose
	// values are all true (for example map[string]bool{"target": true})
	// becomes a nested map holding a relation set; other values are stored
	// as plain values.
	Props map[string]any
}

type treeCreateOp struct {
	Op        string         `json:"op"`
	ID        string         `json:"id"`
	Parent    string         `json:"parent"`
	Index     int            `json:"index"`
	TypeID    string         `json:"type_id,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	CreatedBy string         `json:"created_by,omitempty"`
	Content   string         `json:"content,omitempty"`
	Props     map[string]any `json:"props,omitempty"`
}

func (treeCreateOp) isOp() {}

// TreeCreate creates a block with block id `id` under parentTreeID ("" for a
// root block) at the given sibling index (-1 appends). The node receives its
// "id", the optional init fields, an empty "props" map and a "content" text.
// init may be nil.
func TreeCreate(id, parentTreeID string, index int, init *TreeCreateInit) Op {
	op := treeCreateOp{Op: "tree.create", ID: id, Parent: parentTreeID, Index: index}
	if init != nil {
		op.TypeID = init.TypeID
		op.CreatedAt = init.CreatedAt
		op.CreatedBy = init.CreatedBy
		op.Content = init.Content
		op.Props = init.Props
	}
	return op
}

type treeMoveOp struct {
	Op     string `json:"op"`
	Node   string `json:"node"`
	Parent string `json:"parent"`
	Index  int    `json:"index"`
}

func (treeMoveOp) isOp() {}

// TreeMove moves a block under parentTreeID ("" for the root) at the given
// sibling index (-1 = last). Moving a block under itself or one of its
// descendants is rejected.
func TreeMove(node, parentTreeID string, index int) Op {
	return treeMoveOp{Op: "tree.move", Node: node, Parent: parentTreeID, Index: index}
}

type treeDeleteOp struct {
	Op   string `json:"op"`
	Node string `json:"node"`
}

func (treeDeleteOp) isOp() {}

// TreeDelete deletes a block together with its subtree.
func TreeDelete(node string) Op {
	return treeDeleteOp{Op: "tree.delete", Node: node}
}

type nodeKeyOp struct {
	Op    string `json:"op"`
	Node  string `json:"node"`
	Key   string `json:"key"`
	Value any    `json:"value,omitempty"`
}

func (nodeKeyOp) isOp() {}

// NodeSet sets a key of the block's data map (for example "type_id",
// "portal_doc_id", "created_at", "created_by") to a plain value. The keys
// "id", "props" and "content" are managed by dedicated ops and are rejected.
func NodeSet(node, key string, value any) Op {
	return nodeKeyOp{Op: "node.set", Node: node, Key: key, Value: value}
}

// NodeDel deletes a key of the block's data map; the same keys as for NodeSet
// are rejected.
func NodeDel(node, key string) Op {
	return nodeKeyOp{Op: "node.del", Node: node, Key: key}
}

type nodeTextOp struct {
	Op   string `json:"op"`
	Node string `json:"node"`
	Text string `json:"text"`
}

func (nodeTextOp) isOp() {}

// NodeText replaces the block's Markdown source. The new text is applied as a
// minimal diff against the current content, so concurrent edits of the same
// block merge instead of overwriting each other.
func NodeText(node, text string) Op {
	return nodeTextOp{Op: "node.text", Node: node, Text: text}
}

// NodePropSet sets a property in the block's "props" map, creating the map on
// demand. Relation sets (objects whose values are all true) become nested
// maps; other values are stored as plain values.
func NodePropSet(node, key string, value any) Op {
	return nodeKeyOp{Op: "node.props.set", Node: node, Key: key, Value: value}
}

// NodePropDel deletes a property from the block's "props" map.
func NodePropDel(node, key string) Op {
	return nodeKeyOp{Op: "node.props.del", Node: node, Key: key}
}

type nodePropMapOp struct {
	Op     string `json:"op"`
	Node   string `json:"node"`
	Key    string `json:"key"`
	Subkey string `json:"subkey"`
	Value  any    `json:"value,omitempty"`
}

func (nodePropMapOp) isOp() {}

// NodePropMapSet sets subkey inside the nested map stored at props[key],
// creating the nested map on demand (and replacing a plain value if one is
// there). It is the operation for adding a member to a multi-valued relation:
// NodePropMapSet(node, relation, targetID, true).
func NodePropMapSet(node, key, subkey string, value any) Op {
	return nodePropMapOp{Op: "node.props.mapset", Node: node, Key: key, Subkey: subkey, Value: value}
}

// NodePropMapDel removes subkey from the nested map at props[key]. It is a
// no-op when the map or the subkey does not exist.
func NodePropMapDel(node, key, subkey string) Op {
	return nodePropMapOp{Op: "node.props.mapdel", Node: node, Key: key, Subkey: subkey}
}
