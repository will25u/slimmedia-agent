package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// ReleasePublicKey is the Ed25519 public key used to verify agent release
// binaries. Release builds inject the real key via -ldflags -X; the all-zero
// 32-byte sentinel here is the dev/test fallback. Verification fails closed:
// an embedded sentinel refuses every update (see VerifySignature).
//
// Inject with:
//   -ldflags "-X github.com/will25u/slimmedia-agent/internal/updater.ReleasePublicKeyHex=<64-char hex>"
var ReleasePublicKeyHex = "0000000000000000000000000000000000000000000000000000000000000000"

// releasePublicKey returns the decoded Ed25519 public key from ReleasePublicKeyHex.
// It panics on a malformed injected value; the zero sentinel is always valid.
func releasePublicKey() (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(ReleasePublicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("RELEASE_PUBLIC_KEY_HEX is not valid hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("RELEASE_PUBLIC_KEY_HEX must be %d bytes (got %d)", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}

// EmbeddedKeyConfigured reports whether the embedded release public key is a
// real Ed25519 key. The all-zero dev sentinel and malformed injected values
// both report false: such builds refuse every update at signature
// verification, so startup logging can say so loudly.
func EmbeddedKeyConfigured() bool {
	pub, err := releasePublicKey()
	if err != nil {
		return false
	}
	return !isDevSentinel(pub)
}

// isDevSentinel reports whether pub is the all-zero dev sentinel. Uses
// constant-time compare to avoid a timing oracle.
func isDevSentinel(pub ed25519.PublicKey) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	zero := make([]byte, ed25519.PublicKeySize)
	return subtle.ConstantTimeCompare(pub, zero) == 1
}

// ReleaseSignaturePayload constructs the canonical byte sequence signed for a
// release binary (version||sha256).
func ReleaseSignaturePayload(version, sha256Hex string) []byte {
	return []byte(version + "||" + strings.ToLower(strings.TrimSpace(sha256Hex)))
}

// VerifySignature verifies that sigHex (hex-encoded Ed25519 signature) is a
// valid signature under pub. When version is provided (non-empty), the signature
// is verified over ReleaseSignaturePayload(version, sha256Hex) where sha256Hex
// is the hex-encoded SHA-256 of binary, binding the target version to prevent
// replay and downgrade attacks. When version is empty or omitted, verification
// falls back to the raw binary bytes for compatibility.
//
// Verification is enforced unconditionally: when pub is the all-zero dev
// sentinel (isDevSentinel) every update is refused, and an empty sigHex is
// refused whenever a checksum is present, so a release build never installs
// an unsigned or unverifiable binary.
//
// On any error — dev-sentinel key, malformed hex, wrong signature length,
// failed verification — a descriptive error is returned and the caller must
// refuse to install the binary.
func VerifySignature(pub ed25519.PublicKey, binary []byte, sigHex string, version ...string) error {
	if isDevSentinel(pub) {
		return fmt.Errorf("refusing update: release signing key is the all-zero dev sentinel; signature verification is enforced")
	}

	if sigHex == "" {
		return fmt.Errorf("refusing update: no Ed25519 signature provided and release key is configured")
	}

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("signature is not valid hex: %w", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("signature must be %d bytes, got %d", ed25519.SignatureSize, len(sigBytes))
	}

	payload := binary
	if len(version) > 0 && version[0] != "" {
		sum := sha256.Sum256(binary)
		payload = ReleaseSignaturePayload(version[0], hex.EncodeToString(sum[:]))
	}

	if !ed25519.Verify(pub, payload, sigBytes) {
		return fmt.Errorf("Ed25519 signature verification failed: binary may be tampered or signed with an unknown key")
	}
	return nil
}
