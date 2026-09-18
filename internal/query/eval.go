package query

import (
	"errors"
	"fmt"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/functions"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// ErrEval wraps run-time evaluation errors reported by Program.Eval; a block
// whose evaluation fails does not match, mirroring the SQL path.
var ErrEval = errors.New("query: evaluation error")

// Resolver answers the graph and search questions the custom functions ask
// during in-process evaluation. All ids are canonical lowercase uuids; edge
// lists contain resolved targets only.
type Resolver interface {
	HasEdge(source, relationTypeID, target string) bool // target "" = any edge of that relation
	EdgesOut(source, relationTypeID string) []string
	EdgesIn(target, relationTypeID string) []string
	IsDescendantOf(block, ancestor string) bool
	IsAncestorOf(block, descendant string) bool
	Refs(source, target string) bool // inline ref edge source -> target
	ReferencedBy(target, source string) bool
	Search(text, query string) bool // approximate websearch_to_tsquery semantics
	InCollection(block, collection string) bool
}

// BlockInput is one block of the projection for in-process evaluation.
// Props is keyed by property name and holds typed Go values: string
// (text, url, user; option id or name for select), float64 (number),
// time.Time (date), bool (checkbox) and []string (relation targets, or
// option ids or names for multi_select). Path lists ancestor ids root-first
// including the block itself.
type BlockInput struct {
	ID        string
	PageID    string
	DocID     string
	ProjectID string
	ParentID  string
	Key       string
	TypeID    string
	Text      string
	Path      []string
	Depth     int
	IsPage    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
	UpdatedBy string
	Props     map[string]any
}

// Program is a checked expression ready for in-process evaluation.
type Program struct {
	env       *Env
	ast       *cel.Ast
	costLimit uint64
}

// Program parses and checks an expression for in-process evaluation. It
// applies the same static rules as Compile, so a Program exists only for
// expressions the SQL path accepts.
func (e *Env) Program(src string) (*Program, error) {
	checked, errs := e.analyze(src)
	if errs != nil {
		return nil, errs.asError()
	}
	if _, _, err := e.dryRun(checked); err != nil {
		return nil, err
	}
	return &Program{env: e, ast: checked, costLimit: e.programCostLimit(checked)}, nil
}

// Eval evaluates the expression against one block. A run-time error makes
// the block not match and is reported wrapped in ErrEval.
func (p *Program) Eval(b BlockInput, r Resolver, caller string, now time.Time) (bool, error) {
	val, _, err := p.run(&b, r, caller, now, cel.OptOptimize)
	if err != nil {
		return false, err
	}
	out, ok := val.(types.Bool)
	if !ok {
		return false, fmt.Errorf("%w: expression returned %s", ErrEval, val.Type().TypeName())
	}
	return bool(out), nil
}

// Explain evaluates the expression exhaustively and returns the value
// observed at every expression id (cel.OptExhaustiveEval); errors appear as
// strings prefixed with "error: ".
func (p *Program) Explain(b BlockInput, r Resolver, caller string, now time.Time) (map[int64]any, error) {
	val, det, err := p.run(&b, r, caller, now, cel.OptExhaustiveEval)
	out := map[int64]any{}
	if det != nil && det.State() != nil {
		for _, id := range det.State().IDs() {
			if v, ok := det.State().Value(id); ok {
				out[id] = nativeOf(v)
			}
		}
	}
	if err != nil {
		if len(out) == 0 {
			return nil, err
		}
		out[p.ast.NativeRep().Expr().ID()] = "error: " + err.Error()
		return out, nil
	}
	out[p.ast.NativeRep().Expr().ID()] = nativeOf(val)
	return out, nil
}

// run plans the program with bindings closed over the evaluation context.
func (p *Program) run(b *BlockInput, r Resolver, caller string, now time.Time, opts cel.EvalOption) (ref.Val, *cel.EvalDetails, error) {
	if r == nil {
		return nil, nil, fmt.Errorf("query: resolver is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ctx := &evalCtx{env: p.env, block: b, r: r, caller: caller, now: now}
	bindings := make([]*functions.Overload, 0, len(fnTable))
	for _, f := range fnTable {
		bindings = append(bindings, f.binding(ctx))
	}
	prg, err := p.env.cel.Program(p.ast, cel.Functions(bindings...), cel.EvalOptions(opts), cel.CostLimit(p.costLimit))
	if err != nil {
		return nil, nil, fmt.Errorf("query: program: %w", err)
	}
	act, err := p.activation(ctx)
	if err != nil {
		return nil, nil, err
	}
	val, det, err := prg.Eval(act)
	if err != nil {
		return val, det, fmt.Errorf("%w: %v", ErrEval, err)
	}
	return val, det, nil
}

// activation binds the environment variables for one block.
func (p *Program) activation(c *evalCtx) (map[string]any, error) {
	b := c.block
	props, err := newPropsVal(p.env.ix, b.Props)
	if err != nil {
		return nil, err
	}
	path := b.Path
	if path == nil {
		path = []string{}
	}
	return map[string]any{
		"id":         b.ID,
		"page_id":    b.PageID,
		"doc_id":     b.DocID,
		"project_id": b.ProjectID,
		"parent_id":  b.ParentID,
		"key":        b.Key,
		"type":       p.env.ix.typeName(b.TypeID),
		"type_id":    b.TypeID,
		"text":       b.Text,
		"props":      props,
		"path":       path,
		"depth":      int64(b.Depth),
		"is_page":    b.IsPage,
		"created_at": b.CreatedAt,
		"updated_at": b.UpdatedAt,
		"created_by": b.CreatedBy,
		"updated_by": b.UpdatedBy,
		"now":        c.now,
	}, nil
}

// nativeOf converts a CEL value into a plain Go value for Explain output.
func nativeOf(v ref.Val) any {
	switch x := v.(type) {
	case nil:
		return nil
	case *types.Err:
		return "error: " + x.Error()
	case *types.Unknown:
		return "unknown"
	case types.Timestamp:
		return x.Time
	case types.Duration:
		return x.Duration
	case *propsVal:
		return x.Value()
	case traits.Lister:
		var out []any
		it := x.Iterator()
		for it.HasNext() == types.True {
			out = append(out, nativeOf(it.Next()))
		}
		return out
	}
	return v.Value()
}
