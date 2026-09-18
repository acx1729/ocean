// Package did encodes and parses the decentralized identifiers used for every
// principal (specification section 4): did:key for Ed25519 and X25519 public
// keys and did:pkh for EVM accounts.
package did

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Multicodec prefixes (unsigned varint encoded) for the supported key types.
var (
	prefixEd25519 = []byte{0xed, 0x01} // ed25519-pub 0xed
	prefixX25519  = []byte{0xec, 0x01} // x25519-pub 0xec
)

// ErrInvalid is returned for malformed identifiers.
var ErrInvalid = errors.New("invalid did")

// KeyType names the key behind a did:key.
type KeyType string

const (
	Ed25519 KeyType = "ed25519"
	X25519  KeyType = "x25519"
)

// FromEd25519 returns did:key:z6Mk… for an Ed25519 public key.
func FromEd25519(pub ed25519.PublicKey) string {
	if len(pub) != ed25519.PublicKeySize {
		return ""
	}
	return "did:key:z" + base58Encode(append(append([]byte{}, prefixEd25519...), pub...))
}

// FromX25519 returns did:key:z6LS… for an X25519 public key.
func FromX25519(pub []byte) string {
	if len(pub) != 32 {
		return ""
	}
	return "did:key:z" + base58Encode(append(append([]byte{}, prefixX25519...), pub...))
}

// ParseKey decodes a did:key into its key type and raw public key bytes.
func ParseKey(did string) (KeyType, []byte, error) {
	rest, ok := strings.CutPrefix(did, "did:key:z")
	if !ok || rest == "" {
		return "", nil, fmt.Errorf("%w: %q is not a did:key with base58btc encoding", ErrInvalid, did)
	}
	raw, err := base58Decode(rest)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	switch {
	case len(raw) == 34 && raw[0] == prefixEd25519[0] && raw[1] == prefixEd25519[1]:
		return Ed25519, raw[2:], nil
	case len(raw) == 34 && raw[0] == prefixX25519[0] && raw[1] == prefixX25519[1]:
		return X25519, raw[2:], nil
	}
	return "", nil, fmt.Errorf("%w: unsupported did:key multicodec", ErrInvalid)
}

// Ed25519PublicKey extracts the Ed25519 public key from a did:key.
func Ed25519PublicKey(did string) (ed25519.PublicKey, error) {
	kt, raw, err := ParseKey(did)
	if err != nil {
		return nil, err
	}
	if kt != Ed25519 {
		return nil, fmt.Errorf("%w: not an Ed25519 key", ErrInvalid)
	}
	return ed25519.PublicKey(raw), nil
}

// PKH is a parsed did:pkh identifier.
type PKH struct {
	Namespace string // eip155
	ChainID   uint64
	Address   string // EIP-55 case preserved as given; compare with EqualFold
}

// String renders the identifier.
func (p PKH) String() string {
	return fmt.Sprintf("did:pkh:%s:%d:%s", p.Namespace, p.ChainID, p.Address)
}

// ParsePKH parses did:pkh:eip155:<chain>:<address>.
func ParsePKH(did string) (PKH, error) {
	parts := strings.Split(did, ":")
	if len(parts) != 5 || parts[0] != "did" || parts[1] != "pkh" {
		return PKH{}, fmt.Errorf("%w: %q is not a did:pkh", ErrInvalid, did)
	}
	if parts[2] != "eip155" {
		return PKH{}, fmt.Errorf("%w: unsupported namespace %q", ErrInvalid, parts[2])
	}
	chain, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return PKH{}, fmt.Errorf("%w: chain id %q", ErrInvalid, parts[3])
	}
	if !IsEVMAddress(parts[4]) {
		return PKH{}, fmt.Errorf("%w: address %q", ErrInvalid, parts[4])
	}
	return PKH{Namespace: "eip155", ChainID: chain, Address: parts[4]}, nil
}

// FromEVMAddress returns did:pkh:eip155:<chain>:<address>.
func FromEVMAddress(chainID uint64, address string) string {
	return PKH{Namespace: "eip155", ChainID: chainID, Address: address}.String()
}

// IsEVMAddress reports whether s is 0x followed by 40 hex digits.
func IsEVMAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, c := range s[2:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// Kind classifies a DID string.
type Kind string

const (
	KindKey     Kind = "key"
	KindPKH     Kind = "pkh"
	KindUnknown Kind = ""
)

// KindOf returns the DID method.
func KindOf(did string) Kind {
	switch {
	case strings.HasPrefix(did, "did:key:"):
		return KindKey
	case strings.HasPrefix(did, "did:pkh:"):
		return KindPKH
	}
	return KindUnknown
}

// Validate checks that did is a well-formed did:key or did:pkh.
func Validate(did string) error {
	switch KindOf(did) {
	case KindKey:
		_, _, err := ParseKey(did)
		return err
	case KindPKH:
		_, err := ParsePKH(did)
		return err
	}
	return fmt.Errorf("%w: unsupported method in %q", ErrInvalid, did)
}

// Normalize canonicalizes a DID: did:pkh addresses are lower-cased so the same
// account always maps to one principal row.
func Normalize(did string) string {
	if KindOf(did) == KindPKH {
		p, err := ParsePKH(did)
		if err == nil {
			p.Address = strings.ToLower(p.Address)
			return p.String()
		}
	}
	return did
}

const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(b []byte) string {
	zeros := 0
	for zeros < len(b) && b[zeros] == 0 {
		zeros++
	}
	n := new(big.Int).SetBytes(b)
	radix := big.NewInt(58)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, b58alphabet[mod.Int64()])
	}
	for i := 0; i < zeros; i++ {
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func base58Decode(s string) ([]byte, error) {
	n := new(big.Int)
	radix := big.NewInt(58)
	for _, c := range s {
		idx := strings.IndexRune(b58alphabet, c)
		if idx < 0 {
			return nil, fmt.Errorf("invalid base58 character %q", c)
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(idx)))
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	b := n.Bytes()
	out := make([]byte, zeros+len(b))
	copy(out[zeros:], b)
	return out, nil
}
