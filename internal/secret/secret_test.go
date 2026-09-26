package secret

import (
	"bytes"
	"testing"
)

func testKeys(t *testing.T) *Keys {
	t.Helper()
	k, err := New(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	k := testKeys(t)
	ct, err := k.Seal([]byte("refresh-token"), []byte("row1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("refresh-token")) {
		t.Fatal("ciphertext contains plaintext")
	}
	pt, err := k.Open(ct, []byte("row1"))
	if err != nil || string(pt) != "refresh-token" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := k.Open(ct, []byte("row2")); err == nil {
		t.Fatal("ciphertext opened under a different row key")
	}
}

func TestMACDomainSeparation(t *testing.T) {
	k := testKeys(t)
	a := k.MACString("basic", "ab", "c")
	b := k.MACString("basic", "a", "bc")
	c := k.MACString("oauth", "ab", "c")
	if Equal(a, b) || Equal(a, c) {
		t.Fatal("MAC inputs collide")
	}
	if !Equal(a, k.MACString("basic", "ab", "c")) {
		t.Fatal("MAC not deterministic")
	}
}

func TestToken(t *testing.T) {
	a, b := Token(32), Token(32)
	if a == b || len(a) != 43 {
		t.Fatalf("bad tokens %q %q", a, b)
	}
}
