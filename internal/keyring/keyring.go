package keyring

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Store persists wrapped workspace keys (the workspace_keys table).
type Store interface {
	// WrappedKey returns the key of a workspace at a version, wrapped to this node.
	WrappedKey(ctx context.Context, workspaceID string, version int) ([]byte, error)
	// CurrentVersion returns the workspace's current key version.
	CurrentVersion(ctx context.Context, workspaceID string) (int, error)
}

// ErrNoKey is returned when a workspace has no key at the requested version.
var ErrNoKey = errors.New("keyring: no key for workspace at that version")

// KeyRing unwraps workspace keys through the node key and keeps them in an
// LRU with an idle TTL, zeroized on eviction. Keys never leave the ring:
// callers ask it to seal and open.
type KeyRing struct {
	node  *NodeKey
	store Store
	ttl   time.Duration
	max   int

	mu      sync.Mutex
	entries map[string]*entry // workspace\x00version → key
	current map[string]cur
}

type entry struct {
	key      []byte
	lastUsed time.Time
}

type cur struct {
	version int
	at      time.Time
}

// Options tune the cache.
type Options struct {
	IdleTTL time.Duration // default 10 minutes
	MaxKeys int           // default 4096
}

// New returns a ring backed by node and store.
func New(node *NodeKey, store Store, o Options) *KeyRing {
	if o.IdleTTL == 0 {
		o.IdleTTL = 10 * time.Minute
	}
	if o.MaxKeys == 0 {
		o.MaxKeys = 4096
	}
	return &KeyRing{node: node, store: store, ttl: o.IdleTTL, max: o.MaxKeys, entries: map[string]*entry{}, current: map[string]cur{}}
}

// Node returns the node identity.
func (r *KeyRing) Node() *NodeKey { return r.node }

// CreateWorkspaceKey generates a fresh workspace key for version, installs it
// in the cache and returns it wrapped to the node for storage.
func (r *KeyRing) CreateWorkspaceKey(workspaceID string, version int) (wrapped []byte, err error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	wrapped, err = WrapKey(r.node.EncryptionPublicKey(), key, WrapInfo(workspaceID, version))
	if err != nil {
		Zero(key)
		return nil, err
	}
	r.mu.Lock()
	r.put(cacheKey(workspaceID, version), key)
	r.current[workspaceID] = cur{version: version, at: time.Now()}
	r.mu.Unlock()
	return wrapped, nil
}

// Rewrap wraps the workspace key at version to another recipient (node key
// rotation, workspace export).
func (r *KeyRing) Rewrap(ctx context.Context, workspaceID string, version int, recipientPub []byte, info []byte) ([]byte, error) {
	var out []byte
	err := r.withKey(ctx, workspaceID, version, func(key []byte) error {
		var err error
		out, err = WrapKey(recipientPub, key, info)
		return err
	})
	return out, err
}

// Current returns the workspace's current key version (cached for one minute).
func (r *KeyRing) Current(ctx context.Context, workspaceID string) (int, error) {
	r.mu.Lock()
	c, ok := r.current[workspaceID]
	r.mu.Unlock()
	if ok && time.Since(c.at) < time.Minute {
		return c.version, nil
	}
	v, err := r.store.CurrentVersion(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.current[workspaceID] = cur{version: v, at: time.Now()}
	r.mu.Unlock()
	return v, nil
}

// Forget drops cached state for a workspace (after rotation or deletion).
func (r *KeyRing) Forget(workspaceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.current, workspaceID)
	for k, e := range r.entries {
		if len(k) > len(workspaceID) && k[:len(workspaceID)] == workspaceID && k[len(workspaceID)] == 0 {
			Zero(e.key)
			delete(r.entries, k)
		}
	}
}

// Seal encrypts plaintext under the workspace key at version with the given AAD.
func (r *KeyRing) Seal(ctx context.Context, workspaceID string, version int, aad, plaintext []byte) ([]byte, error) {
	var out []byte
	err := r.withKey(ctx, workspaceID, version, func(key []byte) error {
		var err error
		out, err = seal(key, aad, plaintext)
		return err
	})
	return out, err
}

// Open decrypts ciphertext under the workspace key at version with the given AAD.
func (r *KeyRing) Open(ctx context.Context, workspaceID string, version int, aad, ciphertext []byte) ([]byte, error) {
	var out []byte
	err := r.withKey(ctx, workspaceID, version, func(key []byte) error {
		var err error
		out, err = open(key, aad, ciphertext)
		return err
	})
	return out, err
}

// SealCurrent encrypts under the current key version and returns that version.
func (r *KeyRing) SealCurrent(ctx context.Context, workspaceID string, aad, plaintext []byte) (int, []byte, error) {
	v, err := r.Current(ctx, workspaceID)
	if err != nil {
		return 0, nil, err
	}
	ct, err := r.Seal(ctx, workspaceID, v, aad, plaintext)
	return v, ct, err
}

// withKey runs fn with a private copy of the key, zeroized afterwards.
func (r *KeyRing) withKey(ctx context.Context, workspaceID string, version int, fn func(key []byte) error) error {
	ck := cacheKey(workspaceID, version)
	r.mu.Lock()
	e, ok := r.entries[ck]
	var k []byte
	if ok {
		e.lastUsed = time.Now()
		k = append([]byte(nil), e.key...)
	}
	r.mu.Unlock()
	if !ok {
		wrapped, err := r.store.WrappedKey(ctx, workspaceID, version)
		if err != nil {
			return err
		}
		if len(wrapped) == 0 {
			return ErrNoKey
		}
		key, err := r.node.UnwrapKey(wrapped, WrapInfo(workspaceID, version))
		if err != nil {
			return fmt.Errorf("keyring: unwrap workspace %s v%d: %w", workspaceID, version, err)
		}
		k = append([]byte(nil), key...)
		r.mu.Lock()
		r.put(ck, key)
		r.mu.Unlock()
	}
	defer Zero(k)
	return fn(k)
}

// put stores key under ck and evicts idle or excess entries. Caller holds mu.
func (r *KeyRing) put(ck string, key []byte) {
	now := time.Now()
	if old, ok := r.entries[ck]; ok {
		Zero(old.key)
	}
	r.entries[ck] = &entry{key: key, lastUsed: now}
	for k, e := range r.entries {
		if now.Sub(e.lastUsed) > r.ttl {
			Zero(e.key)
			delete(r.entries, k)
		}
	}
	for len(r.entries) > r.max {
		var oldestK string
		var oldest time.Time
		first := true
		for k, e := range r.entries {
			if first || e.lastUsed.Before(oldest) {
				oldestK, oldest, first = k, e.lastUsed, false
			}
		}
		Zero(r.entries[oldestK].key)
		delete(r.entries, oldestK)
	}
}

// Sweep evicts idle keys; run it periodically from the host process.
func (r *KeyRing) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, e := range r.entries {
		if now.Sub(e.lastUsed) > r.ttl {
			Zero(e.key)
			delete(r.entries, k)
		}
	}
}

// Cached reports how many unwrapped keys are held (metrics).
func (r *KeyRing) Cached() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func cacheKey(workspaceID string, version int) string {
	return fmt.Sprintf("%s\x00%d", workspaceID, version)
}
