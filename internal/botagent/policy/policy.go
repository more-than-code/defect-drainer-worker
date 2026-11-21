// Package policy reads a root-owned host policy file in a small TOML subset.
//
// Vendored from bot-agent internal/agent/policy (Apache-2.0, see ../NOTICE).
// DD changes: Load keeps bot-agent's file checks (regular file, size cap,
// root owner, not group- or world-writable, SHA-256 of the bytes) and its
// parser, but returns the parsed values for a caller-supplied Schema. The
// bot-agent keys (run_as, dirs, images, projects, shared networks) and the
// passwd/group resolution are dropped; dd-worker's keys and their meaning
// live in internal/worker.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"syscall"
)

// MaxBytes bounds a policy file. Larger files are invalid.
const MaxBytes = 64 * 1024

// File is one read of the policy. When Valid is false, Values is nil and
// Reason names why. Hash is the hex SHA-256 of the file bytes when they
// were read.
type File struct {
	Exists bool
	Valid  bool
	Values map[string]Value
	Hash   string
	Reason string
}

// Files selects the policy file and its schema.
// UID, when set, replaces the uid from the file's stat. Production leaves it
// nil. Tests set it because this process cannot chown a file to root.
// The override does not affect the mode check.
// SkipOwner skips the root-owner and mode checks (a "policy check" of a
// file before it is installed).
// Data, when not nil, is the policy's bytes (a candidate read from stdin):
// Policy is not opened and the owner and mode checks are skipped.
type Files struct {
	Policy    string
	Schema    Schema
	UID       func(os.FileInfo) (uint32, bool)
	SkipOwner bool
	Data      []byte
}

// Load reads the policy. A missing file returns a zero File (Exists false).
// It does not return an error: a missing or invalid policy disables work.
func Load(f Files) File {
	if f.Data != nil {
		return parseData(f.Data, f.Schema)
	}
	fh, err := os.Open(f.Policy)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}
		}
		return File{Exists: true, Reason: "unreadable"}
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return File{Exists: true, Reason: "unreadable"}
	}
	if fi.IsDir() || !fi.Mode().IsRegular() {
		return File{Exists: true, Reason: "parse"}
	}
	data, err := io.ReadAll(io.LimitReader(fh, MaxBytes+1))
	if err != nil {
		return File{Exists: true, Reason: "unreadable"}
	}
	out := File{Exists: true, Hash: hashBytes(data)}
	if len(data) > MaxBytes {
		out.Reason = "parse"
		return out
	}
	if !f.SkipOwner {
		uid, ok := fileUID(fi, f.UID)
		if !ok || uid != 0 {
			out.Reason = "not_root"
			return out
		}
		perm := fi.Mode().Perm()
		if perm&0o020 != 0 {
			out.Reason = "group_writable"
			return out
		}
		if perm&0o002 != 0 {
			out.Reason = "world_writable"
			return out
		}
	}
	values, reason := parseTOML(data, f.Schema)
	if reason != "" {
		out.Reason = reason
		return out
	}
	out.Valid = true
	out.Values = values
	return out
}

// parseData checks bytes the caller already holds: the size cap, the hash
// and the parser, as Load does after its file checks.
func parseData(data []byte, schema Schema) File {
	out := File{Exists: true, Hash: hashBytes(data)}
	if len(data) > MaxBytes {
		out.Reason = "parse"
		return out
	}
	values, reason := parseTOML(data, schema)
	if reason != "" {
		out.Reason = reason
		return out
	}
	out.Valid = true
	out.Values = values
	return out
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fileUID(fi os.FileInfo, override func(os.FileInfo) (uint32, bool)) (uint32, bool) {
	if override != nil {
		return override(fi)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Uid, true
}
