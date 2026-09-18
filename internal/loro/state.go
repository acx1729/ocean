package loro

import (
	"encoding/json"
	"fmt"
)

// State is a parsed snapshot of a document: the page metadata and the block
// tree. It is a copy; later changes to the Doc are not reflected.
type State struct {
	// Meta is the content of the "meta" map.
	Meta Meta
	// Roots are the top-level blocks in sibling order.
	Roots []*Node
	// ByID indexes blocks by block id (the "id" stored in the node data).
	// Blocks without an id are absent; when two nodes carry the same id the
	// first one in tree order wins.
	ByID map[string]*Node
	// ByTreeID indexes blocks by Loro tree id ("<counter>@<peer>").
	ByTreeID map[string]*Node
}

// Meta is the page metadata held by the "meta" map. Keys holding a value of an
// unexpected type are left at their zero value.
type Meta struct {
	Title       string
	Icon        string
	Format      string // "markdown" or "outliner"
	JournalDate string // YYYY-MM-DD, empty when the page is not a journal page
	TypeID      string
	// Props is the page block's property bag ("meta.props"); nil when absent.
	// Numbers decode as float64, relation sets as map[string]any.
	Props map[string]any
}

// Node is one block of the tree.
type Node struct {
	// TreeID is Loro's node identity, used to address the block in ops.
	TreeID string
	// ID is the block id stored in the node data.
	ID string
	// TypeID, Content, PortalDocID, CreatedAt and CreatedBy mirror the node
	// data keys of the same names ("content" is the block's Markdown source).
	TypeID      string
	Content     string
	PortalDocID string
	CreatedAt   string
	CreatedBy   string
	// Props is the block's property bag; nil when absent.
	Props map[string]any
	// FractionalIndex is the hex-encoded sibling position; it sorts
	// lexicographically in sibling order.
	FractionalIndex string
	// Index is the position among the siblings.
	Index int
	// Parent is nil for root blocks.
	Parent *Node
	// Children are in sibling order.
	Children []*Node
}

// Walk visits the blocks in pre-order (each block before its children,
// children in sibling order). depth is 0 for roots. When fn returns false the
// block's subtree is skipped.
func (s *State) Walk(fn func(n *Node, depth int) bool) {
	var visit func(nodes []*Node, depth int)
	visit = func(nodes []*Node, depth int) {
		for _, n := range nodes {
			if fn(n, depth) {
				visit(n.Children, depth+1)
			}
		}
	}
	visit(s.Roots, 0)
}

// Path returns the block ids of n's ancestors, root first, excluding n.
func (n *Node) Path() []string {
	var rev []string
	for p := n.Parent; p != nil; p = p.Parent {
		rev = append(rev, p.ID)
	}
	path := make([]string, len(rev))
	for i, id := range rev {
		path[len(rev)-1-i] = id
	}
	return path
}

// rawState mirrors the JSON produced by loro_doc_state_json.
type rawState struct {
	Meta   map[string]any `json:"meta"`
	Blocks []rawBlock     `json:"blocks"`
}

type rawBlock struct {
	ID              string         `json:"id"`
	Parent          *string        `json:"parent"`
	Index           int            `json:"index"`
	FractionalIndex string         `json:"fractional_index"`
	Meta            map[string]any `json:"meta"`
}

// parseState builds a State from the state JSON. Blocks arrive in tree order,
// so every parent precedes its children.
func parseState(data []byte) (*State, error) {
	var raw rawState
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &Error{Code: CodeError, Msg: fmt.Sprintf("decode state: %v", err)}
	}
	s := &State{
		Meta: Meta{
			Title:       stringAt(raw.Meta, "title"),
			Icon:        stringAt(raw.Meta, "icon"),
			Format:      stringAt(raw.Meta, "format"),
			JournalDate: stringAt(raw.Meta, "journal_date"),
			TypeID:      stringAt(raw.Meta, "type_id"),
			Props:       objectAt(raw.Meta, "props"),
		},
		ByID:     make(map[string]*Node, len(raw.Blocks)),
		ByTreeID: make(map[string]*Node, len(raw.Blocks)),
	}
	for _, b := range raw.Blocks {
		n := &Node{
			TreeID:          b.ID,
			ID:              stringAt(b.Meta, "id"),
			TypeID:          stringAt(b.Meta, "type_id"),
			Content:         stringAt(b.Meta, "content"),
			PortalDocID:     stringAt(b.Meta, "portal_doc_id"),
			CreatedAt:       stringAt(b.Meta, "created_at"),
			CreatedBy:       stringAt(b.Meta, "created_by"),
			Props:           objectAt(b.Meta, "props"),
			FractionalIndex: b.FractionalIndex,
			Index:           b.Index,
		}
		if b.Parent != nil {
			parent, ok := s.ByTreeID[*b.Parent]
			if !ok {
				return nil, &Error{Code: CodeError, Msg: fmt.Sprintf("decode state: block %s lists unknown parent %s", b.ID, *b.Parent)}
			}
			n.Parent = parent
			parent.Children = append(parent.Children, n)
		} else {
			s.Roots = append(s.Roots, n)
		}
		s.ByTreeID[n.TreeID] = n
		if n.ID != "" {
			if _, dup := s.ByID[n.ID]; !dup {
				s.ByID[n.ID] = n
			}
		}
	}
	return s, nil
}

func stringAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func objectAt(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}
