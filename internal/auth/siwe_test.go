package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/acx1729/ocean/internal/did"
	"github.com/acx1729/ocean/internal/keyring"
)

// signEIP191 produces an Ethereum personal_sign signature [R||S||V] with V in {27,28}.
func signEIP191(priv *secp256k1.PrivateKey, msg []byte) []byte {
	compact := ecdsa.SignCompact(priv, EIP191Hash(msg), false) // [V+27][R][S]
	sig := make([]byte, 65)
	copy(sig[:64], compact[1:])
	sig[64] = compact[0]
	return sig
}

func TestSIWERoundTripAndRecovery(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := PubkeyToAddress(priv.PubKey())
	if !did.IsEVMAddress(addr) {
		t.Fatalf("bad address %s", addr)
	}
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	m := SIWEMessage{Domain: "kb.example.com", Address: addr, URI: "https://kb.example.com", Version: "1", ChainID: 1, Nonce: "abcdef0123456789", IssuedAt: now, ExpirationTime: now.Add(5 * time.Minute)}
	text := m.String()
	if !strings.HasPrefix(text, "kb.example.com wants you to sign in with your Ethereum account:\n"+addr+"\n\n\nURI: https://kb.example.com\nVersion: 1\nChain ID: 1\nNonce: abcdef0123456789\nIssued At: 2026-09-18T12:00:00Z\nExpiration Time: 2026-09-18T12:05:00Z") {
		t.Fatalf("unexpected layout:\n%s", text)
	}
	parsed, err := ParseSIWE(text)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Address != addr || parsed.ChainID != 1 || parsed.Nonce != m.Nonce || !parsed.ExpirationTime.Equal(m.ExpirationTime) || parsed.Domain != m.Domain {
		t.Fatalf("parsed mismatch: %+v", parsed)
	}
	sig := signEIP191(priv, []byte(text))
	got, err := RecoverAddress(EIP191Hash([]byte(text)), sig)
	if err != nil || !strings.EqualFold(got, addr) {
		t.Fatalf("recover: %s %v", got, err)
	}
	sig[10] ^= 0xff
	if got, err := RecoverAddress(EIP191Hash([]byte(text)), sig); err == nil && strings.EqualFold(got, addr) {
		t.Fatal("tampered signature must not recover the signer")
	}
	// EIP-55 checksum of a well-known address.
	if ChecksumAddress("0xfb6916095ca1df60bb79ce92ce3ea74c37c5d359") != "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359" {
		t.Fatal("checksum")
	}
}

func TestKeyMessageAndEd25519(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := did.FromEd25519(pub)
	now := time.Now()
	msg := KeyMessage("kb.example.com", d, "https://kb.example.com", "n0nce", now, now.Add(5*time.Minute))
	sig := ed25519.Sign(priv, []byte(msg))
	if err := VerifyEd25519(d, []byte(msg), sig); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEd25519(d, []byte(msg+" "), sig); err == nil {
		t.Fatal("modified message must fail")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifyEd25519(did.FromEd25519(other), []byte(msg), sig); err == nil {
		t.Fatal("other key must fail")
	}
}

func TestTokens(t *testing.T) {
	node, _ := keyring.GenerateNodeKey()
	iss, err := NewTokenIssuer(node, "https://kb.example.com", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, claims, err := iss.Issue(Claims{Subject: "did:key:zA", Owner: "did:key:zA", Kind: KindUser, SessionID: "sid1", DeviceID: "did:key:zDev"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !claims.ExpiresAt.Equal(now.Add(15 * time.Minute)) {
		t.Fatal("exp")
	}
	got, err := iss.Verify(tok, now.Add(time.Minute))
	if err != nil || got.Subject != "did:key:zA" || got.SessionID != "sid1" || got.DeviceID != "did:key:zDev" || got.Kind != KindUser {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if _, err := iss.Verify(tok, now.Add(16*time.Minute)); err == nil {
		t.Fatal("expired token accepted")
	}
	other, _ := keyring.GenerateNodeKey()
	iss2, _ := NewTokenIssuer(other, "https://kb.example.com", 15*time.Minute)
	if _, err := iss2.Verify(tok, now); err == nil {
		t.Fatal("token from another node accepted")
	}
	iss3, _ := NewTokenIssuer(node, "https://other.example.com", 15*time.Minute)
	if _, err := iss3.Verify(tok, now); err == nil {
		t.Fatal("token for another issuer accepted")
	}
	if TokenKind(tok) != KindUser || TokenKind("kba_x") != KindAgent || TokenKind("kbo_x") != KindOperator || TokenKind("junk") != "" {
		t.Fatal("token kinds")
	}
	if b, err := ParseBearer("Bearer abc"); err != nil || b != "abc" {
		t.Fatal("bearer")
	}
	if _, err := ParseBearer("Basic abc"); err == nil {
		t.Fatal("basic accepted")
	}
	op := OperatorToken(node)
	if !strings.HasPrefix(op, OperatorPrefix) || op != OperatorToken(node) {
		t.Fatal("operator token")
	}
}
