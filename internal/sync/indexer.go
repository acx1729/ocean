package sync

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/projection"
)

// indexer refreshes the projection of docs changed through sync pushes. It
// debounces per doc so a burst of keystrokes yields one index run; the
// worker role's job queue takes this over when it is present.
type indexer struct {
	h     *Hub
	mu    sync.Mutex
	dirty map[string]time.Time // key → first marked at
	last  map[string]time.Time // key → last marked at
}

const (
	indexQuiet   = 500 * time.Millisecond
	indexMaxWait = 3 * time.Second
	indexTick    = 250 * time.Millisecond
)

func newIndexer(h *Hub) *indexer {
	return &indexer{h: h, dirty: map[string]time.Time{}, last: map[string]time.Time{}}
}

func (ix *indexer) mark(ws, docID string) {
	now := ix.h.Now()
	k := roomKey(ws, docID)
	ix.mu.Lock()
	if _, ok := ix.dirty[k]; !ok {
		ix.dirty[k] = now
	}
	ix.last[k] = now
	ix.mu.Unlock()
}

// due returns the keys ready to index.
func (ix *indexer) due(now time.Time) []string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var out []string
	for k, first := range ix.dirty {
		if now.Sub(ix.last[k]) >= indexQuiet || now.Sub(first) >= indexMaxWait {
			out = append(out, k)
			delete(ix.dirty, k)
			delete(ix.last, k)
		}
	}
	return out
}

func (ix *indexer) run(ctx context.Context) {
	t := time.NewTicker(indexTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, k := range ix.due(now) {
				ws, docID := splitKey(k)
				if err := ix.index(ctx, ws, docID); err != nil {
					ix.h.Log.Warn("sync: index doc", "doc", docID, "error", err)
					ix.mark(ws, docID) // retry on the next tick after the quiet period
				}
			}
		}
	}
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// Flush indexes every dirty doc now (tests and shutdown).
func (ix *indexer) Flush(ctx context.Context) error {
	ix.mu.Lock()
	keys := make([]string, 0, len(ix.dirty))
	for k := range ix.dirty {
		keys = append(keys, k)
	}
	ix.dirty = map[string]time.Time{}
	ix.last = map[string]time.Time{}
	ix.mu.Unlock()
	for _, k := range keys {
		ws, docID := splitKey(k)
		if err := ix.index(ctx, ws, docID); err != nil {
			return err
		}
	}
	return nil
}

// Flush exposes the indexer flush on the hub.
func (h *Hub) Flush(ctx context.Context) error { return h.indexer.Flush(ctx) }

func (ix *indexer) index(ctx context.Context, ws, docID string) error {
	return ix.h.DB.TxWith(ctx, &db.Tenant{WorkspaceID: ws}, db.TxOptions{StatementTimeout: 30 * time.Second}, func(ctx context.Context, tx pgx.Tx) error {
		st, row, err := ix.h.Mat.Read(ctx, tx, ws, docID)
		if err != nil {
			return err
		}
		if row.DeletedAt != nil {
			return nil
		}
		snap, err := ix.h.Schema.Load(ctx, tx, ws, row.ProjectID)
		if err != nil {
			return err
		}
		return projection.IndexDoc(ctx, tx, projection.Input{
			WorkspaceID: ws, ProjectID: row.ProjectID, PageID: row.PageID, DocID: row.ID, PortalBlockID: row.PortalBlockID,
			State: st, Seq: row.CurrentSeq, Actor: "sync", Now: ix.h.Now(), Snap: snap, CreatedAt: row.CreatedAt,
		})
	})
}
