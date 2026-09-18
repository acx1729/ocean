package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	kbv1 "github.com/acx1729/ocean/gen/kb/v1"
	"github.com/acx1729/ocean/gen/kb/v1/kbv1connect"
	"github.com/acx1729/ocean/internal/apierr"
	"github.com/acx1729/ocean/internal/auth"
	"github.com/acx1729/ocean/internal/authz"
	"github.com/acx1729/ocean/internal/loro"
	"github.com/acx1729/ocean/internal/projection"
	"github.com/acx1729/ocean/internal/schema"
	"github.com/acx1729/ocean/internal/truth"
)

// PagesService implements kb.v1.PagesService.
type PagesService struct{ *core }

var _ kbv1connect.PagesServiceHandler = (*PagesService)(nil)

func validJournalDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// Create makes a page (or any doc-owning block) with its own doc.
func (s *PagesService) Create(ctx context.Context, req *connect.Request[kbv1.CreatePageRequest]) (*connect.Response[kbv1.CreatePageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.Edit, authz.Project(pid)); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Msg.GetTitle())
	if req.Msg.GetJournalDate() != "" && !validJournalDate(req.Msg.GetJournalDate()) {
		return nil, apierr.InvalidArgument("journal_date", "YYYY-MM-DD")
	}
	if title == "" {
		title = req.Msg.GetJournalDate()
	}
	if title == "" || len(title) > 1000 {
		return nil, apierr.InvalidArgument("title", "1 to 1000 characters")
	}
	if int64(len(req.Msg.GetMarkdown())) > s.Config.Limits.PageDocBytes {
		return nil, apierr.ResourceExhausted("markdown", "exceeds the page size limit")
	}
	res, err := idempotent(ctx, s.core, id, ws, req, func() *kbv1.CreatePageResponse { return &kbv1.CreatePageResponse{} },
		func(record func(context.Context, pgx.Tx, *kbv1.CreatePageResponse) error) (*kbv1.CreatePageResponse, error) {
			return s.create(ctx, id, ws, pid, title, req.Msg, record)
		})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (s *PagesService) create(ctx context.Context, id *auth.Identity, ws, pid, title string, in *kbv1.CreatePageRequest, record func(context.Context, pgx.Tx, *kbv1.CreatePageResponse) error) (*kbv1.CreatePageResponse, error) {
	pageID := newID()
	out := &kbv1.CreatePageResponse{}
	format := formatName(in.GetFormat())
	now := s.Clock()
	row := &truth.DocRow{WorkspaceID: ws, ID: pageID, ProjectID: pid, Kind: truth.KindPage, PageID: pageID}
	var snap *schema.Snapshot
	results, err := s.Mat.CreateAndMutate(ctx, tenant(id, ws), row, truth.Options{Actor: id.Principal, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
		r := results[0]
		fresh, err := s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return err
		}
		if err := s.index(ctx, tx, fresh, r.State, r.Seq, id.Principal, snap); err != nil {
			return err
		}
		if err := projection.ResolveUnresolved(ctx, tx, ws, pid, pageID, projection.NormalizeTitle(title)); err != nil {
			return err
		}
		out.Page = s.pageProto(r.State, fresh)
		out.Blocks = s.flatten(r.State.Roots, infoOf(fresh), 0)
		s.audit(ctx, tx, id, ws, "page.create", "page", pageID, map[string]any{"type_id": out.Page.TypeId, "key": out.Page.Key})
		return record(ctx, tx, out)
	}}, func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation) error {
		var err error
		snap, err = s.Schema.Load(ctx, tx, ws, pid)
		if err != nil {
			return err
		}
		typeID := in.GetTypeId()
		var t *schema.Type
		if typeID == "" {
			t = snap.TypesByName[schema.TypePage]
		} else {
			var ok bool
			if t, ok = snap.Type(typeID); !ok {
				return apierr.InvalidArgument("type_id", "unknown type")
			}
		}
		if t == nil {
			return apierr.FailedPrecondition("project has no page type")
		}
		if !snap.OwnsDoc(t.ID) {
			return apierr.InvalidArgument("type_id", "type does not own a doc; create it as a block instead")
		}
		props, err := normalizeProps(snap, t.ID, fromStruct(in.GetProps()), false)
		if err != nil {
			return err
		}
		if snap.Numbered(t.ID) {
			key, err := s.allocateKey(ctx, tx, ws, pid, pageID)
			if err != nil {
				return err
			}
			props[schema.KeyProp] = key
		}
		if parent := in.GetParentPageId(); parent != "" {
			prow, err := s.Truth.GetDoc(ctx, tx, ws, parent)
			if err != nil || prow.ProjectID != pid || prow.Kind != truth.KindPage {
				return apierr.InvalidArgument("parent_page_id", "unknown page in this project")
			}
			props["_parent_page_id"] = parent
		}
		mu.Add(loro.MetaSet("title", title), loro.MetaSet("format", format), loro.MetaSet("type_id", t.ID))
		if in.GetIcon() != "" {
			mu.Add(loro.MetaSet("icon", in.GetIcon()))
		}
		if in.GetJournalDate() != "" {
			mu.Add(loro.MetaSet("journal_date", in.GetJournalDate()))
		}
		for k, v := range props {
			mu.Add(loro.MetaPropSet(k, v))
		}
		specs := s.Splitter.Split(in.GetMarkdown(), format == "outliner")
		s.createFromSpecs(mu, specs, "", -1, id.Principal, now, snap, "", "")
		mu.Emit(truth.EventPageCreated, map[string]any{"page_id": pageID, "type_id": t.ID})
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	s.notify(ws, []*truth.Result{results}, id.Principal)
	return out, nil
}

