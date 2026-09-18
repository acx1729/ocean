package markdown

import "sort"

// NodeType identifies the kind of a Node. Names follow the unified/remark
// (mdast) vocabulary wherever mdast has an equivalent node; the R1 extensions
// (Logseq, Obsidian, math, MDX) keep camelCase names of their own.
type NodeType string

// Block-level (flow) node types.
const (
	TypeRoot               NodeType = "root"
	TypeParagraph          NodeType = "paragraph"
	TypeHeading            NodeType = "heading"
	TypeThematicBreak      NodeType = "thematicBreak"
	TypeBlockquote         NodeType = "blockquote"
	TypeCallout            NodeType = "callout"
	TypeList               NodeType = "list"
	TypeListItem           NodeType = "listItem"
	TypeCode               NodeType = "code"
	TypeHTML               NodeType = "html"
	TypeTable              NodeType = "table"
	TypeTableRow           NodeType = "tableRow"
	TypeTableCell          NodeType = "tableCell"
	TypeMath               NodeType = "math"
	TypeRaw                NodeType = "raw"
	TypeFootnoteDefinition NodeType = "footnoteDefinition"
	TypeDefinition         NodeType = "definition"
	TypeMdxEsm             NodeType = "mdxEsm"
	TypeMdxJsxFlow         NodeType = "mdxJsxFlow"
	TypeMdxExpression      NodeType = "mdxExpression"
	TypeFrontmatter        NodeType = "frontmatter"
	TypeProperty           NodeType = "property"
	TypeOrgBlock           NodeType = "orgBlock"
	TypeDrawer             NodeType = "drawer"
	TypeScheduled          NodeType = "scheduled"
	TypeDeadline           NodeType = "deadline"
)

// Inline (phrasing) node types. TypeHTML and TypeMdxExpression occur at both
// levels; their level is that of the parent.
const (
	TypeText              NodeType = "text"
	TypeEmphasis          NodeType = "emphasis"
	TypeStrong            NodeType = "strong"
	TypeDelete            NodeType = "delete"
	TypeHighlight         NodeType = "highlight"
	TypeInlineCode        NodeType = "inlineCode"
	TypeInlineMath        NodeType = "inlineMath"
	TypeLink              NodeType = "link"
	TypeLinkReference     NodeType = "linkReference"
	TypeImage             NodeType = "image"
	TypeImageReference    NodeType = "imageReference"
	TypeWikiLink          NodeType = "wikiLink"
	TypeEmbed             NodeType = "embed"
	TypeBlockRef          NodeType = "blockRef"
	TypeTag               NodeType = "tag"
	TypeMacro             NodeType = "macro"
	TypeFootnoteReference NodeType = "footnoteReference"
	TypeMdxJsxText        NodeType = "mdxJsxText"
	TypeBreak             NodeType = "break"
)

// Position is the source span of a node. Lines and columns are 1-based,
// offsets are 0-based byte offsets; columns count bytes from the line start.
type Position struct {
	StartLine, StartColumn, StartOffset int
	EndLine, EndColumn, EndOffset       int
}

// Node is one node of the document tree. A single struct with a Type
// discriminator keeps the JSON mapping simple; only the fields that apply to
// a Type are populated (see the mdast documentation for their meaning):
//
//	Value      text, inlineCode, code, html, math, inlineMath, raw, frontmatter,
//	           orgBlock, drawer, scheduled, deadline, mdx*, blockRef, tag
//	Depth      heading (1-6)
//	Ordered/Start/Spread   list; Spread and Checked also on listItem
//	Lang/Meta  code
//	URL/Title  link, image, definition; Alt on image, imageReference
//	Align      table ("left", "right", "center" or "" per column)
//	Label/Identifier/ReferenceType  linkReference, imageReference, definition,
//	           footnoteReference, footnoteDefinition
//	Attrs      extension data: wikiLink/embed (target, alias, heading, block,
//	           syntax), callout (kind, title, fold), property (key, value),
//	           macro (name, args), orgBlock/drawer (name), raw (kind),
//	           paragraph/heading/listItem (marker, priority), inlineMath (display)
type Node struct {
	Type          NodeType
	Children      []*Node
	Value         string
	Depth         int
	Ordered       bool
	Start         int
	Spread        bool
	Checked       *bool
	Lang, Meta    string
	URL, Title    string
	Alt           string
	Align         []string
	Label         string
	Identifier    string
	ReferenceType string
	Attrs         map[string]string
	Pos           Position
}

