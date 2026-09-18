package truth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/acx1729/ocean/internal/db"
	"github.com/acx1729/ocean/internal/keyring"
	"github.com/acx1729/ocean/internal/loro"
	"github.com/acx1729/ocean/internal/testutil"
)

type fixture struct {
	db     *db.DB
	keys   *keyring.KeyRing
	store  *Store
	mat    *Materializer
	ws     string
	proj   string
	tenant *db.Tenant
}

type dbKeyStore struct {
	db      *db.DB
	nodeDID string
}

func (k *dbKeyStore) WrappedKey(ctx context.Context, ws string, v int) ([]byte, error) {
	var w []byte
	err := k.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT wrapped_key FROM workspace_keys WHERE workspace_id = $1 AND key_version = $2 AND recipient_id = $3`, ws, v, k.nodeDID).Scan(&w)
	})
	return w, err
}

func (k *dbKeyStore) CurrentVersion(ctx context.Context, ws string) (int, error) {
	var v int
	err := k.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_key_version FROM workspaces WHERE id = $1`, ws).Scan(&v)
	})
	return v, err
}

// newFixture creates a workspace with a wrapped key and one project.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testutil.NewDB(t)
	node, _ := keyring.GenerateNodeKey()
	ks := &dbKeyStore{db: d, nodeDID: node.DID()}
	keys := keyring.New(node, ks, keyring.Options{})
	f := &fixture{db: d, keys: keys, store: NewStore(keys, nil)}
	f.mat = NewMaterializer(f.store, d, 4)
	ctx := context.Background()
	err := d.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO principals (id, kind) VALUES ($1, 'node'), ('did:key:zTester', 'user')`, node.DID()); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO workspaces (slug, name, fga_store_id, fga_model_id) VALUES ('truth', 'Truth', 's', 'm') RETURNING id`).Scan(&f.ws); err != nil {
			return err
		}
		wrapped, err := keys.CreateWorkspaceKey(f.ws, 1)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_keys (workspace_id, key_version, recipient_id, wrapped_key, wrapped_by) VALUES ($1, 1, $2, $3, $2)`, f.ws, node.DID(), wrapped); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (workspace_id, principal_id, role) VALUES ($1, 'did:key:zTester', 'admin')`, f.ws); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO projects (workspace_id, slug, name) VALUES ($1, 'main', 'Main') RETURNING id`, f.ws).Scan(&f.proj)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tenant = &db.Tenant{WorkspaceID: f.ws, PrincipalID: "did:key:zTester"}
	return f
}

func (f *fixture) newDoc(t *testing.T) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	err := f.db.Tx(context.Background(), f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return f.store.CreateDoc(ctx, tx, &DocRow{WorkspaceID: f.ws, ID: id, ProjectID: f.proj, Kind: KindPage, PageID: id})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMutateAppendsAndReloadsFromLog(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	docID := f.newDoc(t)
	blockID := uuid.Must(uuid.NewV7()).String()
	res, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{Actor: "did:key:zTester"}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
		mu.Add(loro.MetaSet("title", "Hello"), loro.MetaSet("format", "markdown"))
		mu.Add(loro.TreeCreate(blockID, "", -1, &loro.TreeCreateInit{Content: "first block", CreatedBy: "did:key:zTester"}))
		mu.Emit(EventPageCreated, map[string]any{"page_id": docID})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq != 1 || !res.Changed || res.State.Meta.Title != "Hello" || len(res.State.Roots) != 1 || res.State.Roots[0].ID != blockID {
		t.Fatalf("unexpected result: seq=%d changed=%v state=%+v", res.Seq, res.Changed, res.State.Meta)
	}
	// Second mutation edits the block through the warm cache.
	res2, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{Actor: "did:key:zTester", IfVersion: 1}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
		tid, ok := mu.TreeID(blockID)
		if !ok {
			t.Fatal("block not found in state")
		}
		mu.Add(loro.NodeText(tid, "first block, edited"))
		return nil
	})
	if err != nil || res2.Seq != 2 {
		t.Fatalf("second mutate: %v seq=%d", err, res2.Seq)
	}
	// A stale if_version is refused with the current version.
	_, err = f.mat.MutateOne(ctx, f.tenant, docID, Options{IfVersion: 1}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
		mu.Add(loro.MetaSet("title", "nope"))
		return nil
	})
	var vc *VersionConflictError
	if !errors.As(err, &vc) || vc.Current != 2 {
		t.Fatalf("expected version conflict at 2, got %v", err)
	}
	// A fresh materializer (another node) rebuilds the same state from the log.
	other := NewMaterializer(f.store, f.db, 4)
	err = f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		st, row, err := other.Read(ctx, tx, f.ws, docID)
		if err != nil {
			return err
		}
		if row.CurrentSeq != 2 || st.Meta.Title != "Hello" || st.ByID[blockID].Content != "first block, edited" {
			t.Fatalf("rebuilt state mismatch: seq=%d title=%q content=%q", row.CurrentSeq, st.Meta.Title, st.ByID[blockID].Content)
		}
		// Version history: state at seq 1 has the original text.
		old, err := other.ReadAt(ctx, tx, f.ws, docID, 1)
		if err != nil {
			return err
		}
		if old.ByID[blockID].Content != "first block" {
			t.Fatalf("state at seq 1: %q", old.ByID[blockID].Content)
		}
		// Outbox carries the event with the seq.
		var kind string
		var seq int64
		if err := tx.QueryRow(ctx, `SELECT kind, seq FROM outbox WHERE doc_id = $1 ORDER BY id LIMIT 1`, docID).Scan(&kind, &seq); err != nil {
			return err
		}
		if kind != EventPageCreated || seq != 1 {
			t.Fatalf("outbox: %s %d", kind, seq)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ciphertext at rest: the update rows do not contain the plaintext.
	err = f.db.System(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM doc_updates WHERE doc_id = $1 AND position('first block'::bytea in ciphertext) > 0`, docID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("plaintext found in doc_updates")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoOpMutationAppendsNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	docID := f.newDoc(t)
	res, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error { return nil })
	if err != nil || res.Changed || res.Seq != 0 {
		t.Fatalf("no-op: %v %+v", err, res)
	}
	// A failing callback rolls everything back and leaves the cache consistent.
	_, err = f.mat.MutateOne(ctx, f.tenant, docID, Options{}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
		mu.Add(loro.MetaSet("title", "x"))
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	err = f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		st, row, err := f.mat.Read(ctx, tx, f.ws, docID)
		if err != nil {
			return err
		}
		if row.CurrentSeq != 0 || st.Meta.Title != "" {
			t.Fatalf("rolled-back mutation leaked: seq=%d title=%q", row.CurrentSeq, st.Meta.Title)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWritersSerializeAndConverge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	docID := f.newDoc(t)
	const writers, each = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				id := uuid.Must(uuid.NewV7()).String()
				_, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{Actor: "did:key:zTester"}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
					mu.Add(loro.TreeCreate(id, "", -1, &loro.TreeCreateInit{Content: "b"}))
					return nil
				})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	err := f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		st, row, err := NewMaterializer(f.store, f.db, 4).Read(ctx, tx, f.ws, docID)
		if err != nil {
			return err
		}
		if row.CurrentSeq != writers*each || len(st.Roots) != writers*each {
			t.Fatalf("seq=%d roots=%d", row.CurrentSeq, len(st.Roots))
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM doc_updates WHERE doc_id = $1`, docID).Scan(&n); err != nil {
			return err
		}
		if n != writers*each {
			t.Fatalf("update rows: %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestImportUpdateFromSyncClient(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	docID := f.newDoc(t)
	// A client builds an update offline.
	client := loro.New()
	defer client.Close()
	blockID := uuid.Must(uuid.NewV7()).String()
	ar, err := client.Apply([]loro.Op{loro.MetaSet("title", "from client"), loro.TreeCreate(blockID, "", -1, &loro.TreeCreateInit{Content: "hi"})})
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	var dup bool
	err = f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		seq, dup, err = f.mat.ImportUpdate(ctx, tx, f.ws, docID, AppendInput{Actor: "did:key:zTester", ClientID: "c1", ClientSeq: 1, Plaintext: ar.Update})
		return err
	})
	if err != nil || dup || seq != 1 {
		t.Fatalf("import: %v dup=%v seq=%d", err, dup, seq)
	}
	// The same client update again is acknowledged with its original seq.
	err = f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		seq, dup, err = f.mat.ImportUpdate(ctx, tx, f.ws, docID, AppendInput{Actor: "did:key:zTester", ClientID: "c1", ClientSeq: 1, Plaintext: ar.Update})
		return err
	})
	if err != nil || !dup || seq != 1 {
		t.Fatalf("duplicate import: %v dup=%v seq=%d", err, dup, seq)
	}
	// Garbage is refused before touching the log.
	err = f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.mat.ImportUpdate(ctx, tx, f.ws, docID, AppendInput{ClientID: "c1", ClientSeq: 2, Plaintext: []byte("not loro")})
		return err
	})
	if err == nil {
		t.Fatal("garbage accepted")
	}
	// A structured write on top of the client update sees its content.
	res, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
		if mu.State.Meta.Title != "from client" {
			t.Fatalf("client update not visible: %q", mu.State.Meta.Title)
		}
		tid, _ := mu.TreeID(blockID)
		mu.Add(loro.NodeText(tid, "hi there"))
		return nil
	})
	if err != nil || res.Seq != 2 {
		t.Fatal(err)
	}
	// And the client, importing the server's update, converges.
	if err := client.Import(res.Update); err != nil {
		t.Fatal(err)
	}
	st, _ := client.State()
	if st.ByID[blockID].Content != "hi there" {
		t.Fatalf("client did not converge: %q", st.ByID[blockID].Content)
	}
}

func TestSnapshotsShortenReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	docID := f.newDoc(t)
	for i := 0; i < 5; i++ {
		id := uuid.Must(uuid.NewV7()).String()
		if _, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
			mu.Add(loro.TreeCreate(id, "", -1, &loro.TreeCreateInit{Content: "x"}))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		snap, vv, seq, err := f.mat.Snapshot(ctx, tx, f.ws, docID)
		if err != nil {
			return err
		}
		if seq != 5 || len(vv) == 0 {
			t.Fatalf("snapshot seq %d vv %d", seq, len(vv))
		}
		return f.store.SaveSnapshot(ctx, tx, f.ws, docID, seq, snap, vv, false, "before-cleanup", "did:key:zTester")
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two more updates after the snapshot; a cold load must combine both.
	for i := 0; i < 2; i++ {
		id := uuid.Must(uuid.NewV7()).String()
		if _, err := f.mat.MutateOne(ctx, f.tenant, docID, Options{}, func(ctx context.Context, tx pgx.Tx, mu *Mutation) error {
			mu.Add(loro.TreeCreate(id, "", -1, &loro.TreeCreateInit{Content: "y"}))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	err = f.db.Tx(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		doc, seq, err := f.store.Load(ctx, tx, f.ws, docID, 0)
		if err != nil {
			return err
		}
		defer doc.Close()
		st, _ := doc.State()
		if seq != 7 || len(st.Roots) != 7 {
			t.Fatalf("cold load: seq=%d roots=%d", seq, len(st.Roots))
		}
		snaps, err := f.store.ListSnapshots(ctx, tx, f.ws, docID, 0, 10)
		if err != nil || len(snaps) != 1 || snaps[0].Named != "before-cleanup" {
			t.Fatalf("snapshots: %v %+v", err, snaps)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = time.Now
}
