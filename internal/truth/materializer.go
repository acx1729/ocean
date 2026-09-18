package truth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/loro"
)

// Materializer applies structured mutations to live docs. It keeps warm,
// decrypted docs in an LRU keyed by doc id; one writer per doc per node is
// guaranteed by the Postgres advisory lock taken inside the transaction, and
// correctness across nodes comes from the CRDT.
type Materializer struct {
	store *Store
	db    *db.DB

	mu      sync.Mutex
	entries map[string]*entry
	maxDocs int
}

type entry struct {
	mu       sync.Mutex // serializes access to doc
	doc      *loro.Doc
	seq      int64
	lastUsed time.Time
	closed   bool
}

// NewMaterializer returns a materializer caching up to maxDocs warm docs (0 = 256).
func NewMaterializer(store *Store, d *db.DB, maxDocs int) *Materializer {
	if maxDocs <= 0 {
		maxDocs = 256
	}
	return &Materializer{store: store, db: d, entries: map[string]*entry{}, maxDocs: maxDocs}
}

// Store exposes the log store.
func (m *Materializer) Store() *Store { return m.store }

func key(workspaceID, docID string) string { return workspaceID + ":" + docID }

// acquire returns the cache entry for a doc, locked. Caller must unlock.
func (m *Materializer) acquire(workspaceID, docID string) *entry {
	k := key(workspaceID, docID)
	m.mu.Lock()
	e, ok := m.entries[k]
	if !ok {
		e = &entry{doc: loro.New()}
		m.entries[k] = e
		m.evictLocked()
	}
	e.lastUsed = time.Now()
	m.mu.Unlock()
	e.mu.Lock()
	if e.closed { // evicted between lookup and lock: start a fresh one
		e.doc = loro.New()
		e.seq = 0
		e.closed = false
	}
	return e
}

// evictLocked drops the least recently used entries beyond maxDocs. Caller holds m.mu.
func (m *Materializer) evictLocked() {
	if len(m.entries) <= m.maxDocs {
		return
	}
	type kv struct {
		k string
		e *entry
	}
	all := make([]kv, 0, len(m.entries))
	for k, e := range m.entries {
		all = append(all, kv{k, e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].e.lastUsed.Before(all[j].e.lastUsed) })
	for _, x := range all[:len(all)-m.maxDocs] {
		delete(m.entries, x.k)
		go func(e *entry) { // wait for any in-flight use, then free
			e.mu.Lock()
			e.closed = true
			e.doc.Close()
			e.mu.Unlock()
		}(x.e)
	}
}

// Evict drops a doc from the cache (after a failed apply or a purge).
func (m *Materializer) Evict(workspaceID, docID string) {
	k := key(workspaceID, docID)
	m.mu.Lock()
	e, ok := m.entries[k]
	delete(m.entries, k)
	m.mu.Unlock()
	if ok {
		go func() {
			e.mu.Lock()
			e.closed = true
			e.doc.Close()
			e.mu.Unlock()
		}()
	}
}

// resetLocked replaces the entry's doc with a fresh one (caller holds e.mu).
func (e *entry) resetLocked() {
	e.doc.Close()
	e.doc = loro.New()
	e.seq = 0
}

// sync brings e up to row.CurrentSeq within tx.
func (m *Materializer) sync(ctx context.Context, tx pgx.Tx, e *entry, row *DocRow) error {
	if e.seq == row.CurrentSeq {
		return nil
	}
	if e.seq > row.CurrentSeq {
		// The log moved backwards (purge and re-create, or a failed commit we did not see): rebuild.
		e.resetLocked()
	}
	reached, err := m.store.catchUp(ctx, tx, e.doc, row.WorkspaceID, row.ID, e.seq, row.CurrentSeq, e.seq == 0)
	if err != nil {
		e.resetLocked()
		return err
	}
	e.seq = reached
	if reached != row.CurrentSeq {
		// Missing rows (quarantined tail): treat the doc as at CurrentSeq so later updates apply.
		e.seq = row.CurrentSeq
	}
	return nil
}

