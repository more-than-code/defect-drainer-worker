package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

const sums = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  dd-worker_0.2.1_linux_arm64.tar.gz\n" +
	"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210  install.sh\n"

func TestVerify(t *testing.T) {
	pub, priv := newKey(t)
	other, otherPriv := newKey(t)
	placeholder := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	sig := EncodeSig(ed25519.Sign(priv, []byte(sums)))
	sigOther := EncodeSig(ed25519.Sign(otherPriv, []byte(sums)))

	cases := []struct {
		name string
		keys []ed25519.PublicKey
		sums string
		sig  []byte
		want error
	}{
		{"good", []ed25519.PublicKey{pub}, sums, sig, nil},
		{"second key (recovery)", []ed25519.PublicKey{other, pub}, sums, sig, nil},
		{"placeholder skipped next to a real key", []ed25519.PublicKey{placeholder, pub}, sums, sig, nil},
		{"wrong key", []ed25519.PublicKey{pub}, sums, sigOther, ErrMismatch},
		{"changed sums", []ed25519.PublicKey{pub}, strings.Replace(sums, "0123", "0124", 1), sig, ErrMismatch},
		{"only placeholder", []ed25519.PublicKey{placeholder}, sums, sig, ErrPlaceholder},
		{"no keys", nil, sums, sig, ErrNoKeys},
		{"short key", []ed25519.PublicKey{pub[:31]}, sums, sig, ErrBadKey},
		{"empty sums", []ed25519.PublicKey{pub}, "", sig, ErrBadSums},
		{"huge sums", []ed25519.PublicKey{pub}, strings.Repeat("x", MaxSums+1), sig, ErrBadSums},
		{"garbage sig", []ed25519.PublicKey{pub}, sums, []byte("not base64!\n"), ErrBadSig},
		{"two lines", []ed25519.PublicKey{pub}, sums, append(append([]byte{}, sig...), sig...), ErrBadSig},
		{"short sig", []ed25519.PublicKey{pub}, sums, []byte(base64.StdEncoding.EncodeToString(make([]byte, 63)) + "\n"), ErrBadSig},
	}
	for _, c := range cases {
		if err := Verify(c.keys, []byte(c.sums), c.sig); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	// CRLF and no newline both read.
	crlf := []byte(strings.TrimSuffix(string(sig), "\n") + "\r\n")
	if err := Verify([]ed25519.PublicKey{pub}, []byte(sums), crlf); err != nil {
		t.Errorf("CRLF: %v", err)
	}
	if err := Verify([]ed25519.PublicKey{pub}, []byte(sums), []byte(strings.TrimSuffix(string(sig), "\n"))); err != nil {
		t.Errorf("no newline: %v", err)
	}
}

func TestSumFor(t *testing.T) {
	got, err := SumFor([]byte(sums), "install.sh")
	if err != nil || got != "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210" {
		t.Fatalf("install.sh: %q %v", got, err)
	}
	if _, err := SumFor([]byte(sums), "missing"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("missing: %v", err)
	}
	dup := sums + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210  install.sh\n"
	if _, err := SumFor([]byte(dup), "install.sh"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := SumFor([]byte("abc  install.sh\n"), "install.sh"); !errors.Is(err, ErrBadSums) {
		t.Errorf("short sum: %v", err)
	}
	upper := strings.ToUpper("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210") + "  install.sh\n"
	if _, err := SumFor([]byte(upper), "install.sh"); !errors.Is(err, ErrBadSums) {
		t.Errorf("uppercase: %v", err)
	}
	if _, err := SumFor([]byte("nonsense\n"), "install.sh"); !errors.Is(err, ErrBadSums) {
		t.Errorf("no separator: %v", err)
	}
	bin := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210 *install.sh\n"
	if _, err := SumFor([]byte(bin), "install.sh"); err != nil {
		t.Errorf("binary mode: %v", err)
	}
}

func TestDecodeKey(t *testing.T) {
	pub, _ := newKey(t)
	k, err := DecodeKey(base64.StdEncoding.EncodeToString(pub) + "\n")
	if err != nil || !k.Equal(pub) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := DecodeKey("AAAA"); !errors.Is(err, ErrBadKey) {
		t.Errorf("short: %v", err)
	}
}

func TestVersionOrder(t *testing.T) {
	order := []string{"0.1.9", "0.2.0-rc1", "0.2.0-rc9", "0.2.0-rc10", "0.2.0", "0.2.1-rc1", "0.2.1", "0.10.0", "1.0.0"}
	for i := range order {
		for j := range order {
			a, err := ParseVersion(order[i])
			if err != nil {
				t.Fatal(err)
			}
			b, _ := ParseVersion(order[j])
			if got, want := a.Compare(b), sign(i-j); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
		if v, _ := ParseVersion(order[i]); v.String() != order[i] {
			t.Errorf("String(%s) = %s", order[i], v.String())
		}
	}
	for _, bad := range []string{"", "v0.2.0", "0.2", "0.2.0-beta1", "0.2.0-rc", "0.2.0-rc0", "0.2.0-rc01", "0.2.0+x", "00000.1.1", "0.2.0-rc1.2", "0.2.0\n"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
}

func TestAllowed(t *testing.T) {
	cases := []struct{ cur, target, want string }{
		{"0.2.0-rc9", "0.2.0-rc10", ""},
		{"0.2.0-rc10", "0.2.0", ""},
		{"0.2.0", "0.2.1", ""},
		{"0.2.0-rc9", "0.2.3", ""},
		{"0.2.0-rc10", "0.2.0-rc9", ReasonNotNewer},
		{"0.2.1", "0.2.1", ReasonNotNewer},
		{"0.2.1", "0.2.0", ReasonNotNewer},
		{"0.2.5", "0.3.0", ReasonCrossMinor},
		{"0.2.5", "1.2.5", ReasonCrossMinor},
		{"0.2.0", "0.2.1-beta", ReasonNotAllowed},
		{"dev", "0.2.1", ReasonNotAllowed},
	}
	for _, c := range cases {
		if got := Allowed(c.cur, c.target); got != c.want {
			t.Errorf("Allowed(%s, %s) = %q, want %q", c.cur, c.target, got, c.want)
		}
	}
}
