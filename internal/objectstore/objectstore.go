// Package objectstore is the EncryptedObjectStore of specification section 5
// ("Object storage contract"): the only package allowed to talk to object
// storage. Every object it writes is ciphertext produced by the node, so the
// store learns object counts, sizes and workspace ids, nothing else.
//
// Envelope encryption per object: a random 256-bit DEK encrypts the plaintext
// in 1 MiB chunks with XChaCha20-Poly1305, the chunk index bound into the AAD;
// the DEK is wrapped by the workspace key (through a DEKWrapper, implemented
// by the key ring) and stored in a fixed 256-byte header. An authenticated
// trailer carries the plaintext size, chunk count and SHA-256, so truncation
// and appended chunks are detected. See format.go for the exact byte layout.
//
// Two backends implement the raw byte contract: NewS3 (aws-sdk-go-v2, path
// style, SeaweedFS/Ceph RGW/AWS S3) and NewFilesystem (desktop profile and
// tests). Neither ever sees plaintext.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Purpose classifies why an object exists; it is the middle segment of its key.
type Purpose string

// The purposes objects are stored under.
const (
	PurposeAsset    Purpose = "asset"
	PurposeSnapshot Purpose = "snapshot"
	PurposeExport   Purpose = "export"
	PurposeImport   Purpose = "import"
	PurposeBackup   Purpose = "backup"
)

// Purposes lists every valid purpose.
var Purposes = []Purpose{PurposeAsset, PurposeSnapshot, PurposeExport, PurposeImport, PurposeBackup}

// Valid reports whether p is one of the known purposes.
func (p Purpose) Valid() bool {
	switch p {
	case PurposeAsset, PurposeSnapshot, PurposeExport, PurposeImport, PurposeBackup:
		return true
	}
	return false
}

// Errors returned by the store and its backends. Integrity failures wrap
// ErrCorrupt; when the failure is an authentication failure of a chunk, the
// trailer or the wrapped DEK, the error additionally wraps keyring.ErrDecrypt.
var (
	// ErrNotFound is returned when no object exists at a key.
	ErrNotFound = errors.New("objectstore: not found")
	// ErrCorrupt is returned when an object fails its integrity checks: a bad
	// header, a chunk that does not authenticate, a missing trailer, a size
	// that does not match the layout, or trailing bytes after the trailer.
	ErrCorrupt = errors.New("objectstore: object corrupt")
	// ErrUnsupported is returned for a well-formed header of a format version
	// or algorithm this build does not understand.
	ErrUnsupported = errors.New("objectstore: unsupported object format")
	// ErrRange is returned when a requested range starts beyond the end.
	ErrRange = errors.New("objectstore: range not satisfiable")
	// ErrInvalidArgument is returned for malformed workspace ids, object ids
	// or purposes.
	ErrInvalidArgument = errors.New("objectstore: invalid argument")
)

// Backend is a raw byte store; it never sees plaintext. Keys are slash
// separated paths produced by ObjectKey.
type Backend interface {
	// Put stores the bytes read from r under key, replacing any previous
	// object atomically. size is the exact byte count when known, or -1 for
	// a stream of unknown length (the S3 backend then uses a multipart
	// upload). contentType and metadata are stored as object metadata where
	// the backend supports it.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string, metadata map[string]string) error
	// Get returns the bytes of key starting at offset; length -1 reads to the
	// end. A missing key returns ErrNotFound and an offset beyond the last
	// byte returns ErrRange. The returned reader may implement TotalSizer.
	Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// Head returns the stored size of key, or ErrNotFound.
	Head(ctx context.Context, key string) (size int64, err error)
	// Delete removes key; deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List calls fn for every key with the given prefix, with its stored
	// size, in backend-defined order, stopping at the first error fn returns.
	List(ctx context.Context, prefix string, fn func(key string, size int64) error) error
	// EnsureBucket creates the bucket if it is missing (first start of the
	// Compose profile); it is a no-op for the filesystem backend.
	EnsureBucket(ctx context.Context) error
}