// Read returns the live state of a doc (fresh, from the cache or the log).
func (m *Materializer) Read(ctx context.Context, tx pgx.Tx, workspaceID, docID string) (*loro.State, *DocRow, error) {
	row, err := m.store.GetDoc(ctx, tx, workspaceID, docID)
	if err != nil {
		return nil, nil, err
	}
	e := m.acquire(workspaceID, docID)
	defer e.mu.Unlock()
	if err := m.sync(ctx, tx, e, row); err != nil {
		return nil, nil, err
	}
	st, err := e.doc.State()
	if err != nil {
		e.resetLocked()
		return nil, nil, err
	}
	return st, row, nil
}

// ReadAt returns the state of a doc at an earlier seq (version history).
func (m *Materializer) ReadAt(ctx context.Context, tx pgx.Tx, workspaceID, docID string, seq int64) (*loro.State, error) {
	doc, _, err := m.store.Load(ctx, tx, workspaceID, docID, seq)
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	return doc.State()
}

// Snapshot exports the live doc as a Loro snapshot with its version vector
// (for sync Open and compaction).
func (m *Materializer) Snapshot(ctx context.Context, tx pgx.Tx, workspaceID, docID string) (snapshot, vv []byte, seq int64, err error) {
	row, err := m.store.GetDoc(ctx, tx, workspaceID, docID)
	if err != nil {
		return nil, nil, 0, err
	}
	e := m.acquire(workspaceID, docID)
	defer e.mu.Unlock()
	if err := m.sync(ctx, tx, e, row); err != nil {
		return nil, nil, 0, err
	}
	b, err := e.doc.ExportSnapshot()
	if err != nil {
		return nil, nil, 0, err
	}
	vv, err = e.doc.OplogVV()
	if err != nil {
		return nil, nil, 0, err
	}
	return b, vv, e.seq, nil
}

// Options control a mutation.
type Options struct {
	// IfVersion, when non-zero, must equal the doc's current seq.
	IfVersion int64
	Actor     string
	// ClientID and ClientSeq identify the update for idempotent retries; empty means a fresh id.
	ClientID  string
	ClientSeq int64
	// SkipState skips computing the post-mutation state (cheaper for fire-and-forget writes).
	SkipState bool
	// OnCommit runs inside the transaction after every update was appended,
	// with the results populated; it is where projections and idempotency
	// records are written so they commit atomically with the log.
	OnCommit func(ctx context.Context, tx pgx.Tx, results []*Result) error
}

// Mutation is the handle a caller uses inside Mutate to describe changes to one doc.
type Mutation struct {
	Row    *DocRow
	State  *loro.State // state before the mutation
	ops    []loro.Op
	events []*Event
	entry  *entry
	m      *Materializer
	result *Result
}

// Add appends ops.
func (mu *Mutation) Add(ops ...loro.Op) { mu.ops = append(mu.ops, ops...) }

// Emit records an outbox event to be written with the update's seq.
func (mu *Mutation) Emit(kind string, payload map[string]any) {
	mu.events = append(mu.events, &Event{Kind: kind, Payload: payload})
}

// TreeID resolves a block id to its Loro tree id, or a placeholder for a block
// created earlier in this mutation.
func (mu *Mutation) TreeID(blockID string) (string, bool) {
	if n, ok := mu.State.ByID[blockID]; ok {
		return n.TreeID, true
	}
	for _, op := range mu.ops {
		if c, ok := op.(interface{ CreatedID() string }); ok && c.CreatedID() == blockID {
			return loro.Placeholder(blockID), true
		}
	}
	return "", false
}

// Result is the outcome for one doc.
type Result struct {
	DocID   string
	Seq     int64
	Changed bool
	State   *loro.State // state after the mutation (nil with SkipState)
	Created map[string]string
	Update  []byte // plaintext Loro update, for room fan-out
}

