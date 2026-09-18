// Package truth owns the source of truth: one append-only, encrypted Loro
// update log per doc, its snapshots, and the materializer that turns
// structured API writes into log entries (specification sections 3 and 7).
//
// Every other table is a projection of this log. Nothing here is acknowledged
// before its doc_updates row commits; the acknowledgement carries the
// database-assigned seq.
package truth

import (
	"errors"
	"fmt"
	"time"
)

// Kind is the doc kind.
type Kind string

const (
	KindPage     Kind = "page"
	KindBoundary Kind = "boundary"
)

// DocRow mirrors the docs table.
type DocRow struct {
	WorkspaceID   string
	ID            string
	ProjectID     string
	Kind          Kind
	PageID        string
	ParentDocID   string
	PortalBlockID string
	KeyVersion    int
	CurrentSeq    int64
	SnapshotSeq   int64
	IndexedSeq    int64
	DeletedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Update is one decrypted log entry.
type Update struct {
	Seq       int64
	Actor     string
	ClientID  string
	ClientSeq int64
	Bytes     []byte
	CreatedAt time.Time
}

// Snapshot is a decrypted snapshot of a doc at a seq.
type Snapshot struct {
	Seq       int64
	Bytes     []byte
	Shallow   bool
	Named     string
	CreatedAt time.Time
}

// Event is an outbox row: consumers receive ids and deltas, never content.
type Event struct {
	Kind        string
	WorkspaceID string
	ProjectID   string
	DocID       string
	Seq         int64
	Actor       string
	Payload     map[string]any
}

// Errors.
var (
	ErrNotFound   = errors.New("truth: doc not found")
	ErrDeleted    = errors.New("truth: doc is in the trash")
	ErrNoSnapshot = errors.New("truth: no snapshot")
)

// VersionConflictError reports an if_version mismatch.
type VersionConflictError struct {
	Expected int64
	Current  int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("truth: version conflict: expected %d, current %d", e.Expected, e.Current)
}

// Event kinds (specification section 7).
const (
	EventPageCreated   = "page.created"
	EventPageUpdated   = "page.updated"
	EventPageMoved     = "page.moved"
	EventPageTrashed   = "page.trashed"
	EventPageRestored  = "page.restored"
	EventBlockCreated  = "block.created"
	EventBlockUpdated  = "block.updated"
	EventBlockMoved    = "block.moved"
	EventBlockDeleted  = "block.deleted"
	EventPropsChanged  = "props.changed"
	EventTypeChanged   = "type.changed"
	EventBoundaryAdded = "boundary.created"
	EventBoundaryGone  = "boundary.removed"
	EventDocSynced     = "doc.synced" // a sync push landed; the indexer treats it like block.updated
)
