package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/more-than-code/defect-drainer-worker/internal/release"
)

func TestPubkeyFromPEM(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "k.pem")
	os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"pubkey", p}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != base64.StdEncoding.EncodeToString(pub) {
		t.Errorf("got %q", out.String())
	}
}

func TestVerifyCompiled(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	old := release.PublicKeys
	release.PublicKeys = []ed25519.PublicKey{pub}
	defer func() { release.PublicKeys = old }()
	dir := t.TempDir()
	sums := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  install.sh\n")
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sums, 0o600)
	os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), release.EncodeSig(ed25519.Sign(priv, sums)), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"verify-compiled", filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.sig")}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), append(sums, 'x'), 0o600)
	if code := run([]string{"verify-compiled", filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.sig")}, &out, &errb); code != 1 {
		t.Fatalf("changed sums: exit %d", code)
	}
}

func TestKeys(t *testing.T) {
	old := release.PublicKeys
	defer func() { release.PublicKeys = old }()
	// Whatever is compiled in: a placeholder build, and a mix (a real key
	// next to a placeholder is a mistake, so it is not signed).
	a, _, _ := ed25519.GenerateKey(rand.Reader)
	b, _, _ := ed25519.GenerateKey(rand.Reader)
	release.PublicKeys = []ed25519.PublicKey{make([]byte, ed25519.PublicKeySize)}
	var out, errb bytes.Buffer
	if code := run([]string{"keys"}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != "placeholder" {
		t.Fatalf("placeholder build: %d %q", code, out.String())
	}
	release.PublicKeys = []ed25519.PublicKey{a, make([]byte, ed25519.PublicKeySize)}
	out.Reset()
	if code := run([]string{"keys"}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != "placeholder" {
		t.Fatalf("mixed: %d %q", code, out.String())
	}
	release.PublicKeys = []ed25519.PublicKey{a, b}
	out.Reset()
	if code := run([]string{"keys"}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != "real 2" {
		t.Fatalf("real keys: %d %q", code, out.String())
	}
}
