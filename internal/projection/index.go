// Package projection writes the rebuildable tables (pages, blocks,
// block_properties, block_edges) from a materialized doc state
// (specification section 7, "Projection pipeline"). IndexDoc is idempotent:
// the API calls it inside write transactions for structured writes, and the
// index worker calls it for sync pushes and reindexing.
package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/loro"
	"github.com/acx1729/ocean/internal/schema"
)

// Analyzer extracts search text and references from a block's Markdown. The
// Markdown engine provides the real one; PlainAnalyzer is the fallback.
type Analyzer interface {
	Analyze(markdown string) Analysis
}

// Analysis is what the indexer needs from a block's content.
type Analysis struct {
	Text  string
	Refs  []Ref
	Props map[string]string
}

// Ref is an inline reference.
type Ref struct {
	Kind   string // ref, tag, embed
	Target string // page title or block id
}

// PlainAnalyzer treats Markdown as plain text and finds [[links]], #tags and ((refs)).
type PlainAnalyzer struct{}

// Analyze implements Analyzer.
func (PlainAnalyzer) Analyze(md string) Analysis {
	a := Analysis{Text: md}
	for i := 0; i < len(md); i++ {
		switch {
		case strings.HasPrefix(md[i:], "[["):
			if end := strings.Index(md[i+2:], "]]"); end >= 0 {
				target := md[i+2 : i+2+end]
				if p := strings.IndexAny(target, "|#"); p >= 0 {
					target = target[:p]
				}
				if target != "" {
					a.Refs = append(a.Refs, Ref{Kind: "ref", Target: target})
				}
				i += end + 3
			}
		case strings.HasPrefix(md[i:], "(("):
			if end := strings.Index(md[i+2:], "))"); end >= 0 {
				a.Refs = append(a.Refs, Ref{Kind: "ref", Target: md[i+2 : i+2+end]})
				i += end + 3
			}
		case md[i] == '#' && i+1 < len(md) && (i == 0 || md[i-1] == ' ' || md[i-1] == '\n') && md[i+1] != ' ' && md[i+1] != '#':
			j := i + 1
			for j < len(md) && !strings.ContainsRune(" \n\t,.;:!?)", rune(md[j])) {
				j++
			}
			if j > i+1 {
				a.Refs = append(a.Refs, Ref{Kind: "tag", Target: md[i+1 : j]})
			}
			i = j - 1
		}
	}
	return a
}

// Input describes one doc to index.
type Input struct {
	WorkspaceID string
	ProjectID   string
	PageID      string
	DocID       string
	// PortalBlockID is the block in the parent doc that stands for a boundary doc; blocks of the
	// boundary hang under it. Empty for page docs.
	PortalBlockID string
	State         *loro.State
	Seq           int64
	Actor         string
	Now           time.Time
	Snap          *schema.Snapshot
	Analyzer      Analyzer
	// CreatedAt of the page (for the pages row).
	CreatedAt time.Time
	DeletedAt *time.Time
}