// Mutate opens a tenant transaction, locks the docs (in sorted order), calls fn
// with a Mutation per doc, then applies, encrypts and appends one update per
// changed doc, writes the events and commits. Caches are updated only after
// the commit; on any error the touched docs are dropped from the cache.
func (m *Materializer) Mutate(ctx context.Context, tenant *db.Tenant, docIDs []string, o Options, fn func(ctx context.Context, tx pgx.Tx, docs []*Mutation) error) ([]*Result, error) {
	return m.mutate(ctx, tenant, docIDs, nil, o, fn)
}

// CreateAndMutate inserts a new doc row and applies its first mutation in the
// same transaction, so a failed creation leaves no empty doc behind.
func (m *Materializer) CreateAndMutate(ctx context.Context, tenant *db.Tenant, row *DocRow, o Options, fn func(ctx context.Context, tx pgx.Tx, mu *Mutation) error) (*Result, error) {
	res, err := m.mutate(ctx, tenant, []string{row.ID}, row, o, func(ctx context.Context, tx pgx.Tx, docs []*Mutation) error {
		return fn(ctx, tx, docs[0])
	})
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// MutateWithNew inserts a new doc and mutates it together with existing docs
// (splitting a boundary out of its parent doc). docIDs must include row.ID.
func (m *Materializer) MutateWithNew(ctx context.Context, tenant *db.Tenant, row *DocRow, docIDs []string, o Options, fn func(ctx context.Context, tx pgx.Tx, docs []*Mutation) error) ([]*Result, error) {
	return m.mutate(ctx, tenant, docIDs, row, o, fn)
}

func (m *Materializer) mutate(ctx context.Context, tenant *db.Tenant, docIDs []string, create *DocRow, o Options, fn func(ctx context.Context, tx pgx.Tx, docs []*Mutation) error) ([]*Result, error) {
	if len(docIDs) == 0 {
		return nil, errors.New("truth: no docs")
	}
	if o.ClientID == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		o.ClientID = "api:" + id.String()
		o.ClientSeq = 1
	}
	sorted := append([]string(nil), docIDs...)
	sort.Strings(sorted)
	var results []*Result
	var entries []*entry
	applied := false
	defer func() {
		for _, e := range entries {
			e.mu.Unlock()
		}
	}()
	err := m.db.Tx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		muts := make([]*Mutation, 0, len(sorted))
		byID := map[string]*Mutation{}
		if create != nil {
			if err := m.store.CreateDoc(ctx, tx, create); err != nil {
				return err
			}
			m.Evict(tenant.WorkspaceID, create.ID)
		}
		for _, id := range sorted {
			if err := Lock(ctx, tx, tenant.WorkspaceID, id); err != nil {
				return err
			}
			row, err := m.store.GetDoc(ctx, tx, tenant.WorkspaceID, id)
			if err != nil {
				return err
			}
			if row.DeletedAt != nil {
				return ErrDeleted
			}
			if o.IfVersion != 0 && len(sorted) == 1 && row.CurrentSeq != o.IfVersion {
				return &VersionConflictError{Expected: o.IfVersion, Current: row.CurrentSeq}
			}
			e := m.acquire(tenant.WorkspaceID, id)
			entries = append(entries, e)
			if err := m.sync(ctx, tx, e, row); err != nil {
				return err
			}
			st, err := e.doc.State()
			if err != nil {
				e.resetLocked()
				return err
			}
			mu := &Mutation{Row: row, State: st, entry: e, m: m}
			muts = append(muts, mu)
			byID[id] = mu
		}
		// Present mutations in the caller's order.
		ordered := make([]*Mutation, 0, len(docIDs))
		for _, id := range docIDs {
			ordered = append(ordered, byID[id])
		}
		if err := fn(ctx, tx, ordered); err != nil {
			return err
		}
		for _, mu := range ordered {
			res := &Result{DocID: mu.Row.ID, Seq: mu.Row.CurrentSeq}
			mu.result = res
			results = append(results, res)
			if len(mu.ops) == 0 {
				if !o.SkipState {
					res.State = mu.State
				}
				continue
			}
			applied = true
			ar, err := mu.entry.doc.Apply(mu.ops)
			if err != nil {
				return fmt.Errorf("apply: %w", err)
			}
			if !ar.Changed {
				if !o.SkipState {
					res.State = mu.State
				}
				continue
			}
			seq, dup, err := m.store.AppendUpdate(ctx, tx, tenant.WorkspaceID, mu.Row.ID, AppendInput{Actor: o.Actor, ClientID: o.ClientID, ClientSeq: o.ClientSeq, Plaintext: ar.Update})
			if err != nil {
				return err
			}
			if dup {
				return fmt.Errorf("truth: client update %s/%d already applied at seq %d", o.ClientID, o.ClientSeq, seq)
			}
			res.Seq, res.Changed, res.Created, res.Update = seq, true, ar.Created, ar.Update
			mu.entry.seq = seq
			if !o.SkipState {
				st, err := mu.entry.doc.State()
				if err != nil {
					return err
				}
				res.State = st
			}
			for _, ev := range mu.events {
				ev.WorkspaceID, ev.ProjectID, ev.DocID, ev.Seq, ev.Actor = tenant.WorkspaceID, mu.Row.ProjectID, mu.Row.ID, seq, o.Actor
				if err := m.store.InsertOutbox(ctx, tx, ev); err != nil {
					return err
				}
			}
		}
		if o.OnCommit != nil {
			if err := o.OnCommit(ctx, tx, results); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if applied {
			for _, e := range entries {
				e.resetLocked()
			}
		}
		return nil, err
	}
	return results, nil
}

