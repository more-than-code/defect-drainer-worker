package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/conn"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// EnrollOptions are dd-worker init's inputs.
type EnrollOptions struct {
	Paths       Paths
	Server      string // https origin
	JoinToken   string // dd_wjn_…, read from stdin
	InsecureDev bool
	// MachineIDFile is /etc/machine-id by default. When it is missing, a
	// random id is kept in {state}/machine-id instead.
	MachineIDFile string
	Hostname      string
}

// Enroll is §3: dial with the join token, send enroll, store the credential
// atomically at mode 0600. A refused token gets an error and writes nothing.
func Enroll(ctx context.Context, o EnrollOptions) (config.Config, error) {
	server := strings.TrimRight(o.Server, "/")
	if err := config.ValidateServerURL(server, o.InsecureDev); err != nil {
		return config.Config{}, err
	}
	if !config.JoinTokenOK(o.JoinToken) {
		return config.Config{}, errors.New("join token is not usable")
	}
	if err := CheckCredentialDir(o.Paths); err != nil {
		return config.Config{}, err
	}
	for _, f := range []string{o.Paths.Env(), o.Paths.LegacyEnv()} {
		if _, err := os.Lstat(f); err == nil {
			return config.Config{}, fmt.Errorf("%s exists: this host is enrolled; remove it first to re-enrol", f)
		}
	}
	host := o.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	host, err := config.SanitizeHostname(host)
	if err != nil {
		return config.Config{}, err
	}
	mid, err := machineIDHash(o)
	if err != nil {
		return config.Config{}, err
	}
	u, err := wsURL(server)
	if err != nil {
		return config.Config{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, status, err := conn.DialStatus(dctx, u, o.JoinToken)
	if err != nil {
		if status != 0 {
			return config.Config{}, fmt.Errorf("server refused the join token (%d)", status)
		}
		return config.Config{}, errors.New(conn.Safe(err, o.JoinToken))
	}
	defer c.CloseNow()
	goos, arch := osArch()
	if err := conn.WriteJSON(dctx, c, workerproto.Enroll{
		Header: workerproto.NewHeader(workerproto.TypeEnroll), Hostname: host, MachineIDHash: mid,
		Version: Version, OS: goos, Arch: arch,
	}); err != nil {
		return config.Config{}, errors.New("enroll could not be sent")
	}
	for {
		typ, data, err := conn.ReadFrame(dctx, c)
		if err != nil {
			if code := websocket.CloseStatus(err); code == workerproto.ClosePolicy {
				return config.Config{}, errors.New("join token refused (used, expired or for another org)")
			}
			if typ == "" && data == nil {
				return config.Config{}, errors.New("enrolment ended without a credential")
			}
			continue
		}
		if typ != workerproto.TypeEnrolled {
			continue
		}
		var f workerproto.Enrolled
		if json.Unmarshal(data, &f) != nil {
			return config.Config{}, errors.New("enrolled frame is not valid")
		}
		cfg := config.Config{ServerURL: server, WorkerID: f.WorkerID, OrgID: f.OrgID, Credential: f.Credential, InsecureDev: o.InsecureDev}
		if err := cfg.Validate(); err != nil {
			return config.Config{}, fmt.Errorf("enrolled frame: %v", err)
		}
		if cfg.OrgID == "" {
			return config.Config{}, errors.New("enrolled frame without org_id")
		}
		if err := config.Write(o.Paths.Env(), cfg); err != nil {
			return config.Config{}, err
		}
		c.Close(websocket.StatusNormalClosure, "")
		cfg.Path = o.Paths.Env()
		return cfg, nil
	}
}

// machineIDHash is the SHA-256 of /etc/machine-id, or of a random id kept
// in the state dir where there is none (macOS).
func machineIDHash(o EnrollOptions) (string, error) {
	file := o.MachineIDFile
	if file == "" {
		file = "/etc/machine-id"
	}
	b, err := os.ReadFile(file)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		local := filepath.Join(o.Paths.StateDir, "machine-id")
		b, err = os.ReadFile(local)
		if err != nil {
			var r [16]byte
			if _, err := rand.Read(r[:]); err != nil {
				return "", err
			}
			b = []byte(hex.EncodeToString(r[:]))
			if err := config.WriteFileAtomic(local, b, 0o600, 0o700); err != nil {
				return "", err
			}
		}
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(b))))
	return hex.EncodeToString(sum[:]), nil
}