// NormalizeTitle canonicalizes a title for lookups and link resolution.
func NormalizeTitle(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// labelOf turns a uuid into an ltree label.
func labelOf(id string) string { return strings.ReplaceAll(id, "-", "") }

// IndexDoc rewrites the projection rows of one doc from its state.
func IndexDoc(ctx context.Context, tx pgx.Tx, in Input) error {
	if in.Analyzer == nil {
		in.Analyzer = PlainAnalyzer{}
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	st := in.State
	if in.PortalBlockID == "" {
		if err := upsertPage(ctx, tx, in); err != nil {
			return err
		}
	}
	// Root path: page docs hang blocks under the page block; boundaries under their portal.
	rootPath := labelOf(in.PageID)
	rootParent := in.PageID
	if in.PortalBlockID != "" {
		var portalPath string
		err := tx.QueryRow(ctx, `SELECT wbs_path::text FROM blocks WHERE workspace_id = $1 AND id = $2`, in.WorkspaceID, in.PortalBlockID).Scan(&portalPath)
		if err == nil {
			rootPath = portalPath
		} else {
			rootPath = labelOf(in.PageID) + "." + labelOf(in.PortalBlockID)
		}
		rootParent = in.PortalBlockID
	}

	type row struct {
		id, parent, rank, path string
		depth                  int
		typeID                 *string
		key                    *string
		markdown, text         string
		attrs                  []byte
		portal                 *string
		createdBy, createdAt   string
	}
	var rows []row
	var portalRows []row
	var propRows []propRow
	var edgeRows []edgeRow
	seen := map[string]bool{}
	var walk func(n *loro.Node, parentID, parentPath string, depth int)
	walk = func(n *loro.Node, parentID, parentPath string, depth int) {
		if seen[n.ID] || n.ID == "" {
			return
		}
		seen[n.ID] = true
		path := parentPath + "." + labelOf(n.ID)
		if n.PortalDocID != "" {
			// The boundary doc owns this block's row; the parent only places it.
			p := n.PortalDocID
			portalRows = append(portalRows, row{id: n.ID, parent: parentID, rank: n.FractionalIndex, path: path, depth: depth, portal: &p})
			return
		}
		an := in.Analyzer.Analyze(n.Content)
		props := map[string]any{}
		for k, v := range n.Props {
			props[k] = v
		}
		attrs, _ := json.Marshal(props)
		r := row{id: n.ID, parent: parentID, rank: n.FractionalIndex, path: path, depth: depth, markdown: n.Content, text: an.Text, attrs: attrs,
			createdBy: n.CreatedBy, createdAt: n.CreatedAt}
		if n.TypeID != "" {
			t := n.TypeID
			r.typeID = &t
		}
		if k, ok := n.Props[schema.KeyProp].(string); ok && k != "" {
			r.key = &k
		}
		if n.PortalDocID != "" {
			p := n.PortalDocID
			r.portal = &p
		}
		if in.PortalBlockID != "" && n.ID == in.PortalBlockID {
			// The boundary root keeps pointing at its own doc so lookups see the split.
			p := in.DocID
			r.portal = &p
		}
		rows = append(rows, r)
		propRows = append(propRows, propertyRows(in.Snap, n.ID, props)...)
		for _, ref := range an.Refs {
			title := ref.Target
			edgeRows = append(edgeRows, edgeRow{source: n.ID, kind: ref.Kind, targetTitle: &title})
		}
		edgeRows = append(edgeRows, relationEdges(in.Snap, n.ID, props)...)
		for _, ch := range n.Children {
			walk(ch, n.ID, path, depth+1)
		}
	}
	baseDepth := strings.Count(rootPath, ".") + 1
	for _, root := range st.Roots {
		walk(root, rootParent, rootPath, baseDepth)
	}
	// The page block itself is a row too (title as content) so queries can address pages.
	if in.PortalBlockID == "" {
		attrs, _ := json.Marshal(publicProps(st.Meta.Props))
		var typeID *string
		if st.Meta.TypeID != "" {
			t := st.Meta.TypeID
			typeID = &t
		}
		var key *string
		if k, ok := st.Meta.Props[schema.KeyProp].(string); ok && k != "" {
			key = &k
		}
		rows = append(rows, row{id: in.PageID, parent: "", rank: "", path: rootPath, depth: 0, typeID: typeID, key: key, markdown: st.Meta.Title, text: st.Meta.Title, attrs: attrs, createdAt: in.CreatedAt.UTC().Format(time.RFC3339Nano), createdBy: in.Actor})
		propRows = append(propRows, propertyRows(in.Snap, in.PageID, st.Meta.Props)...)
		edgeRows = append(edgeRows, relationEdges(in.Snap, in.PageID, st.Meta.Props)...)
	}

	ids := make([]string, len(rows))
	parents := make([]*string, len(rows))
	ranks := make([]string, len(rows))
	paths := make([]string, len(rows))
	depths := make([]int32, len(rows))
	typeIDs := make([]*string, len(rows))
	keys := make([]*string, len(rows))
	markdowns := make([]string, len(rows))
	texts := make([]string, len(rows))
	attrsList := make([][]byte, len(rows))
	portals := make([]*string, len(rows))
	createdBys := make([]string, len(rows))
	createdAts := make([]time.Time, len(rows))
	for i, r := range rows {
		ids[i], ranks[i], paths[i], depths[i], typeIDs[i], keys[i], markdowns[i], texts[i], attrsList[i], portals[i] = r.id, r.rank, r.path, int32(r.depth), r.typeID, r.key, r.markdown, r.text, r.attrs, r.portal
		if r.parent != "" {
			p := r.parent
			parents[i] = &p
		}
		createdBys[i] = r.createdBy
		if createdBys[i] == "" {
			createdBys[i] = in.Actor
		}
		if t, err := time.Parse(time.RFC3339Nano, r.createdAt); err == nil {
			createdAts[i] = t
		} else {
			createdAts[i] = in.Now
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO blocks (workspace_id, id, project_id, page_id, doc_id, parent_block_id, rank, wbs_path, depth, type_id, key, markdown, text,
		                    attributes, portal_doc_id, has_acl, indexed_seq, created_by, updated_by, created_at, updated_at, deleted_at)
		SELECT $1, u.id, $2, $3, $4, u.parent, u.rank, u.path::ltree, u.depth, u.type_id, u.key, u.markdown, u.text, u.attrs, u.portal, u.portal IS NOT NULL, $5, u.created_by, $6, u.created_at, $7, $8
		FROM unnest($9::uuid[], $10::uuid[], $11::text[], $12::text[], $13::int[], $14::uuid[], $15::text[], $16::text[], $17::text[], $18::jsonb[], $19::uuid[], $20::text[], $21::timestamptz[])
		  AS u(id, parent, rank, path, depth, type_id, key, markdown, text, attrs, portal, created_by, created_at)
		ON CONFLICT (workspace_id, id) DO UPDATE SET
		  project_id = EXCLUDED.project_id, page_id = EXCLUDED.page_id, doc_id = EXCLUDED.doc_id, parent_block_id = EXCLUDED.parent_block_id,
		  rank = EXCLUDED.rank, wbs_path = EXCLUDED.wbs_path, depth = EXCLUDED.depth, type_id = EXCLUDED.type_id, key = EXCLUDED.key,
		  markdown = EXCLUDED.markdown, text = EXCLUDED.text, attributes = EXCLUDED.attributes, portal_doc_id = EXCLUDED.portal_doc_id, has_acl = EXCLUDED.has_acl,
		  indexed_seq = EXCLUDED.indexed_seq, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at, deleted_at = EXCLUDED.deleted_at
		WHERE blocks.markdown IS DISTINCT FROM EXCLUDED.markdown OR blocks.attributes IS DISTINCT FROM EXCLUDED.attributes
		   OR blocks.parent_block_id IS DISTINCT FROM EXCLUDED.parent_block_id OR blocks.rank IS DISTINCT FROM EXCLUDED.rank
		   OR blocks.type_id IS DISTINCT FROM EXCLUDED.type_id OR blocks.key IS DISTINCT FROM EXCLUDED.key OR blocks.deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
		   OR blocks.doc_id IS DISTINCT FROM EXCLUDED.doc_id OR blocks.portal_doc_id IS DISTINCT FROM EXCLUDED.portal_doc_id OR blocks.wbs_path IS DISTINCT FROM EXCLUDED.wbs_path`,
		in.WorkspaceID, in.ProjectID, in.PageID, in.DocID, in.Seq, in.Actor, in.Now, in.DeletedAt,
		ids, parents, ranks, paths, depths, typeIDs, keys, markdowns, texts, attrsList, portals, createdBys, createdAts); err != nil {
		return fmt.Errorf("upsert blocks: %w", err)
	}
	// Blocks that left this doc (deleted or moved) disappear from the projection.
	if _, err := tx.Exec(ctx, `DELETE FROM blocks WHERE workspace_id = $1 AND doc_id = $2 AND NOT (id = ANY($3::uuid[]))`, in.WorkspaceID, in.DocID, ids); err != nil {
		return fmt.Errorf("prune blocks: %w", err)
	}
	// Portals: place the boundary block (owned by its own doc) and move its subtree's paths.
	for _, p := range portalRows {
		var parent *string
		if p.parent != "" {
			pp := p.parent
			parent = &pp
		}
		if _, err := tx.Exec(ctx, `INSERT INTO blocks (workspace_id, id, project_id, page_id, doc_id, parent_block_id, rank, wbs_path, depth, portal_doc_id, has_acl, indexed_seq, created_by, updated_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::ltree, $9, $5, true, $10, $11, $11, $12, $12)
			ON CONFLICT (workspace_id, id) DO UPDATE SET parent_block_id = EXCLUDED.parent_block_id, rank = EXCLUDED.rank, wbs_path = EXCLUDED.wbs_path,
			  depth = EXCLUDED.depth, page_id = EXCLUDED.page_id, portal_doc_id = EXCLUDED.portal_doc_id, has_acl = true, updated_at = EXCLUDED.updated_at`,
			in.WorkspaceID, p.id, in.ProjectID, in.PageID, *p.portal, parent, p.rank, p.path, p.depth, in.Seq, in.Actor, in.Now); err != nil {
			return fmt.Errorf("place portal: %w", err)
		}
		// Descendants inside the boundary follow the portal's new position.
		if _, err := tx.Exec(ctx, `UPDATE blocks SET wbs_path = $3::ltree || subpath(wbs_path, index(wbs_path, $4::ltree) + 1), depth = $5 + nlevel(wbs_path) - index(wbs_path, $4::ltree) - 1, page_id = $6
			WHERE workspace_id = $1 AND doc_id = $2 AND id <> $7 AND wbs_path ~ ('*.' || $8 || '.*')::lquery`,
			in.WorkspaceID, *p.portal, p.path, labelOf(p.id), p.depth, in.PageID, p.id, labelOf(p.id)); err != nil {
			return fmt.Errorf("move boundary paths: %w", err)
		}
	}
	if err := writeProperties(ctx, tx, in.WorkspaceID, ids, propRows); err != nil {
		return err
	}
	if err := writeEdges(ctx, tx, in, ids, edgeRows); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE docs SET indexed_seq = GREATEST(indexed_seq, $3) WHERE workspace_id = $1 AND id = $2`, in.WorkspaceID, in.DocID, in.Seq); err != nil {
		return err
	}
	return nil
}

func publicProps(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

func upsertPage(ctx context.Context, tx pgx.Tx, in Input) error {
	st := in.State
	var journal *time.Time
	if st.Meta.JournalDate != "" {
		if t, err := time.Parse("2006-01-02", st.Meta.JournalDate); err == nil {
			journal = &t
		}
	}
	format := st.Meta.Format
	if format == "" {
		format = "markdown"
	}
	var parent *string
	if p, ok := st.Meta.Props["_parent_page_id"].(string); ok && p != "" {
		parent = &p
	}
	var typeID *string
	if st.Meta.TypeID != "" {
		t := st.Meta.TypeID
		typeID = &t
	}
	var key *string
	if k, ok := st.Meta.Props[schema.KeyProp].(string); ok && k != "" {
		key = &k
	}
	attrs, _ := json.Marshal(publicProps(st.Meta.Props))
	norm := NormalizeTitle(st.Meta.Title)
	created := in.CreatedAt
	if created.IsZero() {
		created = in.Now
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO pages (workspace_id, id, project_id, doc_id, title, title_norm, parent_page_id, journal_date, format, type_id, key, icon, attributes,
		                   title_conflict, indexed_seq, deleted_at, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13,
		        EXISTS (SELECT 1 FROM pages p WHERE p.workspace_id = $1 AND p.project_id = $3 AND p.title_norm = $6 AND p.id <> $2 AND p.deleted_at IS NULL),
		        $14, $15, $16, $17, $18)
		ON CONFLICT (workspace_id, id) DO UPDATE SET
		  title = EXCLUDED.title, title_norm = EXCLUDED.title_norm, parent_page_id = EXCLUDED.parent_page_id, journal_date = EXCLUDED.journal_date,
		  format = EXCLUDED.format, type_id = EXCLUDED.type_id, key = EXCLUDED.key, icon = EXCLUDED.icon, attributes = EXCLUDED.attributes,
		  title_conflict = EXCLUDED.title_conflict, indexed_seq = EXCLUDED.indexed_seq, deleted_at = EXCLUDED.deleted_at, updated_at = EXCLUDED.updated_at`,
		in.WorkspaceID, in.PageID, in.ProjectID, in.DocID, st.Meta.Title, norm, parent, journal, format, typeID, key, st.Meta.Icon, attrs,
		in.Seq, in.DeletedAt, in.Actor, created, in.Now)
	if err != nil {
		return fmt.Errorf("upsert page: %w", err)
	}
	// Aliases: the alias property lists alternative titles.
	if _, err := tx.Exec(ctx, `DELETE FROM page_aliases WHERE workspace_id = $1 AND page_id = $2`, in.WorkspaceID, in.PageID); err != nil {
		return err
	}
	if in.Snap != nil {
		if ap, ok := in.Snap.PropsByName["alias"]; ok {
			if raw, ok := st.Meta.Props[ap.ID].(string); ok && raw != "" {
				for _, a := range strings.Split(raw, ",") {
					if n := NormalizeTitle(a); n != "" {
						if _, err := tx.Exec(ctx, `INSERT INTO page_aliases (workspace_id, alias_norm, page_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, in.WorkspaceID, n, in.PageID); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// DeleteDoc removes the projection rows owned by one doc (a merged or deleted boundary).
func DeleteDoc(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	for _, q := range []string{
		`DELETE FROM block_properties WHERE workspace_id = $1 AND block_id IN (SELECT id FROM blocks WHERE workspace_id = $1 AND doc_id = $2)`,
		`DELETE FROM block_edges WHERE workspace_id = $1 AND source_block_id IN (SELECT id FROM blocks WHERE workspace_id = $1 AND doc_id = $2)`,
		`DELETE FROM blocks WHERE workspace_id = $1 AND doc_id = $2`,
	} {
		if _, err := tx.Exec(ctx, q, workspaceID, docID); err != nil {
			return err
		}
	}
	return nil
}

// DeletePage removes every projection row of a purged page.
func DeletePage(ctx context.Context, tx pgx.Tx, workspaceID, pageID string) error {
	for _, q := range []string{
		`DELETE FROM block_properties WHERE workspace_id = $1 AND block_id IN (SELECT id FROM blocks WHERE workspace_id = $1 AND page_id = $2)`,
		`DELETE FROM block_edges WHERE workspace_id = $1 AND source_block_id IN (SELECT id FROM blocks WHERE workspace_id = $1 AND page_id = $2)`,
		`DELETE FROM blocks WHERE workspace_id = $1 AND page_id = $2`,
		`DELETE FROM page_aliases WHERE workspace_id = $1 AND page_id = $2`,
		`DELETE FROM pages WHERE workspace_id = $1 AND id = $2`,
	} {
		if _, err := tx.Exec(ctx, q, workspaceID, pageID); err != nil {
			return err
		}
	}
	return nil
}
