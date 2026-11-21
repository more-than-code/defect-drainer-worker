package worker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"

	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// postgresBinaries are what dd-stack runs from policy agent_postgres (S2).
var postgresBinaries = []string{"initdb", "pg_ctl", "postgres"}

// postgresDirReason checks policy agent_postgres: "" when it is a directory
// owned by root that only root can write, holding initdb, pg_ctl and
// postgres, each passing browserFileReason; ReasonMissing when the
// directory or a binary does not exist; ReasonUnsafe otherwise. uid
// overrides the owner for tests, as for policy.toml.
func postgresDirReason(dir string, uid func(os.FileInfo) (uint32, bool)) string {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return workerproto.ReasonMissing
	}
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
		return workerproto.ReasonUnsafe
	}
	if owner, ok := ownerUID(fi, uid); !ok || owner != 0 {
		return workerproto.ReasonUnsafe
	}
	missing := false
	for _, b := range postgresBinaries {
		switch browserFileReason(filepath.Join(dir, b), uid) {
		case "":
		case workerproto.ReasonMissing:
			missing = true
		default:
			return workerproto.ReasonUnsafe
		}
	}
	if missing {
		return workerproto.ReasonMissing
	}
	return ""
}

// checkPostgres is the "postgres" tool's readiness: the directory check,
// then `<dir>/postgres --version` (stdin closed, from /, a timeout). Linux
// only, like the browser: dd-stack runs in a browser job's sandbox, which
// was proved with bubblewrap (probes P4, P5).
func checkPostgres(ctx context.Context, dir string, uid func(os.FileInfo) (uint32, bool)) workerproto.ToolStatus {
	st := workerproto.ToolStatus{Name: workerproto.ToolPostgres}
	if runtime.GOOS != "linux" {
		st.Reason = workerproto.ReasonUnsupportedOS
		return st
	}
	if r := postgresDirReason(dir, uid); r != "" {
		st.Reason = r
		return st
	}
	out, err := runProbe(ctx, osexec.Command(filepath.Join(dir, "postgres"), "--version"))
	st.Version = versionRe.FindString(out)
	if err != nil || st.Version == "" {
		st.Version, st.Reason = "", workerproto.ReasonMissing
		return st
	}
	st.Ready = true
	return st
}
