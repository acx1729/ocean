// Package keyring implements the encryption design of specification section 5:
// one XChaCha20-Poly1305 key per workspace, wrapped with HPKE to the node's
// X25519 key; the node identity key sealed at rest with Argon2id; and the
// KeyRing that is the only code path unwrapping workspace keys.
package keyring

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the size of a workspace key and of a per-object DEK.
const KeySize = chacha20poly1305.KeySize

// ErrDecrypt is returned when authentication fails (wrong key, AAD or tampering).
var ErrDecrypt = errors.New("keyring: decryption failed")

// seal encrypts plaintext with a fresh random 24-byte nonce prepended to the output.
func seal(key []byte, aad, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(out[:aead.NonceSize()]); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[:aead.NonceSize()], plaintext, aad), nil
}

// open decrypts a message produced by seal.
func open(key []byte, aad, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrDecrypt
	}
	pt, err := aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// SealWithKey and OpenWithKey are exported for the object store's per-object
// DEKs, which are random keys outside the ring.
func SealWithKey(key, aad, plaintext []byte) ([]byte, error) { return seal(key, aad, plaintext) }

// OpenWithKey decrypts with an explicit key.
func OpenWithKey(key, aad, ciphertext []byte) ([]byte, error) { return open(key, aad, ciphertext) }

// NewDEK returns a random 256-bit data encryption key.
func NewDEK() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// Zero overwrites b.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// AAD builders bind ciphertext to its location: a message moved to another
// doc, seq, chunk or workspace fails to decrypt.

// AADUpdate is workspace_id ‖ doc_id ‖ seq for doc_updates rows.
func AADUpdate(workspaceID, docID string, seq int64) []byte {
	return aad("update", workspaceID, docID, uint64(seq))
}

// AADSnapshot is workspace_id ‖ doc_id ‖ seq ‖ "snapshot" for doc_snapshots rows.
func AADSnapshot(workspaceID, docID string, seq int64) []byte {
	return aad("snapshot", workspaceID, docID, uint64(seq))
}

// AADObjectChunk is workspace_id ‖ object_id ‖ chunk for object storage chunks.
func AADObjectChunk(workspaceID, objectID string, chunk uint32) []byte {
	return aad("object", workspaceID, objectID, uint64(chunk))
}

// AADObjectDEK binds a wrapped per-object DEK to its object header.
func AADObjectDEK(workspaceID, objectID string) []byte {
	return aad("dek", workspaceID, objectID, 0)
}

// AADExport binds an export archive key.
func AADExport(workspaceID, exportID string) []byte {
	return aad("export", workspaceID, exportID, 0)
}

func aad(kind, a, b string, n uint64) []byte {
	out := make([]byte, 0, len(kind)+len(a)+len(b)+11)
	out = append(out, kind...)
	out = append(out, 0)
	out = append(out, a...)
	out = append(out, 0)
	out = append(out, b...)
	out = append(out, 0)
	out = binary.BigEndian.AppendUint64(out, n)
	return out
}

func checkKey(k []byte) error {
	if len(k) != KeySize {
		return fmt.Errorf("keyring: key must be %d bytes, got %d", KeySize, len(k))
	}
	return nil
}
