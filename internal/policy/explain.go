package policy

import (
	"strings"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
)

// Operators of an ExplainNode.
const (
	OperatorDirect         = "direct"           // leaf listing the users assigned to the relation
	OperatorComputed       = "computed"         // leaf rewriting to another relation on the same object
	OperatorTupleToUserset = "tuple_to_userset" // leaf following a tupleset relation to other objects
	OperatorUnion          = "union"
	OperatorIntersection   = "intersection"
	OperatorDifference     = "difference" // children are [base, subtract]
)

// ExplainNode is one node of an OpenFGA Expand tree annotated with the scheme
// role behind its relation, so an administrator can read why a principal
// holds a permission without knowing the generated model.
type ExplainNode struct {
	// Relation and Object identify the userset the node expands
	// ("can_edit" on "doc:D").
	Relation string
	Object   string
	// Role is the scheme role the relation compiles from, when it is one.
	Role string
	// Operator is one of the Operator constants.
	Operator string
	// Users lists the subjects of a direct leaf ("user:alice",
	// "group:g#member", "user:*").
	Users []string
	// Children are the operands of a set operation, the target of a computed
	// rewrite, or the tupleset followed by the computed usersets of a
	// tuple-to-userset rewrite.
	Children []*ExplainNode
}

// AnnotateExpand converts an Expand tree into ExplainNodes, mapping every
// relation name back to the scheme role that produced it. It returns nil for
// an empty tree.
func AnnotateExpand(tree *openfgav1.UsersetTree, c *Compiled) *ExplainNode {
	if tree == nil || tree.GetRoot() == nil {
		return nil
	}
	return annotateNode(tree.GetRoot(), c)
}

func annotateNode(n *openfgav1.UsersetTree_Node, c *Compiled) *ExplainNode {
	node := newExplainNode(n.GetName(), c)
	switch v := n.GetValue().(type) {
	case *openfgav1.UsersetTree_Node_Leaf:
		annotateLeaf(node, v.Leaf, c)
	case *openfgav1.UsersetTree_Node_Union:
		node.Operator = OperatorUnion
		for _, child := range v.Union.GetNodes() {
			node.Children = append(node.Children, annotateNode(child, c))
		}
	case *openfgav1.UsersetTree_Node_Intersection:
		node.Operator = OperatorIntersection
		for _, child := range v.Intersection.GetNodes() {
			node.Children = append(node.Children, annotateNode(child, c))
		}
	case *openfgav1.UsersetTree_Node_Difference:
		node.Operator = OperatorDifference
		if base := v.Difference.GetBase(); base != nil {
			node.Children = append(node.Children, annotateNode(base, c))
		}
		if sub := v.Difference.GetSubtract(); sub != nil {
			node.Children = append(node.Children, annotateNode(sub, c))
		}
	}
	return node
}

func annotateLeaf(node *ExplainNode, leaf *openfgav1.UsersetTree_Leaf, c *Compiled) {
	switch v := leaf.GetValue().(type) {
	case *openfgav1.UsersetTree_Leaf_Users:
		node.Operator = OperatorDirect
		node.Users = append([]string(nil), v.Users.GetUsers()...)
	case *openfgav1.UsersetTree_Leaf_Computed:
		node.Operator = OperatorComputed
		node.Children = append(node.Children, newExplainNode(v.Computed.GetUserset(), c))
	case *openfgav1.UsersetTree_Leaf_TupleToUserset:
		node.Operator = OperatorTupleToUserset
		node.Children = append(node.Children, newExplainNode(v.TupleToUserset.GetTupleset(), c))
		for _, computed := range v.TupleToUserset.GetComputed() {
			node.Children = append(node.Children, newExplainNode(computed.GetUserset(), c))
		}
	}
}

// newExplainNode builds a node for a userset string such as "doc:D#editor".
func newExplainNode(userset string, c *Compiled) *ExplainNode {
	object, relation := splitUserset(userset)
	return &ExplainNode{Object: object, Relation: relation, Role: c.roleFor(object, relation)}
}

// roleFor returns the scheme role behind a relation on an object, or "".
func (c *Compiled) roleFor(object, relation string) string {
	if c == nil {
		return ""
	}
	objectType, _, _ := strings.Cut(object, ":")
	if role, ok := c.relationRoles[objectType][relation]; ok {
		return role
	}
	for _, level := range append([]string{TypeBlock}, Levels...) {
		for role, rel := range c.RoleRelations[level] {
			if level == objectType && rel == relation {
				return role
			}
		}
	}
	return ""
}

// splitUserset splits "type:id#relation" into object and relation.
func splitUserset(userset string) (object, relation string) {
	object, relation, _ = strings.Cut(userset, "#")
	return object, relation
}