// resolvePageRef turns page_id | uri | key into a page id.
func (s *PagesService) resolvePageRef(ctx context.Context, tx pgx.Tx, ws string, req *kbv1.GetPageRequest) (string, error) {
	switch ref := req.GetRef().(type) {
	case *kbv1.GetPageRequest_PageId:
		if !validUUID(ref.PageId) {
			return "", apierr.NotFound("page", ref.PageId)
		}
		return ref.PageId, nil
	case *kbv1.GetPageRequest_Uri:
		id, ok := parseURI(ws, ref.Uri)
		if !ok {
			return "", apierr.NotFound("page", ref.Uri)
		}
		return id, nil
	case *kbv1.GetPageRequest_Key:
		return s.blockByKey(ctx, tx, ws, ref.Key)
	}
	return "", apierr.InvalidArgument("ref", "page_id, uri or key is required")
}

// Get returns a page with its blocks (tree) or as Markdown, from the live doc.
func (s *PagesService) Get(ctx context.Context, req *connect.Request[kbv1.GetPageRequest]) (*connect.Response[kbv1.GetPageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws := req.Msg.GetWorkspaceId()
	if err := s.check(ctx, id, ws, authz.View, authz.Workspace(ws)); err != nil {
		return nil, err
	}
	out := &kbv1.GetPageResponse{}
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		pageID, err := s.resolvePageRef(ctx, tx, ws, req.Msg)
		if err != nil {
			return err
		}
		row, err := s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return apierr.NotFound("page", pageID)
		}
		if err := s.check(ctx, id, ws, authz.View, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return err
		}
		st, row, err := s.Mat.Read(ctx, tx, ws, row.ID)
		if err != nil {
			return err
		}
		out.Page = s.pageProto(st, row)
		out.IndexedSeq = row.IndexedSeq
		xs, err := s.pageTree(ctx, tx, id, row, st, int(req.Msg.GetDepth()))
		if err != nil {
			return err
		}
		if req.Msg.GetFormat() == kbv1.ContentFormat_CONTENT_FORMAT_MARKDOWN {
			out.Markdown = s.Splitter.Join(specsOfX(xs), st.Meta.Format == "outliner")
		} else {
			out.Blocks = s.flattenX(xs)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// List reads the pages projection with keyset pagination.
func (s *PagesService) List(ctx context.Context, req *connect.Request[kbv1.ListPagesRequest]) (*connect.Response[kbv1.ListPagesResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if pid == "" {
		return nil, apierr.InvalidArgument("project_id", "required")
	}
	if err := s.check(ctx, id, ws, authz.View, authz.Project(pid)); err != nil {
		return nil, err
	}
	size := pageSize(req.Msg.GetPageSize())
	cur, err := decodeCursor(req.Msg.GetCursor(), 2)
	if err != nil {
		return nil, err
	}
	var afterT time.Time
	afterID := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if cur[0] != "" {
		afterT, err = time.Parse(time.RFC3339Nano, cur[0])
		if err != nil {
			return nil, apierr.InvalidArgument("cursor", "invalid")
		}
		afterID = cur[1]
	} else {
		afterT = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	out := &kbv1.ListPagesResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, doc_id, title, COALESCE(parent_page_id::text, ''), journal_date, format, COALESCE(type_id::text, ''), COALESCE(key, ''), COALESCE(icon, ''),
				attributes, title_conflict, indexed_seq, deleted_at, created_at, updated_at
			FROM pages WHERE workspace_id = $1 AND project_id = $2
			  AND ($3 OR deleted_at IS NULL)
			  AND ($4 = '' OR parent_page_id = $4::uuid)
			  AND (NOT $5 OR journal_date IS NOT NULL)
			  AND ($6 = '' OR title_norm LIKE $6 || '%')
			  AND ($7 = '' OR title_norm = $7)
			  AND (updated_at, id) < ($8, $9::uuid)
			ORDER BY updated_at DESC, id DESC LIMIT $10`,
			ws, pid, req.Msg.GetIncludeTrashed(), req.Msg.GetParentPageId(), req.Msg.GetJournalsOnly(),
			escapeLike(projection.NormalizeTitle(req.Msg.GetTitlePrefix())), projection.NormalizeTitle(req.Msg.GetTitle()), afterT, afterID, size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p kbv1.Page
			var journal, deleted *time.Time
			var created, updated time.Time
			var attrs []byte
			var format string
			if err := rows.Scan(&p.Id, &p.DocId, &p.Title, &p.ParentPageId, &journal, &format, &p.TypeId, &p.Key, &p.Icon, &attrs, &p.TitleConflict, &p.IndexedSeq, &deleted, &created, &updated); err != nil {
				return err
			}
			if len(out.Pages) == size {
				last := out.Pages[size-1]
				out.NextCursor = encodeCursor(last.UpdatedAt.AsTime().Format(time.RFC3339Nano), last.Id)
				break
			}
			p.WorkspaceId, p.ProjectId = ws, pid
			p.Format = formatEnum(format)
			if journal != nil {
				p.JournalDate = journal.Format("2006-01-02")
			}
			p.Props = jsonStruct(attrs)
			p.Version = p.IndexedSeq
			p.Uri = s.uri(ws, p.Id)
			p.CreatedAt, p.UpdatedAt, p.DeletedAt = ts(created), ts(updated), tsPtr(deleted)
			out.Pages = append(out.Pages, &p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if req.Msg.GetCountMode() == kbv1.CountMode_COUNT_MODE_CAPPED {
			var n int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM pages WHERE workspace_id = $1 AND project_id = $2 AND deleted_at IS NULL LIMIT 10001) c`, ws, pid).Scan(&n); err != nil {
				return err
			}
			out.Count = &kbv1.Count{Value: min64(n, 10000), Capped: n > 10000}
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// mutatePage runs a mutation on a page doc with permission checks, indexing and notification.
func (s *PagesService) mutatePage(ctx context.Context, id *auth.Identity, ws, pageID string, ifVersion int64, event string, fn func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation, snap *schema.Snapshot) error) (*truth.Result, *truth.DocRow, error) {
	var snap *schema.Snapshot
	var fresh *truth.DocRow
	res, err := s.Mat.MutateOne(ctx, tenant(id, ws), pageID, truth.Options{Actor: id.Principal, IfVersion: ifVersion, OnCommit: func(ctx context.Context, tx pgx.Tx, results []*truth.Result) error {
		r := results[0]
		var err error
		fresh, err = s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return err
		}
		if !r.Changed {
			return nil
		}
		if err := s.index(ctx, tx, fresh, r.State, r.Seq, id.Principal, snap); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, event, "page", pageID, map[string]any{"seq": r.Seq})
		return nil
	}}, func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation) error {
		if err := s.check(ctx, id, ws, authz.Edit, authz.Doc(mu.Row.ID, mu.Row.ProjectID)); err != nil {
			return err
		}
		var err error
		snap, err = s.Schema.Load(ctx, tx, ws, mu.Row.ProjectID)
		if err != nil {
			return err
		}
		return fn(ctx, tx, mu, snap)
	})
	if err != nil {
		return nil, nil, mapErr(err)
	}
	s.notify(ws, []*truth.Result{res}, id.Principal)
	return res, fresh, nil
}

