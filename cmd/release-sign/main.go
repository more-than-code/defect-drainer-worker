// Command release-sign is a release-time helper (docs/adr/0002-self-update.md
// §3). It is never shipped in a release tarball and holds no key: releases
// are signed by AWS KMS in the release workflow.
//
//	release-sign verify-compiled <SHA256SUMS> <SHA256SUMS.sig>
//	    exit 0 when the signature verifies with a public key compiled into
//	    dd-worker (internal/release.PublicKeys); the sign job runs it before
//	    it uploads a signature
//	release-sign keys
//	    prints "real <n>" when every compiled key is real, else
//	    "placeholder" (exit 0 either way); the release workflow signs only
//	    with real keys
//	release-sign pubkey <public-key.pem>
//	    prints the base64 of the 32-byte Ed25519 key in a PEM
//	    SubjectPublicKeyInfo (what the KMS console's Public key tab shows),
//	    the form PublicKeys takes
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/more-than-code/defect-drainer-worker/internal/release"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "verify-compiled":
		if len(args) != 3 {
			return usage(stderr)
		}
		if err := verifyCompiled(args[1], args[2]); err != nil {
			fmt.Fprintln(stderr, "release-sign:", err)
			return 1
		}
		fmt.Fprintln(stdout, "signature verifies with a compiled key")
		return 0
	case "keys":
		if len(args) != 1 {
			return usage(stderr)
		}
		for _, k := range release.PublicKeys {
			if len(k) != ed25519.PublicKeySize || release.IsPlaceholder(k) {
				fmt.Fprintln(stdout, "placeholder")
				return 0
			}
		}
		if len(release.PublicKeys) == 0 {
			fmt.Fprintln(stdout, "placeholder")
			return 0
		}
		fmt.Fprintf(stdout, "real %d\n", len(release.PublicKeys))
		return 0
	case "pubkey":
		if len(args) != 2 {
			return usage(stderr)
		}
		k, err := pubkeyFromPEM(args[1])
		if err != nil {
			fmt.Fprintln(stderr, "release-sign:", err)
			return 1
		}
		fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(k))
		return 0
	}
	return usage(stderr)
}

func usage(stderr io.Writer) int {
	fmt.Fprintln(stderr, "usage: release-sign verify-compiled <SHA256SUMS> <SHA256SUMS.sig>\n       release-sign keys\n       release-sign pubkey <public-key.pem>")
	return 2
}

func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", path, max)
	}
	return b, nil
}

func verifyCompiled(sumsPath, sigPath string) error {
	sums, err := readCapped(sumsPath, release.MaxSums)
	if err != nil {
		return err
	}
	sig, err := readCapped(sigPath, release.MaxSigFile)
	if err != nil {
		return err
	}
	return release.Verify(release.PublicKeys, sums, sig)
}

func pubkeyFromPEM(path string) (ed25519.PublicKey, error) {
	b, err := readCapped(path, 4096)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("no PUBLIC KEY PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	k, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return k, nil
}
