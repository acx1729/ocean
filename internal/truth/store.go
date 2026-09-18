package truth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/keyring"
)

// Store reads and writes the log. Every method takes the caller's transaction
// so services compose several operations atomically.
type Store struct {
	keys *keyring.KeyRing
	log  *slog.Logger
}

// NewStore returns a Store using keys for encryption.
func NewStore(keys *keyring.KeyRing, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{keys: keys, log: log}
}

// Keys exposes the key ring.
func (s *Store) Keys() *keyring.KeyRing { return s.keys }

// Lock takes the per-doc advisory lock for the rest of the transaction.
func Lock(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, workspaceID+":"+docID)
	return err
}

const docSelect = `SELECT workspace_id, id, project_id, kind, page_id, COALESCE(parent_doc_id::text, ''), COALESCE(portal_block_id::text, ''),
	key_version, current_seq, snapshot_seq, indexed_seq, deleted_at, created_at, updated_at FROM docs`

func scanDoc(row pgx.Row) (*DocRow, error) {
	var d DocRow
	var kind string
	err := row.Scan(&d.WorkspaceID, &d.ID, &d.ProjectID, &kind, &d.PageID, &d.ParentDocID, &d.PortalBlockID,
		&d.KeyVersion, &d.CurrentSeq, &d.SnapshotSeq, &d.IndexedSeq, &d.DeletedAt, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Kind = Kind(kind)
	return &d, nil
}

// CreateDoc inserts a docs row at seq 0 using the workspace's current key version.
func (s *Store) CreateDoc(ctx context.Context, tx pgx.Tx, d *DocRow) error {
	kv, err := s.keys.Current(ctx, d.WorkspaceID)
	if err != nil {
		return err
	}
	d.KeyVersion = kv
	var parent, portal *string
	if d.ParentDocID != "" {
		parent = &d.ParentDocID
	}
	if d.PortalBlockID != "" {
		portal = &d.PortalBlockID
	}
	return tx.QueryRow(ctx, `INSERT INTO docs (workspace_id, id, project_id, kind, page_id, parent_doc_id, portal_block_id, key_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at, updated_at`,
		d.WorkspaceID, d.ID, d.ProjectID, string(d.Kind), d.PageID, parent, portal, kv).Scan(&d.CreatedAt, &d.UpdatedAt)
}

// GetDoc loads a docs row.
func (s *Store) GetDoc(ctx context.Context, tx pgx.Tx, workspaceID, docID string) (*DocRow, error) {
	return scanDoc(tx.QueryRow(ctx, docSelect+` WHERE workspace_id = $1 AND id = $2`, workspaceID, docID))
}

// DocsOfPage lists the page doc and its boundaries.
func (s *Store) DocsOfPage(ctx context.Context, tx pgx.Tx, workspaceID, pageID string) ([]*DocRow, error) {
	rows, err := tx.Query(ctx, docSelect+` WHERE workspace_id = $1 AND page_id = $2 ORDER BY created_at`, workspaceID, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DocRow
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetDeleted marks or restores a doc and its boundaries.
func (s *Store) SetDeleted(ctx context.Context, tx pgx.Tx, workspaceID, pageID string, at *time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE docs SET deleted_at = $3, updated_at = now() WHERE workspace_id = $1 AND page_id = $2`, workspaceID, pageID, at)
	return err
}

// SetDocDeleted marks or restores a single doc (a boundary).
func (s *Store) SetDocDeleted(ctx context.Context, tx pgx.Tx, workspaceID, docID string, at *time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE docs SET deleted_at = $3, updated_at = now() WHERE workspace_id = $1 AND id = $2`, workspaceID, docID, at)
	return err
}

// PurgeDoc deletes one doc's log (a merged boundary).
func (s *Store) PurgeDoc(ctx context.Context, tx pgx.Tx, workspaceID, docID string) error {
	_, err := tx.Exec(ctx, `DELETE FROM docs WHERE workspace_id = $1 AND id = $2`, workspaceID, docID)
	return err
}

// PurgeDocs deletes the log of a page (trash purge); projections cascade elsewhere.
func (s *Store) PurgeDocs(ctx context.Context, tx pgx.Tx, workspaceID, pageID string) error {
	_, err := tx.Exec(ctx, `DELETE FROM docs WHERE workspace_id = $1 AND page_id = $2`, workspaceID, pageID)
	return err
}

// AppendInput describes one update to append.
type AppendInput struct {
	Actor     string
	ClientID  string
	ClientSeq int64
	Plaintext []byte
}

// AppendUpdate encrypts and appends an update, returning its seq. A repeated
// (client_id, client_seq) returns the original seq and duplicate = true; the
// caller must hold the doc lock.
func (s *Store) AppendUpdate(ctx context.Context, tx pgx.Tx, workspaceID, docID string, in AppendInput) (seq int64, duplicate bool, err error) {
	if in.ClientID != "" {
		err := tx.QueryRow(ctx, `SELECT seq FROM doc_updates WHERE workspace_id = $1 AND doc_id = $2 AND client_id = $3 AND client_seq = $4`,
			workspaceID, docID, in.ClientID, in.ClientSeq).Scan(&seq)
		if err == nil {
			return seq, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, false, err
		}
	}
	if err := tx.QueryRow(ctx, `UPDATE docs SET current_seq = current_seq + 1, updated_at = now() WHERE workspace_id = $1 AND id = $2 RETURNING current_seq`,
		workspaceID, docID).Scan(&seq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, ErrNotFound
		}
		return 0, false, err
	}
	kv, ct, err := s.keys.SealCurrent(ctx, workspaceID, keyring.AADUpdate(workspaceID, docID, seq), in.Plaintext)
	if err != nil {
		return 0, false, fmt.Errorf("encrypt update: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO doc_updates (workspace_id, doc_id, seq, actor_id, client_id, client_seq, key_version, ciphertext, size)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		workspaceID, docID, seq, in.Actor, in.ClientID, in.ClientSeq, kv, ct, len(in.Plaintext))
	if err != nil {
		return 0, false, err
	}
	return seq, false, nil
}

// Updates returns decrypted updates with afterSeq < seq <= untilSeq (untilSeq 0
// = latest), skipping quarantined rows, at most limit rows (0 = no limit).
func (s *Store) Updates(ctx context.Context, tx pgx.Tx, workspaceID, docID string, afterSeq, untilSeq int64, limit int) ([]Update, error) {
	q := `SELECT seq, actor_id, client_id, client_seq, key_version, ciphertext, created_at FROM doc_updates
		WHERE workspace_id = $1 AND doc_id = $2 AND seq > $3 AND ($4 = 0 OR seq <= $4) AND quarantined_at IS NULL ORDER BY seq`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := tx.Query(ctx, q, workspaceID, docID, afterSeq, untilSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		var kv int
		var ct []byte
		if err := rows.Scan(&u.Seq, &u.Actor, &u.ClientID, &u.ClientSeq, &kv, &ct, &u.CreatedAt); err != nil {
			return nil, err
		}
		pt, err := s.keys.Open(ctx, workspaceID, kv, keyring.AADUpdate(workspaceID, docID, u.Seq), ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt update %s@%d: %w", docID, u.Seq, err)
		}
		u.Bytes = pt
		out = append(out, u)
	}
	return out, rows.Err()
}

// Quarantine marks an update the compactor could not import.
func (s *Store) Quarantine(ctx context.Context, tx pgx.Tx, workspaceID, docID string, seq int64) error {
	_, err := tx.Exec(ctx, `UPDATE doc_updates SET quarantined_at = now() WHERE workspace_id = $1 AND doc_id = $2 AND seq = $3`, workspaceID, docID, seq)
	return err
}

// LatestSnapshot returns the newest snapshot with seq <= atOrBelow (0 = any).
func (s *Store) LatestSnapshot(ctx context.Context, tx pgx.Tx, workspaceID, docID string, atOrBelow int64) (*Snapshot, error) {
	var snap Snapshot
	var kv int
	var ct []byte
	var named *string
	err := tx.QueryRow(ctx, `SELECT seq, key_version, ciphertext, shallow, named, created_at FROM doc_snapshots
		WHERE workspace_id = $1 AND doc_id = $2 AND ($3 = 0 OR seq <= $3) ORDER BY seq DESC LIMIT 1`, workspaceID, docID, atOrBelow).
		Scan(&snap.Seq, &kv, &ct, &snap.Shallow, &named, &snap.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	pt, err := s.keys.Open(ctx, workspaceID, kv, keyring.AADSnapshot(workspaceID, docID, snap.Seq), ct)
	if err != nil {
		return nil, fmt.Errorf("decrypt snapshot %s@%d: %w", docID, snap.Seq, err)
	}
	snap.Bytes = pt
	if named != nil {
		snap.Named = *named
	}
	return &snap, nil
}

// SaveSnapshot encrypts and stores a snapshot at seq.
func (s *Store) SaveSnapshot(ctx context.Context, tx pgx.Tx, workspaceID, docID string, seq int64, plaintext, vv []byte, shallow bool, named, createdBy string) error {
	kv, ct, err := s.keys.SealCurrent(ctx, workspaceID, keyring.AADSnapshot(workspaceID, docID, seq), plaintext)
	if err != nil {
		return err
	}
	var namedPtr *string
	if named != "" {
		namedPtr = &named
	}
	if vv == nil {
		vv = []byte{}
	}
	_, err = tx.Exec(ctx, `INSERT INTO doc_snapshots (workspace_id, doc_id, seq, key_version, ciphertext, state_vector, shallow, named, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (workspace_id, doc_id, seq) DO UPDATE SET named = COALESCE(EXCLUDED.named, doc_snapshots.named)`,
		workspaceID, docID, seq, kv, ct, vv, shallow, namedPtr, nullIfEmpty(createdBy))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE docs SET snapshot_seq = GREATEST(snapshot_seq, $3) WHERE workspace_id = $1 AND id = $2`, workspaceID, docID, seq)
	return err
}

// ListSnapshots lists snapshot seqs newest first (version history).
func (s *Store) ListSnapshots(ctx context.Context, tx pgx.Tx, workspaceID, docID string, beforeSeq int64, limit int) ([]Snapshot, error) {
	rows, err := tx.Query(ctx, `SELECT seq, shallow, COALESCE(named, ''), created_at FROM doc_snapshots
		WHERE workspace_id = $1 AND doc_id = $2 AND ($3 = 0 OR seq < $3) ORDER BY seq DESC LIMIT $4`, workspaceID, docID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		if err := rows.Scan(&sn.Seq, &sn.Shallow, &sn.Named, &sn.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// InsertOutbox appends an event to the outbox in the same transaction.
func (s *Store) InsertOutbox(ctx context.Context, tx pgx.Tx, ev *Event) error {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return err
	}
	if ev.Payload == nil {
		payload = []byte("{}")
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox (workspace_id, project_id, doc_id, seq, kind, payload, actor_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ev.WorkspaceID, nullIfEmpty(ev.ProjectID), nullIfEmpty(ev.DocID), nullIfZero(ev.Seq), ev.Kind, payload, ev.Actor)
	return err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullIfZero(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// IsNotFound reports a missing doc from either the store or the database layer.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, db.ErrNoRows)
}
