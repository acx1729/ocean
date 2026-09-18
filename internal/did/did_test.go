package did

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func TestEd25519RoundTrip(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d := FromEd25519(pub)
	if !strings.HasPrefix(d, "did:key:z6Mk") {
		t.Fatalf("unexpected prefix: %s", d)
	}
	kt, raw, err := ParseKey(d)
	if err != nil || kt != Ed25519 || string(raw) != string(pub) {
		t.Fatalf("parse: %v %v", kt, err)
	}
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
}

// A did:key from the did:key specification test vectors must decode to a
// 32-byte Ed25519 key and re-encode to the identical string.
func TestKnownVector(t *testing.T) {
	const known = "did:key:z6MkiTBz1ymuepAQ4HEHYSF1H8quG5GLVVQR3djdX3mDooWp"
	kt, raw, err := ParseKey(known)
	if err != nil || kt != Ed25519 || len(raw) != ed25519.PublicKeySize {
		t.Fatalf("parse known vector: kt=%v len=%d err=%v", kt, len(raw), err)
	}
	if got := FromEd25519(ed25519.PublicKey(raw)); got != known {
		t.Fatalf("re-encode: got %s want %s", got, known)
	}
	if _, err := hex.DecodeString(hex.EncodeToString(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestX25519(t *testing.T) {
	var pub [32]byte
	rand.Read(pub[:])
	d := FromX25519(pub[:])
	if !strings.HasPrefix(d, "did:key:z6LS") {
		t.Fatalf("unexpected prefix: %s", d)
	}
	kt, raw, err := ParseKey(d)
	if err != nil || kt != X25519 || string(raw) != string(pub[:]) {
		t.Fatal(err)
	}
}

func TestPKH(t *testing.T) {
	d := "did:pkh:eip155:1:0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B"
	p, err := ParsePKH(d)
	if err != nil || p.ChainID != 1 || p.Address != "0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B" {
		t.Fatalf("%+v %v", p, err)
	}
	if Normalize(d) != strings.ToLower(d) {
		t.Fatalf("normalize: %s", Normalize(d))
	}
	if KindOf(d) != KindPKH || KindOf("did:web:x") != KindUnknown {
		t.Fatal("kind")
	}
	for _, bad := range []string{"did:pkh:eip155:1:0x123", "did:pkh:solana:1:abc", "did:key:abc", "did:key:z", "", "did:pkh:eip155:x:0xAb5801a7D398351b8bE11C439e05C5B3259aeC9B"} {
		if err := Validate(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
