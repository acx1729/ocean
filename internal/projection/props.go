package projection

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/schema"
)

type propRow struct {
	block, prop string
	ord         int
	text        *string
	num         *float64
	date        *time.Time
	boolean     *bool
	ref         *string
	principal   *string
}

type edgeRow struct {
	source      string
	kind        string // ref, tag, embed, relation
	relationID  *string
	target      *string
	targetTitle *string
}

// propertyRows turns a bag into typed block_properties rows.
func propertyRows(snap *schema.Snapshot, blockID string, props map[string]any) []propRow {
	if snap == nil {
		return nil
	}
	var out []propRow
	for id, val := range props {
		p, ok := snap.Props[id]
		if !ok || val == nil {
			continue
		}
		switch p.Kind {
		case schema.KindText, schema.KindURL, schema.KindSelect:
			if s, ok := val.(string); ok {
				out = append(out, propRow{block: blockID, prop: id, text: &s})
			}
		case schema.KindNumber:
			if f, ok := toFloat(val); ok {
				out = append(out, propRow{block: blockID, prop: id, num: &f})
			}
		case schema.KindCheckbox:
			if b, ok := val.(bool); ok {
				out = append(out, propRow{block: blockID, prop: id, boolean: &b})
			}
		case schema.KindDate:
			if s, ok := val.(string); ok {
				if t, err := schema.ParseDate(s); err == nil {
					out = append(out, propRow{block: blockID, prop: id, date: &t})
				}
			}
		case schema.KindUser:
			if s, ok := val.(string); ok {
				out = append(out, propRow{block: blockID, prop: id, principal: &s})
			}
		case schema.KindMultiSelect:
			for i, s := range setValues(val) {
				s := s
				out = append(out, propRow{block: blockID, prop: id, ord: i, text: &s})
			}
		case schema.KindRelation:
			for i, s := range setValues(val) {
				s := s
				out = append(out, propRow{block: blockID, prop: id, ord: i, ref: &s})
			}
		}
	}
	return out
}

// relationEdges turns relation properties into relation edges.
func relationEdges(snap *schema.Snapshot, blockID string, props map[string]any) []edgeRow {
	if snap == nil {
		return nil
	}
	var out []edgeRow
	for id, val := range props {
		p, ok := snap.Props[id]
		if !ok || p.Kind != schema.KindRelation {
			continue
		}
		rel := p.RelationTypeID()
		var relPtr *string
		if rel != "" {
			relPtr = &rel
		}
		for _, target := range setValues(val) {
			t := target
			out = append(out, edgeRow{source: blockID, kind: "relation", relationID: relPtr, target: &t})
		}
	}
	return out
}

// setValues returns the members of a LoroMap<id,true> set or a list, sorted.
func setValues(val any) []string {
	var out []string
	switch x := val.(type) {
	case map[string]any:
		for k, v := range x {
			if b, ok := v.(bool); ok && b {
				out = append(out, k)
			}
		}
	case []any:
		for _, v := range x {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, x...)
	case string:
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	}
	return 0, false
}

func writeProperties(ctx context.Context, tx pgx.Tx, ws string, blockIDs []string, rows []propRow) error {
	if _, err := tx.Exec(ctx, `DELETE FROM block_properties WHERE workspace_id = $1 AND block_id = ANY($2::uuid[])`, ws, blockIDs); err != nil {
		return fmt.Errorf("clear properties: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	blocks, props, ords := make([]string, n), make([]string, n), make([]int32, n)
	texts, nums, dates, bools, refs, principals := make([]*string, n), make([]*float64, n), make([]*time.Time, n), make([]*bool, n), make([]*string, n), make([]*string, n)
	for i, r := range rows {
		blocks[i], props[i], ords[i] = r.block, r.prop, int32(r.ord)
		texts[i], nums[i], dates[i], bools[i], refs[i], principals[i] = r.text, r.num, r.date, r.boolean, r.ref, r.principal
	}
	_, err := tx.Exec(ctx, `INSERT INTO block_properties (workspace_id, block_id, property_id, ord, value_text, value_number, value_date, value_bool, value_ref, value_principal)
		SELECT $1, u.block, u.prop, u.ord, u.t, u.n, u.d, u.b, u.r, u.p
		FROM unnest($2::uuid[], $3::uuid[], $4::int[], $5::text[], $6::float8[], $7::timestamptz[], $8::bool[], $9::uuid[], $10::text[]) AS u(block, prop, ord, t, n, d, b, r, p)
		ON CONFLICT DO NOTHING`, ws, blocks, props, ords, texts, nums, dates, bools, refs, principals)
	if err != nil {
		return fmt.Errorf("insert properties: %w", err)
	}
	return nil
}

func writeEdges(ctx context.Context, tx pgx.Tx, in Input, blockIDs []string, rows []edgeRow) error {
	if _, err := tx.Exec(ctx, `DELETE FROM block_edges WHERE workspace_id = $1 AND source_block_id = ANY($2::uuid[])`, in.WorkspaceID, blockIDs); err != nil {
		return fmt.Errorf("clear edges: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	sources, kinds, rels, targets, titles, origins := make([]string, n), make([]string, n), make([]*string, n), make([]*string, n), make([]*string, n), make([]string, n)
	for i, r := range rows {
		sources[i], kinds[i], rels[i] = r.source, r.kind, r.relationID
		origins[i] = "inline"
		if r.kind == "relation" {
			origins[i] = "property"
		}
		target := r.target
		title := r.targetTitle
		// Inline refs name a page by title or a block by id; resolve titles and aliases now.
		if target == nil && title != nil {
			if isUUID(*title) {
				t := *title
				target = &t
				title = nil
			} else {
				var id string
				norm := NormalizeTitle(*title)
				err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE workspace_id = $1 AND project_id = $2 AND title_norm = $3 AND deleted_at IS NULL
					UNION ALL SELECT page_id FROM page_aliases WHERE workspace_id = $1 AND alias_norm = $3 LIMIT 1`, in.WorkspaceID, in.ProjectID, norm).Scan(&id)
				if err == nil {
					target = &id
				}
			}
		}
		targets[i], titles[i] = target, title
	}
	_, err := tx.Exec(ctx, `INSERT INTO block_edges (workspace_id, source_block_id, target_block_id, edge_kind, relation_type_id, origin, target_title, indexed_seq)
		SELECT $1, u.s, u.t, u.k, u.r, u.o, u.title, $2
		FROM unnest($3::uuid[], $4::uuid[], $5::text[], $6::uuid[], $7::text[], $8::text[]) AS u(s, t, k, r, o, title)
		ON CONFLICT DO NOTHING`, in.WorkspaceID, in.Seq, sources, targets, kinds, rels, origins, titles)
	if err != nil {
		return fmt.Errorf("insert edges: %w", err)
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// ResolveUnresolved links edges whose target title now matches a page (called
// after a page is created or renamed).
func ResolveUnresolved(ctx context.Context, tx pgx.Tx, workspaceID, projectID, pageID, titleNorm string) error {
	_, err := tx.Exec(ctx, `UPDATE block_edges e SET target_block_id = $3
		FROM blocks b WHERE e.workspace_id = $1 AND e.target_block_id IS NULL AND lower(e.target_title) = $4
		  AND b.workspace_id = e.workspace_id AND b.id = e.source_block_id AND b.project_id = $2`, workspaceID, projectID, pageID, titleNorm)
	return err
}
