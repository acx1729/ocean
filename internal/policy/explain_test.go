package policy

import (
	"testing"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
)

func usersLeaf(name string, users ...string) *openfgav1.UsersetTree_Node {
	return &openfgav1.UsersetTree_Node{Name: name, Value: &openfgav1.UsersetTree_Node_Leaf{Leaf: &openfgav1.UsersetTree_Leaf{
		Value: &openfgav1.UsersetTree_Leaf_Users{Users: &openfgav1.UsersetTree_Users{Users: users}},
	}}}
}

func ttuLeaf(name, tupleset string, computed ...string) *openfgav1.UsersetTree_Node {
	ttu := &openfgav1.UsersetTree_TupleToUserset{Tupleset: tupleset}
	for _, c := range computed {
		ttu.Computed = append(ttu.Computed, &openfgav1.UsersetTree_Computed{Userset: c})
	}
	return &openfgav1.UsersetTree_Node{Name: name, Value: &openfgav1.UsersetTree_Node_Leaf{Leaf: &openfgav1.UsersetTree_Leaf{
		Value: &openfgav1.UsersetTree_Leaf_TupleToUserset{TupleToUserset: ttu},
	}}}
}

func computedLeaf(name, userset string) *openfgav1.UsersetTree_Node {
	return &openfgav1.UsersetTree_Node{Name: name, Value: &openfgav1.UsersetTree_Node_Leaf{Leaf: &openfgav1.UsersetTree_Leaf{
		Value: &openfgav1.UsersetTree_Leaf_Computed{Computed: &openfgav1.UsersetTree_Computed{Userset: userset}},
	}}}
}

func TestAnnotateExpand(t *testing.T) {
	c, err := Compile(DefaultScheme())
	if err != nil {
		t.Fatal(err)
	}
	// doc:D#can_edit = editor but not blocked, as Expand reports it.
	tree := &openfgav1.UsersetTree{Root: &openfgav1.UsersetTree_Node{
		Name: "doc:D#can_edit",
		Value: &openfgav1.UsersetTree_Node_Difference{Difference: &openfgav1.UsersetTree_Difference{
			Base: &openfgav1.UsersetTree_Node{Name: "doc:D#editor", Value: &openfgav1.UsersetTree_Node_Union{Union: &openfgav1.UsersetTree_Nodes{Nodes: []*openfgav1.UsersetTree_Node{
				usersLeaf("doc:D#editor", "user:alice", "group:g#member"),
				ttuLeaf("doc:D#editor", "doc:D#parent", "doc:P0#editor"),
				ttuLeaf("doc:D#editor", "doc:D#project", "project:P#editor"),
			}}}},
			Subtract: ttuLeaf("doc:D#blocked", "doc:D#project", "project:P#blocked"),
		}},
	}}
	root := AnnotateExpand(tree, c)
	if root == nil {
		t.Fatal("nil root")
	}
	if root.Object != "doc:D" || root.Relation != "can_edit" || root.Role != "" || root.Operator != OperatorDifference || len(root.Children) != 2 {
		t.Fatalf("root = %+v", root)
	}
	base := root.Children[0]
	if base.Role != "editor" || base.Operator != OperatorUnion || len(base.Children) != 3 {
		t.Fatalf("base = %+v", base)
	}
	direct := base.Children[0]
	if direct.Operator != OperatorDirect || direct.Role != "editor" || len(direct.Users) != 2 || direct.Users[0] != "user:alice" {
		t.Errorf("direct = %+v", direct)
	}
	parent := base.Children[1]
	if parent.Operator != OperatorTupleToUserset || len(parent.Children) != 2 ||
		parent.Children[0].Relation != "parent" || parent.Children[0].Role != "" ||
		parent.Children[1].Object != "doc:P0" || parent.Children[1].Role != "editor" {
		t.Errorf("parent ttu = %+v / %+v", parent, parent.Children)
	}
	project := base.Children[2]
	if project.Children[1].Object != "project:P" || project.Children[1].Relation != "editor" || project.Children[1].Role != "editor" {
		t.Errorf("project ttu = %+v", project.Children[1])
	}
	sub := root.Children[1]
	if sub.Relation != "blocked" || sub.Role != "" || sub.Children[1].Relation != "blocked" {
		t.Errorf("subtract = %+v", sub)
	}

	// Reflected roles and computed rewrites.
	block := AnnotateExpand(&openfgav1.UsersetTree{Root: &openfgav1.UsersetTree_Node{
		Name: "block:X#can_set_status",
		Value: &openfgav1.UsersetTree_Node_Union{Union: &openfgav1.UsersetTree_Nodes{Nodes: []*openfgav1.UsersetTree_Node{
			ttuLeaf("block:X#can_set_status", "block:X#doc", "doc:D#can_edit"),
			computedLeaf("block:X#can_set_status", "block:X#assignee"),
		}}},
	}}, c)
	if block.Children[1].Operator != OperatorComputed || block.Children[1].Children[0].Role != "assignee" {
		t.Errorf("block computed = %+v", block.Children[1])
	}
	if AnnotateExpand(nil, c) != nil || AnnotateExpand(&openfgav1.UsersetTree{}, c) != nil {
		t.Error("empty trees must annotate to nil")
	}
}
