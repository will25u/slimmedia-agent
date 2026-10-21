package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestVerifySignatureAcceptsValidSig: a binary signed with the matching private
// key must pass verification.
func TestVerifySignatureAcceptsValidSig(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	payload := []byte("the real agent binary bytes")
	sig := ed25519.Sign(priv, payload)

	if err := VerifySignature(pub, payload, hex.EncodeToString(sig)); err != nil {
		t.Fatalf("VerifySignature returned error for valid sig: %v", err)
	}
}

// TestVerifySignatureRejectsBadSig: a tampered binary must be rejected.
func TestVerifySignatureRejectsBadSig(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	payload := []byte("the real agent binary bytes")
	sig := ed25519.Sign(priv, payload)

	tampered := append([]byte{}, payload...)
	tampered[0] ^= 0xFF

	if err := VerifySignature(pub, tampered, hex.EncodeToString(sig)); err == nil {
		t.Fatal("VerifySignature accepted tampered binary")
	}
}

// TestVerifySignatureRejectsWrongKey: a signature from a different key must be
// rejected even when the binary bytes are unchanged.
func TestVerifySignatureRejectsWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	payload := []byte("the real agent binary bytes")
	sig := ed25519.Sign(priv, payload)

	if err := VerifySignature(pub2, payload, hex.EncodeToString(sig)); err == nil {
		t.Fatal("VerifySignature accepted sig from wrong key")
	}
}

// TestVerifySignatureRejectsMalformedHex: an invalid hex string must return an
// error without panicking.
func TestVerifySignatureRejectsMalformedHex(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifySignature(pub, []byte("x"), "not-hex!"); err == nil {
		t.Fatal("VerifySignature accepted malformed hex signature")
	}
}

// TestVerifySignatureRejectsWrongLength: a correctly-encoded hex value that is
// not 64 bytes must be rejected without panicking.
func TestVerifySignatureRejectsWrongLength(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	shortSig := hex.EncodeToString([]byte("tooshort"))
	if err := VerifySignature(pub, []byte("x"), shortSig); err == nil {
		t.Fatal("VerifySignature accepted wrong-length signature")
	}
}

// withReleaseKey swaps ReleasePublicKeyHex for a generated Ed25519 public key
// for the duration of the test, returning the private key so the test can
// sign payloads the verifier will accept.
func withReleaseKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	orig := ReleasePublicKeyHex
	ReleasePublicKeyHex = hex.EncodeToString(pub)
	t.Cleanup(func() { ReleasePublicKeyHex = orig })
	return priv
}

// TestVerifySignatureRejectsEmptySignature: a real key with no signature must
// be refused — a checksum present means a signature is required.
func TestVerifySignatureRejectsEmptySignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifySignature(pub, []byte("binary"), ""); err == nil {
		t.Fatal("VerifySignature accepted an empty signature with a real key")
	}
}

// TestDevSentinelRefusesVerification: the all-zero dev sentinel (32 zero
// bytes) fails closed — every update is refused, signed or not, until a real
// release key is injected.
func TestDevSentinelRefusesVerification(t *testing.T) {
	zeroPub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	payload := []byte("some binary")

	if err := VerifySignature(zeroPub, payload, ""); err == nil {
		t.Fatal("VerifySignature accepted the dev sentinel key with no signature")
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := ed25519.Sign(priv, payload)
	if err := VerifySignature(zeroPub, payload, hex.EncodeToString(sig)); err == nil {
		t.Fatal("VerifySignature accepted a valid signature under the dev sentinel key")
	}
}

// TestEmbeddedKeyConfigured: the default all-zero sentinel reports false; a
// real injected Ed25519 key reports true.
func TestEmbeddedKeyConfigured(t *testing.T) {
	if EmbeddedKeyConfigured() {
		t.Fatal("EmbeddedKeyConfigured = true for the default dev sentinel key")
	}

	withReleaseKey(t)
	if !EmbeddedKeyConfigured() {
		t.Fatal("EmbeddedKeyConfigured = false for a real Ed25519 key")
	}
}

// TestVerifySignatureWithVersion: verifies that signatures covering version||sha256
// succeed when version matches, and fail when version mismatches (anti-downgrade).
func TestVerifySignatureWithVersion(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	payload := []byte("the real agent binary bytes")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])
	version := "2.6.4"

	signedPayload := ReleaseSignaturePayload(version, sumHex)
	sig := ed25519.Sign(priv, signedPayload)
	sigHex := hex.EncodeToString(sig)

	// Valid matching version
	if err := VerifySignature(pub, payload, sigHex, version); err != nil {
		t.Fatalf("VerifySignature failed for matching version: %v", err)
	}

	// Mismatched / replayed older version (anti-downgrade check)
	if err := VerifySignature(pub, payload, sigHex, "2.6.3"); err == nil {
		t.Fatal("VerifySignature accepted signature for different version (downgrade not prevented)")
	}

	// Tampered binary under same version
	tampered := append([]byte{}, payload...)
	tampered[0] ^= 0xFF
	if err := VerifySignature(pub, tampered, sigHex, version); err == nil {
		t.Fatal("VerifySignature accepted tampered binary under target version")
	}
}