// Document is a parsed Markdown document: a root node holding the block tree.
type Document struct {
	Root *Node
}

// NewNode returns a node of the given type.
func NewNode(t NodeType) *Node { return &Node{Type: t} }

// Append appends children to n and returns n.
func (n *Node) Append(children ...*Node) *Node {
	n.Children = append(n.Children, children...)
	return n
}

// Attr returns the extension attribute key, or "" when absent.
func (n *Node) Attr(key string) string {
	if n == nil || n.Attrs == nil {
		return ""
	}
	return n.Attrs[key]
}

// SetAttr sets an extension attribute and returns n.
func (n *Node) SetAttr(key, value string) *Node {
	if n.Attrs == nil {
		n.Attrs = map[string]string{}
	}
	n.Attrs[key] = value
	return n
}

// Clone returns a deep copy of the node.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Checked != nil {
		v := *n.Checked
		c.Checked = &v
	}
	if n.Align != nil {
		c.Align = append([]string(nil), n.Align...)
	}
	if n.Attrs != nil {
		c.Attrs = make(map[string]string, len(n.Attrs))
		for k, v := range n.Attrs {
			c.Attrs[k] = v
		}
	}
	if n.Children != nil {
		c.Children = make([]*Node, len(n.Children))
		for i, ch := range n.Children {
			c.Children[i] = ch.Clone()
		}
	}
	return &c
}

// Walk visits n and its descendants in document order. When fn returns false
// the node's children are skipped.
func Walk(n *Node, fn func(*Node) bool) {
	if n == nil {
		return
	}
	if !fn(n) {
		return
	}
	for _, c := range n.Children {
		Walk(c, fn)
	}
}

// Equal reports whether two documents are structurally identical, ignoring
// positions.
func Equal(a, b *Document) bool {
	if a == nil || b == nil {
		return a == b
	}
	return EqualNode(a.Root, b.Root)
}

// EqualNode reports whether two nodes are structurally identical, ignoring
// positions. A nil Attrs map equals an empty one.
func EqualNode(a, b *Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Type != b.Type || a.Value != b.Value || a.Depth != b.Depth ||
		a.Ordered != b.Ordered || a.Start != b.Start || a.Spread != b.Spread ||
		a.Lang != b.Lang || a.Meta != b.Meta || a.URL != b.URL || a.Title != b.Title ||
		a.Alt != b.Alt || a.Label != b.Label || a.Identifier != b.Identifier ||
		a.ReferenceType != b.ReferenceType {
		return false
	}
	if (a.Checked == nil) != (b.Checked == nil) || (a.Checked != nil && *a.Checked != *b.Checked) {
		return false
	}
	if len(a.Align) != len(b.Align) {
		return false
	}
	for i := range a.Align {
		if a.Align[i] != b.Align[i] {
			return false
		}
	}
	if len(a.Attrs) != len(b.Attrs) {
		return false
	}
	for k, v := range a.Attrs {
		if bv, ok := b.Attrs[k]; !ok || bv != v {
			return false
		}
	}
	if len(a.Children) != len(b.Children) {
		return false
	}
	for i := range a.Children {
		if !EqualNode(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

// sortedAttrKeys returns the attribute keys in deterministic order.
func sortedAttrKeys(attrs map[string]string) []string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isInline reports whether t is always an inline node type.
func isInline(t NodeType) bool {
	switch t {
	case TypeText, TypeEmphasis, TypeStrong, TypeDelete, TypeHighlight, TypeInlineCode,
		TypeInlineMath, TypeLink, TypeLinkReference, TypeImage, TypeImageReference,
		TypeWikiLink, TypeEmbed, TypeBlockRef, TypeTag, TypeMacro, TypeFootnoteReference,
		TypeMdxJsxText, TypeBreak:
		return true
	}
	return false
}

// boolPtr returns a pointer to b.
func boolPtr(b bool) *bool { return &b }