// Update changes title, icon and properties.
func (s *PagesService) Update(ctx context.Context, req *connect.Request[kbv1.UpdatePageRequest]) (*connect.Response[kbv1.UpdatePageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	in := req.Msg.GetPage()
	if in == nil {
		return nil, apierr.InvalidArgument("page", "required")
	}
	paths := maskPaths(req.Msg.GetUpdateMask(), "title", "icon", "props")
	res, row, err := s.mutatePage(ctx, id, ws, pageID, req.Msg.GetIfVersion(), "page.update", func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation, snap *schema.Snapshot) error {
		for _, p := range paths {
			switch p {
			case "title":
				t := strings.TrimSpace(in.GetTitle())
				if t == "" {
					if len(req.Msg.GetUpdateMask().GetPaths()) == 0 {
						continue
					}
					return apierr.InvalidArgument("page.title", "required")
				}
				mu.Add(loro.MetaSet("title", t))
			case "icon":
				if in.GetIcon() == "" && len(req.Msg.GetUpdateMask().GetPaths()) == 0 {
					continue
				}
				mu.Add(loro.MetaSet("icon", in.GetIcon()))
			case "props":
				if in.GetProps() == nil {
					continue
				}
				merged := map[string]any{}
				for k, v := range mu.State.Meta.Props {
					merged[k] = v
				}
				for k, v := range fromStruct(in.GetProps()) {
					merged[k] = v
				}
				props, err := normalizeProps(snap, mu.State.Meta.TypeID, merged, true)
				if err != nil {
					return err
				}
				for k, v := range props {
					if v == nil {
						mu.Add(loro.MetaPropDel(k))
					} else {
						mu.Add(loro.MetaPropSet(k, v))
					}
				}
			default:
				return apierr.InvalidArgument("update_mask", "unknown path "+p)
			}
		}
		mu.Emit(truth.EventPageUpdated, map[string]any{"page_id": pageID, "paths": paths})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.UpdatePageResponse{Page: s.pageProto(res.State, row)}), nil
}

// Move re-parents a page within its project.
func (s *PagesService) Move(ctx context.Context, req *connect.Request[kbv1.MovePageRequest]) (*connect.Response[kbv1.MovePageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	if req.Msg.GetProjectId() != "" {
		return nil, apierr.Unimplemented("moving pages across projects")
	}
	res, row, err := s.mutatePage(ctx, id, ws, pageID, 0, "page.move", func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation, snap *schema.Snapshot) error {
		parent := req.Msg.GetParentPageId()
		if parent == "" {
			mu.Add(loro.MetaPropDel("_parent_page_id"))
		} else {
			if parent == pageID {
				return apierr.InvalidArgument("parent_page_id", "a page cannot be its own parent")
			}
			prow, err := s.Truth.GetDoc(ctx, tx, ws, parent)
			if err != nil || prow.ProjectID != mu.Row.ProjectID || prow.Kind != truth.KindPage {
				return apierr.InvalidArgument("parent_page_id", "unknown page in this project")
			}
			// Refuse cycles through the projection's parent chain.
			cur := parent
			for i := 0; i < 64 && cur != ""; i++ {
				var next *string
				if err := tx.QueryRow(ctx, `SELECT parent_page_id::text FROM pages WHERE workspace_id = $1 AND id = $2`, ws, cur).Scan(&next); err != nil || next == nil {
					break
				}
				if *next == pageID {
					return apierr.InvalidArgument("parent_page_id", "would create a cycle")
				}
				cur = *next
			}
			mu.Add(loro.MetaPropSet("_parent_page_id", parent))
		}
		mu.Emit(truth.EventPageMoved, map[string]any{"page_id": pageID, "parent_page_id": parent})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.MovePageResponse{Page: s.pageProto(res.State, row)}), nil
}

// setDeleted trashes or restores a page and its projections.
func (s *PagesService) setDeleted(ctx context.Context, id *auth.Identity, ws, pageID string, deleted bool) (time.Time, error) {
	now := s.Clock()
	err := s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		row, err := s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return apierr.NotFound("page", pageID)
		}
		if err := s.check(ctx, id, ws, authz.Edit, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return err
		}
		var at *time.Time
		kind := truth.EventPageRestored
		action := "page.restore"
		if deleted {
			at = &now
			kind = truth.EventPageTrashed
			action = "page.trash"
		}
		if err := s.Truth.SetDeleted(ctx, tx, ws, pageID, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE pages SET deleted_at = $3, updated_at = $4 WHERE workspace_id = $1 AND id = $2`, ws, pageID, at, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE blocks SET deleted_at = $3, updated_at = $4 WHERE workspace_id = $1 AND page_id = $2`, ws, pageID, at, now); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, action, "page", pageID, nil)
		return s.Truth.InsertOutbox(ctx, tx, &truth.Event{Kind: kind, WorkspaceID: ws, ProjectID: row.ProjectID, DocID: row.ID, Seq: row.CurrentSeq, Actor: id.Principal, Payload: map[string]any{"page_id": pageID}})
	})
	if deleted {
		s.Mat.Evict(ws, pageID)
	}
	return now, mapErr(err)
}

