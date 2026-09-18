package keyring

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloudflare/circl/hpke"
	"github.com/cloudflare/circl/kem"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"

	"github.com/acx1729/ocean/internal/did"
)

// HPKE suite for wrapping workspace keys (RFC 9180): X25519-HKDF-SHA256,
// HKDF-SHA256, ChaCha20-Poly1305.
var (
	suite     = hpke.NewSuite(hpke.KEM_X25519_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_ChaCha20Poly1305)
	kemScheme = hpke.KEM_X25519_HKDF_SHA256.Scheme()
)

// NodeKey is the node identity: one 32-byte seed from which the Ed25519
// signing key, the X25519 wrapping key and every derived secret come.
type NodeKey struct {
	seed    []byte
	signing ed25519.PrivateKey
	kemPriv kem.PrivateKey
	kemPub  kem.PublicKey
	didStr  string
}

// GenerateNodeKey creates a new random node identity.
func GenerateNodeKey() (*NodeKey, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return nodeKeyFromSeed(seed)
}

func nodeKeyFromSeed(seed []byte) (*NodeKey, error) {
	if len(seed) != 32 {
		return nil, errors.New("keyring: node seed must be 32 bytes")
	}
	n := &NodeKey{seed: seed}
	n.signing = ed25519.NewKeyFromSeed(n.Derive("ed25519-signing", ed25519.SeedSize))
	kemSeed := n.Derive("x25519-wrapping", 32)
	pub, priv := kemScheme.DeriveKeyPair(kemSeed)
	n.kemPub, n.kemPriv = pub, priv
	n.didStr = did.FromEd25519(n.signing.Public().(ed25519.PublicKey))
	return n, nil
}

// DID is the node principal (did:key of the signing key).
func (n *NodeKey) DID() string { return n.didStr }

// SigningPublicKey returns the Ed25519 public key.
func (n *NodeKey) SigningPublicKey() ed25519.PublicKey {
	return n.signing.Public().(ed25519.PublicKey)
}

// Sign signs a message with the node's Ed25519 key.
func (n *NodeKey) Sign(msg []byte) []byte { return ed25519.Sign(n.signing, msg) }

// EncryptionPublicKey returns the raw 32-byte X25519 public key workspace keys are wrapped to.
func (n *NodeKey) EncryptionPublicKey() []byte {
	b, _ := n.kemPub.MarshalBinary()
	return b
}

// Derive returns size bytes of key material bound to purpose (HKDF-SHA256).
// Used for the operator token, the backup key and the sub-keys.
func (n *NodeKey) Derive(purpose string, size int) []byte {
	r := hkdf.New(sha256.New, n.seed, []byte("kb-node-key-v1"), []byte(purpose))
	out := make([]byte, size)
	if _, err := io.ReadFull(r, out); err != nil {
		panic(err)
	}
	return out
}

// Destroy zeroizes the seed. The key must not be used afterwards.
func (n *NodeKey) Destroy() {
	Zero(n.seed)
	Zero(n.signing)
}

// WrapKey wraps key to recipientPub (raw X25519 public key) with HPKE base mode.
// The output is enc ‖ ciphertext.
func WrapKey(recipientPub []byte, key []byte, info []byte) ([]byte, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	pub, err := kemScheme.UnmarshalBinaryPublicKey(recipientPub)
	if err != nil {
		return nil, fmt.Errorf("keyring: recipient key: %w", err)
	}
	sender, err := suite.NewSender(pub, info)
	if err != nil {
		return nil, err
	}
	enc, sealer, err := sender.Setup(rand.Reader)
	if err != nil {
		return nil, err
	}
	ct, err := sealer.Seal(key, nil)
	if err != nil {
		return nil, err
	}
	return append(enc, ct...), nil
}

// UnwrapKey unwraps a key produced by WrapKey for this node.
func (n *NodeKey) UnwrapKey(wrapped, info []byte) ([]byte, error) {
	encSize := kemScheme.CiphertextSize() // 32 for X25519
	if len(wrapped) <= encSize {
		return nil, ErrDecrypt
	}
	receiver, err := suite.NewReceiver(n.kemPriv, info)
	if err != nil {
		return nil, err
	}
	opener, err := receiver.Setup(wrapped[:encSize])
	if err != nil {
		return nil, ErrDecrypt
	}
	key, err := opener.Open(wrapped[encSize:], nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	if err := checkKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

// WrapInfo is the HPKE info string binding a wrapped workspace key to its
// workspace and version.
func WrapInfo(workspaceID string, version int) []byte {
	return []byte(fmt.Sprintf("kb-workspace-key\x00%s\x00%d", workspaceID, version))
}

// Sealed node key file: magic ‖ argon2id params ‖ salt ‖ nonce+ciphertext of the seed.
var sealedMagic = []byte("KBNK1\x00")

// SealParams tune Argon2id. Defaults: t=3, m=64 MiB, p=4.
type SealParams struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

// DefaultSealParams are the production parameters.
var DefaultSealParams = SealParams{Time: 3, Memory: 64 * 1024, Threads: 4}

// Seal encrypts the node seed under a passphrase.
func (n *NodeKey) Seal(passphrase []byte, p SealParams) ([]byte, error) {
	if len(passphrase) < 8 {
		return nil, errors.New("keyring: passphrase must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	kek := argon2.IDKey(passphrase, salt, p.Time, p.Memory, p.Threads, KeySize)
	defer Zero(kek)
	ct, err := seal(kek, sealedMagic, n.seed)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write(sealedMagic)
	_ = binary.Write(&buf, binary.BigEndian, p.Time)
	_ = binary.Write(&buf, binary.BigEndian, p.Memory)
	buf.WriteByte(p.Threads)
	buf.Write(salt)
	buf.Write(ct)
	return buf.Bytes(), nil
}

// UnsealNodeKey decrypts a sealed node key.
func UnsealNodeKey(sealed, passphrase []byte) (*NodeKey, error) {
	const hdr = 6 + 4 + 4 + 1 + 16
	if len(sealed) < hdr || !bytes.Equal(sealed[:6], sealedMagic) {
		return nil, errors.New("keyring: not a sealed node key")
	}
	t := binary.BigEndian.Uint32(sealed[6:10])
	m := binary.BigEndian.Uint32(sealed[10:14])
	th := sealed[14]
	salt := sealed[15:31]
	if t == 0 || m < 8*1024 || th == 0 {
		return nil, errors.New("keyring: implausible argon2 parameters")
	}
	kek := argon2.IDKey(passphrase, salt, t, m, th, KeySize)
	defer Zero(kek)
	seed, err := open(kek, sealedMagic, sealed[hdr:])
	if err != nil {
		return nil, fmt.Errorf("keyring: unseal node key: %w (wrong passphrase?)", err)
	}
	return nodeKeyFromSeed(seed)
}

// LoadOrCreateNodeKey unseals the key at path or, when the file does not exist,
// generates one, seals it under passphrase and writes it with mode 0600.
func LoadOrCreateNodeKey(path string, passphrase []byte, p SealParams) (key *NodeKey, created bool, err error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		k, err := UnsealNodeKey(data, passphrase)
		return k, false, err
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, false, err
	}
	k, err := GenerateNodeKey()
	if err != nil {
		return nil, false, err
	}
	sealed, err := k.Seal(passphrase, p)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return nil, false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, false, err
	}
	return k, true, nil
}
