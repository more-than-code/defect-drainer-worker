package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/more-than-code/defect-drainer-worker/exec"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/config"
	"github.com/more-than-code/defect-drainer-worker/internal/botagent/redact"
	"github.com/more-than-code/defect-drainer-worker/workerproto"
)

// prepare turns an exec event into its remote shape (§6.2, §7, §8.4):
//   - harvested images, and notes over 64 KiB, become blobs, uploaded
//     before the event that names them, with Data and the note text removed;
//   - every string is redacted against the host's secret values;
//   - any string over 256 KiB is truncated, with a log line naming it, and
//     the event is kept at or under 768 KiB.
func (j *job) prepare(ctx context.Context, e exec.Event) exec.Event {
	r := j.w.currentRedactor()
	var notes []string
	if e.Harvest != nil {
		h := *e.Harvest
		h.Defects = append([]exec.HarvestedDefect(nil), h.Defects...)
		for i := range h.Defects {
			d := &h.Defects[i]
			d.Images = j.blobImages(ctx, d.Images, &notes)
			if d.Note != "" {
				d.Note = r.Values(d.Note)
				if len(d.Note) > workerproto.NoteBlobAbove {
					data := []byte(d.Note)
					sha := hashHex(data)
					if err := j.uploadBlob(ctx, sha, "text/plain", data); err == nil {
						d.Note, d.NoteSHA256, d.NoteSize = "", sha, int64(len(data))
					} else {
						notes = append(notes, fmt.Sprintf("note of %s kept inline: upload failed (%v)", d.DefectID, err))
					}
				}
			}
		}
		e.Harvest = &h
	}
	if e.TestResults != nil {
		t := *e.TestResults
		t.Steps = append([]exec.TestStepResult(nil), t.Steps...)
		for i := range t.Steps {
			t.Steps[i].Images = j.blobImages(ctx, t.Steps[i].Images, &notes)
		}
		e.TestResults = &t
	}
	if e.Message != nil && len(e.Message.Images) > 0 {
		m := *e.Message
		var files []exec.HarvestFile
		for i, img := range m.Images {
			name := img.Filename
			if !workerproto.ValidRelPath(name) || strings.Contains(name, "/") {
				name = fmt.Sprintf("image-%d.png", i+1)
			}
			files = append(files, exec.HarvestFile{Filename: name, Data: img.Data, SHA256: img.SHA256, Size: img.Size})
		}
		m.Images = nil
		for _, f := range j.blobImages(ctx, files, &notes) {
			m.Images = append(m.Images, exec.MessageImage{Filename: f.Filename, MIME: exec.ImageMIME(f.Filename), SHA256: f.SHA256, Size: f.Size})
		}
		e.Message = &m
	}
	e, truncated := capEvent(e, r)
	for _, p := range truncated {
		notes = append(notes, "field truncated: "+p)
	}
	for _, n := range notes {
		e.Lines = append(e.Lines, exec.LogLine{Text: n, Level: "warn", Source: "dd-worker"})
	}
	return e
}

func (w *Worker) currentRedactor() redact.Exact {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.redactor
}

// blobImages uploads each image and returns it by reference. An image the
// server would not accept (type, size) or that cannot be uploaded is left
// out with a note.
func (j *job) blobImages(ctx context.Context, imgs []exec.HarvestFile, notes *[]string) []exec.HarvestFile {
	var out []exec.HarvestFile
	for _, f := range imgs {
		if !workerproto.ValidRelPath(f.Filename) || strings.Contains(f.Filename, "/") {
			*notes = append(*notes, fmt.Sprintf("image %q skipped: name must be letters, digits, '.', '_' or '-'", f.Filename))
			continue
		}
		if f.Data == nil && f.SHA256 != "" {
			out = append(out, f)
			continue
		}
		ct := blobType(f.Data)
		if ct == "" {
			*notes = append(*notes, fmt.Sprintf("image %s skipped: not png, jpeg, webp or gif", f.Filename))
			continue
		}
		sha := hashHex(f.Data)
		if err := j.uploadBlob(ctx, sha, ct, f.Data); err != nil {
			*notes = append(*notes, fmt.Sprintf("image %s skipped: upload failed (%v)", f.Filename, err))
			continue
		}
		out = append(out, exec.HarvestFile{Filename: f.Filename, SHA256: sha, Size: int64(len(f.Data))})
	}
	return out
}

// uploadBlob keeps the bytes in the job's blob dir (for a blob_missing
// resend) and PUTs them with the newest job token, retrying while the job
// is active.
func (j *job) uploadBlob(ctx context.Context, sha, contentType string, data []byte) error {
	dir := j.w.opts.Paths.Blobs(j.id)
	if err := config.WriteFileAtomic(filepath.Join(dir, sha), data, 0o600, 0o700); err != nil {
		return err
	}
	_ = config.WriteFileAtomic(filepath.Join(dir, sha+".type"), []byte(contentType), 0o600, 0o700)
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		j.mu.Lock()
		tok := j.token
		j.mu.Unlock()
		if err = j.w.putBlob(ctx, j.id, sha, contentType, tok, data); err == nil {
			return nil
		}
		if _, status := err.(httpStatusError); status {
			return err // the server refused it; a retry will not change that
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return err
}

// reuploadBlob is the blob_missing answer: PUT the stored bytes again.
func (j *job) reuploadBlob(ctx context.Context, sha string) error {
	if !workerproto.ValidSHA256(sha) {
		return fmt.Errorf("invalid sha256")
	}
	dir := j.w.opts.Paths.Blobs(j.id)
	data, err := os.ReadFile(filepath.Join(dir, sha))
	if err != nil {
		return err
	}
	ct, _ := os.ReadFile(filepath.Join(dir, sha+".type"))
	j.mu.Lock()
	tok := j.token
	j.mu.Unlock()
	return j.w.putBlob(ctx, j.id, sha, string(ct), tok, data)
}

// capEvent redacts every string and enforces the size limits. It returns
// the paths of truncated fields.
func capEvent(e exec.Event, r redact.Exact) (exec.Event, []string) {
	raw, err := json.Marshal(e)
	if err != nil {
		return e, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return e, nil
	}
	var truncated []string
	limit := workerproto.MaxStringFieldBytes
	for {
		tree = walkStrings(tree, "", func(path, s string) string {
			s = r.Values(s)
			if len(s) > limit {
				truncated = append(truncated, path)
				return cutUTF8(s, limit)
			}
			return s
		})
		out, err := json.Marshal(tree)
		if err != nil {
			return e, nil
		}
		if len(out) <= workerproto.MaxEventBytes-4096 || limit <= 1024 {
			var back exec.Event
			if err := json.Unmarshal(out, &back); err != nil {
				return e, nil
			}
			return back, dedupe(truncated)
		}
		limit /= 4
	}
}

func walkStrings(v any, path string, fn func(path, s string) string) any {
	switch t := v.(type) {
	case string:
		return fn(path, t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			t[k] = walkStrings(t[k], p, fn)
		}
		return t
	case []any:
		for i := range t {
			t[i] = walkStrings(t[i], fmt.Sprintf("%s[%d]", path, i), fn)
		}
		return t
	default:
		return v
	}
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