// Trash moves a page to the trash for 30 days.
func (s *PagesService) Trash(ctx context.Context, req *connect.Request[kbv1.TrashPageRequest]) (*connect.Response[kbv1.TrashPageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	now, err := s.setDeleted(ctx, id, req.Msg.GetWorkspaceId(), req.Msg.GetPageId(), true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.TrashPageResponse{PurgeAfter: ts(now.AddDate(0, 0, 30))}), nil
}

// Restore brings a page back from the trash.
func (s *PagesService) Restore(ctx context.Context, req *connect.Request[kbv1.RestorePageRequest]) (*connect.Response[kbv1.RestorePageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	if _, err := s.setDeleted(ctx, id, ws, pageID, false); err != nil {
		return nil, err
	}
	var page *kbv1.Page
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		st, row, err := s.Mat.Read(ctx, tx, ws, pageID)
		if err != nil {
			return err
		}
		page = s.pageProto(st, row)
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.RestorePageResponse{Page: page}), nil
}

// Purge deletes a trashed page's truth and projections permanently.
func (s *PagesService) Purge(ctx context.Context, req *connect.Request[kbv1.PurgePageRequest]) (*connect.Response[kbv1.PurgePageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		row, err := s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return apierr.NotFound("page", pageID)
		}
		if err := s.check(ctx, id, ws, authz.Admin, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return err
		}
		if row.DeletedAt == nil {
			return apierr.FailedPrecondition("only trashed pages can be purged")
		}
		if err := projection.DeletePage(ctx, tx, ws, pageID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM block_keys WHERE workspace_id = $1 AND block_id = $2`, ws, pageID); err != nil {
			return err
		}
		if err := s.Truth.PurgeDocs(ctx, tx, ws, pageID); err != nil {
			return err
		}
		s.audit(ctx, tx, id, ws, "page.purge", "page", pageID, nil)
		return nil
	})
	s.Mat.Evict(ws, pageID)
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&kbv1.PurgePageResponse{}), nil
}

// Journal returns the page for a date, creating it when absent.
func (s *PagesService) Journal(ctx context.Context, req *connect.Request[kbv1.JournalRequest]) (*connect.Response[kbv1.JournalResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pid := req.Msg.GetWorkspaceId(), req.Msg.GetProjectId()
	if err := s.check(ctx, id, ws, authz.View, authz.Project(pid)); err != nil {
		return nil, err
	}
	date := req.Msg.GetDate()
	if date == "" {
		date = s.Clock().UTC().Format("2006-01-02")
	}
	if !validJournalDate(date) {
		return nil, apierr.InvalidArgument("date", "YYYY-MM-DD")
	}
	var existing string
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id FROM pages WHERE workspace_id = $1 AND project_id = $2 AND journal_date = $3 AND deleted_at IS NULL ORDER BY created_at LIMIT 1`, ws, pid, date).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	if existing != "" {
		var page *kbv1.Page
		err = s.DB.Tx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
			st, row, err := s.Mat.Read(ctx, tx, ws, existing)
			if err != nil {
				return err
			}
			page = s.pageProto(st, row)
			return nil
		})
		if err != nil {
			return nil, mapErr(err)
		}
		return connect.NewResponse(&kbv1.JournalResponse{Page: page}), nil
	}
	if err := s.check(ctx, id, ws, authz.Edit, authz.Project(pid)); err != nil {
		return nil, err
	}
	created, err := s.create(ctx, id, ws, pid, date, &kbv1.CreatePageRequest{WorkspaceId: ws, ProjectId: pid, Title: date, JournalDate: date, Format: kbv1.PageFormat_PAGE_FORMAT_OUTLINER}, func(context.Context, pgx.Tx, *kbv1.CreatePageResponse) error { return nil })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.JournalResponse{Page: created.Page, Created: true}), nil
}