// TotalSizer is optionally implemented by the readers a Backend returns from
// Get: TotalSize reports the full stored size of the object regardless of the
// requested range (S3 has it in Content-Range, the filesystem in Stat), which
// lets the store derive the chunk layout without a separate Head request. A
// negative value means unknown.
type TotalSizer interface {
	TotalSize() int64
}

// DEKWrapper wraps per-object data keys with a workspace key. The key ring
// implements it (see KeyRingWrapper); tests use a fake. Implementations must
// bind the wrapping to the object with keyring.AADObjectDEK(workspaceID,
// objectID) so a header cannot be moved between objects or workspaces.
type DEKWrapper interface {
	// WrapDEK seals dek under the workspace's current key and returns that
	// key version with the wrapped bytes (72 bytes for XChaCha20-Poly1305).
	WrapDEK(ctx context.Context, workspaceID, objectID string, dek []byte) (keyVersion int, wrapped []byte, err error)
	// UnwrapDEK opens wrapped under the workspace key at keyVersion.
	UnwrapDEK(ctx context.Context, workspaceID, objectID string, keyVersion int, wrapped []byte) ([]byte, error)
}

// Object describes a stored object. Size is the plaintext size; StoredSize
// the ciphertext size in the backend (header, chunk overhead and trailer
// included); SHA256 the digest of the plaintext.
type Object struct {
	ID          string
	WorkspaceID string
	Purpose     Purpose
	Key         string
	Size        int64
	StoredSize  int64
	KeyVersion  int
	SHA256      []byte
}

// Stored object metadata: the content type is always octet-stream and the
// only user metadata is the envelope marker.
const (
	// ContentType is the Content-Type of every stored object.
	ContentType = "application/octet-stream"
	// MetadataKey and MetadataValue form the single user metadata entry,
	// kb-env=1, that marks an envelope-encrypted object.
	MetadataKey   = "kb-env"
	MetadataValue = "1"
)

// Metadata returns the user metadata stored with every object.
func Metadata() map[string]string { return map[string]string{MetadataKey: MetadataValue} }

// Field widths of the header shared with key validation.
const (
	// MaxIDLen is the longest object id the header can hold (a UUID is 36).
	MaxIDLen = 36
	// MaxWorkspaceIDLen bounds workspace ids in keys.
	MaxWorkspaceIDLen = 128
)

// ObjectKey returns the backend key of an object: "{workspace_id}/{purpose}/{id}".
func ObjectKey(workspaceID string, purpose Purpose, id string) string {
	return workspaceID + "/" + string(purpose) + "/" + id
}

// parseKey splits a key produced by ObjectKey. It reports false for keys
// with a different shape (foreign objects in the bucket).
func parseKey(key string) (workspaceID string, purpose Purpose, id string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], Purpose(parts[1]), parts[2], true
}

// validateSegment checks a workspace or object id: 1..max bytes of
// [A-Za-z0-9._-], not starting with a dot. The alphabet keeps keys portable
// across S3 implementations and desktop filesystems (including Windows) and
// rules out path traversal in the filesystem backend.
func validateSegment(what, s string, max int) error {
	if s == "" {
		return fmt.Errorf("%w: empty %s", ErrInvalidArgument, what)
	}
	if len(s) > max {
		return fmt.Errorf("%w: %s longer than %d bytes", ErrInvalidArgument, what, max)
	}
	if s[0] == '.' {
		return fmt.Errorf("%w: %s must not start with a dot", ErrInvalidArgument, what)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return fmt.Errorf("%w: %s contains %q", ErrInvalidArgument, what, c)
		}
	}
	return nil
}

// validateRef checks the three parts of an object reference.
func validateRef(workspaceID string, purpose Purpose, id string) error {
	if err := validateSegment("workspace id", workspaceID, MaxWorkspaceIDLen); err != nil {
		return err
	}
	if !purpose.Valid() {
		return fmt.Errorf("%w: unknown purpose %q", ErrInvalidArgument, string(purpose))
	}
	return validateSegment("object id", id, MaxIDLen)
}
