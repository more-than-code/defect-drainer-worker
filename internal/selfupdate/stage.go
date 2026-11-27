package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BundleFiles are the only names a release tarball may hold
// (packaging/dist.sh). dd-worker and install.sh must be there.
var BundleFiles = map[string]bool{
	"dd-worker":                true,
	"install.sh":               true,
	"dd-worker.service":        true,
	"dd-worker-update.service": true,
	"dd-worker-update.path":    true,
	"policy.example.toml":      true,
	"README-worker.md":         true,
	"LICENSE":                  true,
	"NOTICE":                   true,
}

// MaxBundle bounds the unpacked size.
const MaxBundle = 256 << 20

var errBundle = errors.New("bundle_invalid")

// unpack writes the tarball's files into dir (which must exist, empty):
// regular files only, each a name in BundleFiles, at most once, under
// MaxBundle in all. dd-worker and install.sh get 0755, the rest 0644.
func unpack(tgz []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return fmt.Errorf("%w: %v", errBundle, err)
	}
	tr := tar.NewReader(io.LimitReader(zr, MaxBundle+1<<20))
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errBundle, err)
		}
		if h.Typeflag != tar.TypeReg || !BundleFiles[h.Name] || seen[h.Name] {
			return fmt.Errorf("%w: unexpected entry %q", errBundle, h.Name)
		}
		seen[h.Name] = true
		total += h.Size
		if h.Size < 0 || total > MaxBundle {
			return fmt.Errorf("%w: too large", errBundle)
		}
		mode := os.FileMode(0o644)
		if h.Name == "dd-worker" || h.Name == "install.sh" {
			mode = 0o755
		}
		f, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(tr, h.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if n != h.Size {
			return fmt.Errorf("%w: short entry %q", errBundle, h.Name)
		}
	}
	if !seen["dd-worker"] || !seen["install.sh"] {
		return fmt.Errorf("%w: no dd-worker or install.sh", errBundle)
	}
	return nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}