// Convert switches the page format; the tree is unchanged.
func (s *PagesService) Convert(ctx context.Context, req *connect.Request[kbv1.ConvertPageRequest]) (*connect.Response[kbv1.ConvertPageResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetFormat() == kbv1.PageFormat_PAGE_FORMAT_UNSPECIFIED {
		return nil, apierr.InvalidArgument("format", "required")
	}
	res, row, err := s.mutatePage(ctx, id, req.Msg.GetWorkspaceId(), req.Msg.GetPageId(), 0, "page.convert", func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation, snap *schema.Snapshot) error {
		mu.Add(loro.MetaSet("format", formatName(req.Msg.GetFormat())))
		mu.Emit(truth.EventPageUpdated, map[string]any{"page_id": req.Msg.GetPageId(), "paths": []string{"format"}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.ConvertPageResponse{Page: s.pageProto(res.State, row)}), nil
}

// ListVersions lists the seqs of a page's doc, newest first, with named snapshots flagged.
func (s *PagesService) ListVersions(ctx context.Context, req *connect.Request[kbv1.ListVersionsRequest]) (*connect.Response[kbv1.ListVersionsResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	size := pageSize(req.Msg.GetPageSize())
	cur, err := decodeCursor(req.Msg.GetCursor(), 1)
	if err != nil {
		return nil, err
	}
	out := &kbv1.ListVersionsResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		row, err := s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return apierr.NotFound("page", pageID)
		}
		if err := s.check(ctx, id, ws, authz.View, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return err
		}
		before := int64(0)
		if cur[0] != "" {
			if _, err := parseInt(cur[0], &before); err != nil {
				return apierr.InvalidArgument("cursor", "invalid")
			}
		}
		rows, err := tx.Query(ctx, `SELECT u.seq, u.actor_id, u.created_at, s.named, (s.seq IS NOT NULL) FROM doc_updates u
			LEFT JOIN doc_snapshots s ON s.workspace_id = u.workspace_id AND s.doc_id = u.doc_id AND s.seq = u.seq
			WHERE u.workspace_id = $1 AND u.doc_id = $2 AND ($3 = 0 OR u.seq < $3) ORDER BY u.seq DESC LIMIT $4`, ws, row.ID, before, size+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v kbv1.Version
			var created time.Time
			var named *string
			if err := rows.Scan(&v.Seq, &v.Actor, &created, &named, &v.Snapshot); err != nil {
				return err
			}
			if len(out.Versions) == size {
				out.NextCursor = encodeCursor(itoa(int(out.Versions[size-1].Seq)))
				break
			}
			v.CreatedAt = ts(created)
			if named != nil {
				v.Named = *named
			}
			out.Versions = append(out.Versions, &v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

func parseInt(s string, dst *int64) (bool, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return false, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	*dst = n
	return true, nil
}

// GetVersion reconstructs a page at an earlier seq.
func (s *PagesService) GetVersion(ctx context.Context, req *connect.Request[kbv1.GetVersionRequest]) (*connect.Response[kbv1.GetVersionResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	out := &kbv1.GetVersionResponse{}
	err = s.DB.ReadTx(ctx, tenant(id, ws), func(ctx context.Context, tx pgx.Tx) error {
		row, err := s.Truth.GetDoc(ctx, tx, ws, pageID)
		if err != nil {
			return apierr.NotFound("page", pageID)
		}
		if err := s.check(ctx, id, ws, authz.View, authz.Doc(row.ID, row.ProjectID)); err != nil {
			return err
		}
		if req.Msg.GetSeq() <= 0 || req.Msg.GetSeq() > row.CurrentSeq {
			return apierr.NotFound("version", itoa(int(req.Msg.GetSeq())))
		}
		st, err := s.Mat.ReadAt(ctx, tx, ws, row.ID, req.Msg.GetSeq())
		if err != nil {
			return err
		}
		at := *row
		at.CurrentSeq = req.Msg.GetSeq()
		out.Page = s.pageProto(st, &at)
		xs, err := s.pageTree(ctx, tx, id, &at, st, 0)
		if err != nil {
			return err
		}
		if req.Msg.GetFormat() == kbv1.ContentFormat_CONTENT_FORMAT_MARKDOWN {
			out.Markdown = s.Splitter.Join(specsOfX(xs), st.Meta.Format == "outliner")
		} else {
			out.Blocks = s.flattenX(xs)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(out), nil
}

// RestoreVersion appends the changes that bring the page back to an earlier
// state: every current block is removed and the old tree is recreated with its
// original block ids. History is never rewritten.
func (s *PagesService) RestoreVersion(ctx context.Context, req *connect.Request[kbv1.RestoreVersionRequest]) (*connect.Response[kbv1.RestoreVersionResponse], error) {
	id, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ws, pageID := req.Msg.GetWorkspaceId(), req.Msg.GetPageId()
	res, row, err := s.mutatePage(ctx, id, ws, pageID, req.Msg.GetIfVersion(), "page.restore_version", func(ctx context.Context, tx pgx.Tx, mu *truth.Mutation, snap *schema.Snapshot) error {
		seq := req.Msg.GetSeq()
		if seq <= 0 || seq > mu.Row.CurrentSeq {
			return apierr.NotFound("version", itoa(int(seq)))
		}
		old, err := s.Mat.ReadAt(ctx, tx, ws, mu.Row.ID, seq)
		if err != nil {
			return err
		}
		for _, r := range mu.State.Roots {
			mu.Add(loro.TreeDelete(r.TreeID))
		}
		mu.Add(loro.MetaSet("title", old.Meta.Title), loro.MetaSet("icon", old.Meta.Icon), loro.MetaSet("format", old.Meta.Format))
		var rec func(nodes []*loro.Node, parent string)
		rec = func(nodes []*loro.Node, parent string) {
			for _, n := range nodes {
				props := map[string]any{}
				for k, v := range n.Props {
					props[k] = v
				}
				mu.Add(loro.TreeCreate(n.ID, parent, -1, &loro.TreeCreateInit{Content: n.Content, TypeID: n.TypeID, CreatedAt: n.CreatedAt, CreatedBy: n.CreatedBy, Props: props}))
				rec(n.Children, loro.Placeholder(n.ID))
			}
		}
		rec(old.Roots, "")
		mu.Emit(truth.EventPageRestored, map[string]any{"page_id": pageID, "restored_seq": seq})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kbv1.RestoreVersionResponse{Page: s.pageProto(res.State, row), Version: res.Seq}), nil
}

// ImportFolder is provided by the import/export milestone.
func (s *PagesService) ImportFolder(ctx context.Context, req *connect.Request[kbv1.ImportFolderRequest]) (*connect.Response[kbv1.ImportFolderResponse], error) {
	return nil, apierr.Unimplemented("PagesService.ImportFolder")
}

// GetImportReport is provided by the import/export milestone.
func (s *PagesService) GetImportReport(ctx context.Context, req *connect.Request[kbv1.GetImportReportRequest]) (*connect.Response[kbv1.GetImportReportResponse], error) {
	return nil, apierr.Unimplemented("PagesService.GetImportReport")
}

// ExportMarkdown returns the canonical Markdown of one page.
func (s *PagesService) ExportMarkdown(ctx context.Context, req *connect.Request[kbv1.ExportMarkdownRequest]) (*connect.Response[kbv1.ExportMarkdownResponse], error) {
	if req.Msg.GetIncludeChildren() {
		return nil, apierr.Unimplemented("ExportMarkdown with include_children")
	}
	got, err := s.Get(ctx, connect.NewRequest(&kbv1.GetPageRequest{WorkspaceId: req.Msg.GetWorkspaceId(), Ref: &kbv1.GetPageRequest_PageId{PageId: req.Msg.GetPageId()}, Format: kbv1.ContentFormat_CONTENT_FORMAT_MARKDOWN}))
	if err != nil {
		return nil, err
	}
	front := "---\ntitle: " + got.Msg.Page.Title + "\n"
	if got.Msg.Page.Key != "" {
		front += "key: " + got.Msg.Page.Key + "\n"
	}
	front += "---\n\n"
	return connect.NewResponse(&kbv1.ExportMarkdownResponse{Markdown: front + got.Msg.Markdown}), nil
}
