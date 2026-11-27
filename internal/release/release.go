// Package release checks a dd-worker release before a host updates to it
// (docs/adr/0002-self-update.md §3, §5): the Ed25519 signature over the
// release's SHA256SUMS, a file's entry in those sums, and the version rules
// (newer only, same major and minor, pre-releases only as rcN).
//
// SHA256SUMS.sig is one line: the base64 of a plain Ed25519 signature
// (RFC 8032 PureEdDSA) over the exact bytes of SHA256SUMS. An AWS KMS key of
// spec ECC_NIST_EDWARDS25519 signing with ED25519_SHA_512 and MessageType
// RAW makes exactly that signature.
package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// MaxSigFile bounds SHA256SUMS.sig: 88 base64 characters, a newline and
// some slack for a CRLF. MaxSums bounds SHA256SUMS (KMS signs RAW messages
// of at most 4096 bytes).
const (
	MaxSigFile = 128
	MaxSums    = 4096
)

// PublicKeys are the release keys compiled into dd-worker: the KMS release
// key that the release workflow signs with, then the KMS recovery key that
// signs only a release that rotates keys. A signature verifies when any of
// them accepts it.
//
// PLACEHOLDER: one all-zero key. The maintainer replaces it with the two
// real public keys (base64 of the 32 raw bytes; `release-sign pubkey`
// converts the PEM KMS shows) before the first signed release. A build with
// only the placeholder refuses every update, and `go test -tags release
// ./internal/release` fails on it.
var PublicKeys = []ed25519.PublicKey{
	make([]byte, ed25519.PublicKeySize),
}

var (
	ErrPlaceholder = errors.New("release key is the placeholder")
	ErrNoKeys      = errors.New("no release key")
	ErrBadKey      = errors.New("bad public key")
	ErrBadSig      = errors.New("bad signature file")
	ErrMismatch    = errors.New("signature does not verify")
	ErrBadSums     = errors.New("bad SHA256SUMS")
	ErrNoEntry     = errors.New("SHA256SUMS has no single entry for the file")
)

// IsPlaceholder reports whether pub is the all-zero placeholder key.
func IsPlaceholder(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && bytes.Equal(pub, make([]byte, ed25519.PublicKeySize))
}

// EncodeSig is the SHA256SUMS.sig content for a raw signature.
func EncodeSig(sig []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// DecodeSig reads a SHA256SUMS.sig file: one base64 line of 64 bytes, with
// an optional trailing newline.
func DecodeSig(file []byte) ([]byte, error) {
	if len(file) == 0 || len(file) > MaxSigFile {
		return nil, ErrBadSig
	}
	line := bytes.TrimSuffix(file, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if bytes.ContainsAny(line, "\r\n \t") {
		return nil, ErrBadSig
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(string(line))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrBadSig
	}
	return sig, nil
}

// Verify checks sigFile (the SHA256SUMS.sig bytes) over sums (the exact
// SHA256SUMS bytes) against keys, accepting the first that verifies. An
// empty list, a malformed key and a list holding only placeholders are
// refused; a placeholder next to real keys is skipped.
func Verify(keys []ed25519.PublicKey, sums, sigFile []byte) error {
	if len(keys) == 0 {
		return ErrNoKeys
	}
	if len(sums) == 0 || len(sums) > MaxSums {
		return ErrBadSums
	}
	nReal := 0
	for _, k := range keys {
		if len(k) != ed25519.PublicKeySize {
			return ErrBadKey
		}
		if !IsPlaceholder(k) {
			nReal++
		}
	}
	if nReal == 0 {
		return ErrPlaceholder
	}
	sig, err := DecodeSig(sigFile)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if !IsPlaceholder(k) && ed25519.Verify(k, sums, sig) {
			return nil
		}
	}
	return ErrMismatch
}

// OnlyPlaceholder reports whether keys holds no real key.
func OnlyPlaceholder(keys []ed25519.PublicKey) bool {
	for _, k := range keys {
		if !IsPlaceholder(k) {
			return false
		}
	}
	return true
}

// DecodeKey reads one base64 line of a 32-byte Ed25519 public key.
func DecodeKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrBadKey
	}
	return ed25519.PublicKey(b), nil
}

// SumFor returns the lowercase hex SHA-256 that sums lists for name. Lines
// are "<64 hex>  <name>" (or "*<name>", binary mode); exactly one entry
// must match.
func SumFor(sums []byte, name string) (string, error) {
	if len(sums) == 0 || len(sums) > MaxSums {
		return "", ErrBadSums
	}
	found := ""
	n := 0
	for _, line := range strings.Split(strings.TrimRight(string(sums), "\n"), "\n") {
		sum, file, ok := strings.Cut(line, "  ")
		if !ok {
			sum, file, ok = strings.Cut(line, " *")
		}
		if !ok {
			return "", ErrBadSums
		}
		file = strings.TrimPrefix(file, "*")
		if file != name {
			continue
		}
		if len(sum) != 64 {
			return "", ErrBadSums
		}
		if _, err := hex.DecodeString(sum); err != nil || strings.ToLower(sum) != sum {
			return "", ErrBadSums
		}
		found = sum
		n++
	}
	if n != 1 {
		return "", ErrNoEntry
	}
	return found, nil
}
