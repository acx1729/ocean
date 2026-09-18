package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/acx1729/ocean/internal/did"
)

// SIWEMessage is an EIP-4361 message.
type SIWEMessage struct {
	Domain         string
	Address        string
	Statement      string
	URI            string
	Version        string
	ChainID        uint64
	Nonce          string
	IssuedAt       time.Time
	ExpirationTime time.Time
}

// String renders the message in the exact EIP-4361 ABNF layout.
func (m SIWEMessage) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s wants you to sign in with your Ethereum account:\n%s\n\n", m.Domain, m.Address)
	if m.Statement != "" {
		b.WriteString(m.Statement)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "URI: %s\nVersion: %s\nChain ID: %d\nNonce: %s\nIssued At: %s", m.URI, m.Version, m.ChainID, m.Nonce, m.IssuedAt.UTC().Format(time.RFC3339))
	if !m.ExpirationTime.IsZero() {
		fmt.Fprintf(&b, "\nExpiration Time: %s", m.ExpirationTime.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// ParseSIWE parses a message produced by String (strict layout).
func ParseSIWE(s string) (SIWEMessage, error) {
	var m SIWEMessage
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) < 7 {
		return m, errors.New("siwe: too short")
	}
	const suffix = " wants you to sign in with your Ethereum account:"
	if !strings.HasSuffix(lines[0], suffix) {
		return m, errors.New("siwe: bad header")
	}
	m.Domain = strings.TrimSuffix(lines[0], suffix)
	m.Address = lines[1]
	if lines[2] != "" {
		return m, errors.New("siwe: expected blank line after address")
	}
	i := 3
	if lines[i] != "" {
		m.Statement = lines[i]
		i++
		if i >= len(lines) || lines[i] != "" {
			return m, errors.New("siwe: expected blank line after statement")
		}
	}
	i++
	m.Version = ""
	for ; i < len(lines); i++ {
		key, val, ok := strings.Cut(lines[i], ": ")
		if !ok {
			return m, fmt.Errorf("siwe: bad field line %q", lines[i])
		}
		var err error
		switch key {
		case "URI":
			m.URI = val
		case "Version":
			m.Version = val
		case "Chain ID":
			m.ChainID, err = strconv.ParseUint(val, 10, 64)
		case "Nonce":
			m.Nonce = val
		case "Issued At":
			m.IssuedAt, err = time.Parse(time.RFC3339, val)
		case "Expiration Time":
			m.ExpirationTime, err = time.Parse(time.RFC3339, val)
		case "Not Before", "Request ID", "Resources":
			// accepted, unused
		default:
			if strings.HasPrefix(lines[i], "- ") {
				continue // resource list entry
			}
			return m, fmt.Errorf("siwe: unknown field %q", key)
		}
		if err != nil {
			return m, fmt.Errorf("siwe: field %s: %w", key, err)
		}
	}
	if m.Version != "1" || m.Nonce == "" || m.IssuedAt.IsZero() || !did.IsEVMAddress(m.Address) {
		return m, errors.New("siwe: missing required fields")
	}
	return m, nil
}

// KeyMessage is the challenge text signed by did:key principals. It mirrors the
// SIWE layout so both flows bind domain, nonce and expiry.
func KeyMessage(domain, didStr, uri, nonce string, issuedAt, expires time.Time) string {
	return fmt.Sprintf("%s wants you to sign in with your key:\n%s\n\nURI: %s\nVersion: 1\nNonce: %s\nIssued At: %s\nExpiration Time: %s",
		domain, didStr, uri, nonce, issuedAt.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339))
}

// Keccak256 hashes with the Ethereum variant of SHA-3.
func Keccak256(data ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	return h.Sum(nil)
}

// EIP191Hash returns keccak256("\x19Ethereum Signed Message:\n" + len(msg) + msg).
func EIP191Hash(msg []byte) []byte {
	prefix := []byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg)))
	return Keccak256(prefix, msg)
}

// RecoverAddress recovers the signer address from a 65-byte [R||S||V] signature.
func RecoverAddress(hash, sig []byte) (string, error) {
	if len(sig) != 65 {
		return "", fmt.Errorf("%w: signature must be 65 bytes", ErrInvalidSignature)
	}
	v := sig[64]
	if v >= 27 {
		v -= 27
	}
	if v > 1 {
		return "", fmt.Errorf("%w: bad recovery id", ErrInvalidSignature)
	}
	// dcrd's compact format is [V+27(+4 if compressed)][R][S].
	compact := make([]byte, 65)
	compact[0] = v + 27
	copy(compact[1:], sig[:64])
	pub, _, err := ecdsa.RecoverCompact(compact, hash)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	return PubkeyToAddress(pub), nil
}

// PubkeyToAddress derives the EIP-55 checksummed address of a public key.
func PubkeyToAddress(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed() // 0x04 || X || Y
	h := Keccak256(uncompressed[1:])
	return ChecksumAddress(hex.EncodeToString(h[12:]))
}

// ChecksumAddress applies EIP-55 casing to a hex address (with or without 0x).
func ChecksumAddress(addr string) string {
	addr = strings.ToLower(strings.TrimPrefix(addr, "0x"))
	h := hex.EncodeToString(Keccak256([]byte(addr)))
	var b strings.Builder
	b.WriteString("0x")
	for i, c := range addr {
		if c >= 'a' && c <= 'f' && h[i] >= '8' {
			b.WriteByte(byte(c) - 32)
		} else {
			b.WriteByte(byte(c))
		}
	}
	return b.String()
}

// VerifyEd25519 checks a did:key signature over message.
func VerifyEd25519(didStr string, message, sig []byte) error {
	pub, err := did.Ed25519PublicKey(didStr)
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, message, sig) {
		return ErrInvalidSignature
	}
	return nil
}

// EIP1271 verifies a contract wallet signature through eth_call isValidSignature(bytes32,bytes).
type EIP1271 struct {
	RPCURL string
	Client *http.Client
}

const eip1271Magic = "1626ba7e"

// IsValidSignature returns nil when the contract at address accepts the signature over hash.
func (e *EIP1271) IsValidSignature(ctx context.Context, address string, hash, sig []byte) error {
	if e == nil || e.RPCURL == "" {
		return ErrContractWallet
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// ABI: selector ‖ bytes32 hash ‖ offset(0x40) ‖ len(sig) ‖ sig padded to 32.
	data := make([]byte, 0, 4+32*3+len(sig)+32)
	sel, _ := hex.DecodeString(eip1271Magic)
	data = append(data, sel...)
	data = append(data, hash...)
	data = append(data, leftPad32(64)...)
	data = append(data, leftPad32(uint64(len(sig)))...)
	data = append(data, sig...)
	if rem := len(sig) % 32; rem != 0 {
		data = append(data, make([]byte, 32-rem)...)
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": address, "data": "0x" + hex.EncodeToString(data)}, "latest"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.RPCURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("eip1271: %w", err)
	}
	defer res.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return fmt.Errorf("eip1271: %w", err)
	}
	if out.Error != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSignature, out.Error.Message)
	}
	if !strings.HasPrefix(strings.TrimPrefix(out.Result, "0x"), eip1271Magic) {
		return ErrInvalidSignature
	}
	return nil
}

func leftPad32(n uint64) []byte {
	b := make([]byte, 32)
	for i := 0; i < 8; i++ {
		b[31-i] = byte(n >> (8 * i))
	}
	return b
}
