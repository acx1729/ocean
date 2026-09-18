package keyring

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testSeal = SealParams{Time: 1, Memory: 8 * 1024, Threads: 1}

func TestAEADRoundTripAndAADBinding(t *testing.T) {
	key, _ := NewDEK()
	aad := AADUpdate("ws", "doc", 7)
	ct, err := SealWithKey(key, aad, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := OpenWithKey(key, aad, ct)
	if err != nil || string(pt) != "hello" {
		t.Fatal(err)
	}
	if _, err := OpenWithKey(key, AADUpdate("ws", "doc", 8), ct); !errors.Is(err, ErrDecrypt) {
		t.Fatal("moved seq must fail")
	}
	if _, err := OpenWithKey(key, AADSnapshot("ws", "doc", 7), ct); !errors.Is(err, ErrDecrypt) {
		t.Fatal("snapshot aad must differ from update aad")
	}
	ct[len(ct)-1] ^= 1
	if _, err := OpenWithKey(key, aad, ct); !errors.Is(err, ErrDecrypt) {
		t.Fatal("tamper must fail")
	}
	ct2, _ := SealWithKey(key, aad, []byte("hello"))
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonces must be random")
	}
}

func TestNodeKeySealUnseal(t *testing.T) {
	n, err := GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(n.DID(), "did:key:z6Mk") {
		t.Fatalf("node did: %s", n.DID())
	}
	sealed, err := n.Seal([]byte("correct horse battery staple"), testSeal)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := UnsealNodeKey(sealed, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if n2.DID() != n.DID() || !bytes.Equal(n2.EncryptionPublicKey(), n.EncryptionPublicKey()) {
		t.Fatal("unsealed key differs")
	}
	if !bytes.Equal(n.Derive("operator-token", 32), n2.Derive("operator-token", 32)) {
		t.Fatal("derivation must be deterministic")
	}
	if bytes.Equal(n.Derive("operator-token", 32), n.Derive("backup", 32)) {
		t.Fatal("purposes must differ")
	}
	if _, err := UnsealNodeKey(sealed, []byte("wrong passphrase")); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
	if _, err := n.Seal([]byte("short"), testSeal); err == nil {
		t.Fatal("short passphrase must be refused")
	}
}

func TestLoadOrCreateNodeKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "node.key")
	k1, created, err := LoadOrCreateNodeKey(path, []byte("passphrase-1"), testSeal)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	k2, created, err := LoadOrCreateNodeKey(path, []byte("passphrase-1"), testSeal)
	if err != nil || created || k2.DID() != k1.DID() {
		t.Fatal(err, created)
	}
	if _, _, err := LoadOrCreateNodeKey(path, []byte("passphrase-2"), testSeal); err == nil {
		t.Fatal("wrong passphrase must not silently create a new key")
	}
}

func TestHPKEWrap(t *testing.T) {
	n, _ := GenerateNodeKey()
	other, _ := GenerateNodeKey()
	key, _ := NewDEK()
	info := WrapInfo("ws1", 1)
	wrapped, err := WrapKey(n.EncryptionPublicKey(), key, info)
	if err != nil {
		t.Fatal(err)
	}
	got, err := n.UnwrapKey(wrapped, info)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal(err)
	}
	if _, err := other.UnwrapKey(wrapped, info); err == nil {
		t.Fatal("other node must not unwrap")
	}
	if _, err := n.UnwrapKey(wrapped, WrapInfo("ws1", 2)); err == nil {
		t.Fatal("info mismatch must fail")
	}
}

type memStore struct {
	wrapped map[string][]byte
	current map[string]int
	calls   int
}

func (m *memStore) WrappedKey(_ context.Context, ws string, v int) ([]byte, error) {
	m.calls++
	return m.wrapped[cacheKey(ws, v)], nil
}
func (m *memStore) CurrentVersion(_ context.Context, ws string) (int, error) {
	return m.current[ws], nil
}

func TestKeyRingCachesAndEvicts(t *testing.T) {
	ctx := context.Background()
	n, _ := GenerateNodeKey()
	st := &memStore{wrapped: map[string][]byte{}, current: map[string]int{}}
	ring := New(n, st, Options{IdleTTL: 50 * time.Millisecond})
	w1, err := ring.CreateWorkspaceKey("ws1", 1)
	if err != nil {
		t.Fatal(err)
	}
	st.wrapped[cacheKey("ws1", 1)] = w1
	st.current["ws1"] = 1
	ct, err := ring.Seal(ctx, "ws1", 1, AADUpdate("ws1", "d", 1), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if st.calls != 0 {
		t.Fatal("key created locally must be served from cache")
	}
	ring.Forget("ws1")
	pt, err := ring.Open(ctx, "ws1", 1, AADUpdate("ws1", "d", 1), ct)
	if err != nil || string(pt) != "x" || st.calls != 1 {
		t.Fatalf("unwrap from store expected once: %v calls=%d", err, st.calls)
	}
	if _, err := ring.Open(ctx, "ws1", 1, AADUpdate("ws1", "d", 1), ct); err != nil || st.calls != 1 {
		t.Fatal("second open must hit the cache")
	}
	time.Sleep(80 * time.Millisecond)
	ring.Sweep()
	if ring.Cached() != 0 {
		t.Fatal("idle key must be evicted")
	}
	if _, err := ring.Open(ctx, "ws1", 2, nil, ct); !errors.Is(err, ErrNoKey) {
		t.Fatalf("missing version: %v", err)
	}
	v, ct2, err := ring.SealCurrent(ctx, "ws1", AADSnapshot("ws1", "d", 3), []byte("y"))
	if err != nil || v != 1 {
		t.Fatal(err, v)
	}
	if _, err := ring.Open(ctx, "ws1", 1, AADSnapshot("ws1", "d", 3), ct2); err != nil {
		t.Fatal(err)
	}
}