// MutateOne is Mutate for a single doc.
func (m *Materializer) MutateOne(ctx context.Context, tenant *db.Tenant, docID string, o Options, fn func(ctx context.Context, tx pgx.Tx, mu *Mutation) error) (*Result, error) {
	res, err := m.Mutate(ctx, tenant, []string{docID}, o, func(ctx context.Context, tx pgx.Tx, docs []*Mutation) error {
		return fn(ctx, tx, docs[0])
	})
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// ImportUpdate appends an externally produced Loro update (a sync push) to a
// doc: validates the header, appends under the doc lock, and folds the bytes
// into the warm doc when one is cached. It returns the seq (original seq on a
// duplicate). The caller owns the tenant transaction and must have checked
// permissions.
func (m *Materializer) ImportUpdate(ctx context.Context, tx pgx.Tx, workspaceID, docID string, in AppendInput) (seq int64, duplicate bool, err error) {
	if err := loro.CheckUpdateHeader(in.Plaintext); err != nil {
		return 0, false, fmt.Errorf("truth: invalid update: %w", err)
	}
	if err := Lock(ctx, tx, workspaceID, docID); err != nil {
		return 0, false, err
	}
	row, err := m.store.GetDoc(ctx, tx, workspaceID, docID)
	if err != nil {
		return 0, false, err
	}
	if row.DeletedAt != nil {
		return 0, false, ErrDeleted
	}
	seq, duplicate, err = m.store.AppendUpdate(ctx, tx, workspaceID, docID, in)
	if err != nil || duplicate {
		return seq, duplicate, err
	}
	// Fold into the warm copy when it is exactly one step behind; otherwise let it resync from the log.
	m.mu.Lock()
	e, ok := m.entries[key(workspaceID, docID)]
	m.mu.Unlock()
	if ok {
		e.mu.Lock()
		if !e.closed && e.seq == seq-1 {
			if err := e.doc.Import(in.Plaintext); err == nil {
				e.seq = seq
			} else {
				e.resetLocked()
			}
		} else if !e.closed && e.seq >= seq {
			e.resetLocked()
		}
		e.mu.Unlock()
	}
	return seq, false, nil
}

// Cached reports the number of warm docs (metrics).
func (m *Materializer) Cached() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
