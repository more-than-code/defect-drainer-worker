package exec

import (
	"bytes"
	"encoding/json"
)

// Deprecated key aliases (worker protocol v1 §18, project_keys.v1). A worker
// before 0.2.0-rc7 knows a project as an "app": the spec keys appId and app,
// and app_id on Defect, Batch and TestRun. This worker sends only the new
// keys, and reads the old ones until 1.0 when the new ones are absent. A
// value under both keys that differs is a KeyConflictError, which the worker
// declines bad_spec. After decoding, only the new fields are used.

// KeyConflictError is a key and its deprecated alias sent with different
// values.
type KeyConflictError struct {
	Key   string
	Alias string
}

func (e *KeyConflictError) Error() string {
	return "conflicting keys: " + e.Key + " and " + e.Alias + " differ"
}

// UnmarshalJSON reads projectId and project, or their aliases appId and app.
func (s *JobSpec) UnmarshalJSON(b []byte) error {
	type plain JobSpec
	var v struct {
		plain
		OldProjectID string `json:"appId"`
		// RawProject shadows plain.Project (a shallower field wins), so an
		// absent "project" is told apart from a zero one.
		RawProject json.RawMessage `json:"project"`
		OldProject json.RawMessage `json:"app"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	id, err := aliasID("projectId", v.ProjectID, "appId", v.OldProjectID)
	if err != nil {
		return err
	}
	settings, err := aliasSettings(v.RawProject, v.OldProject)
	if err != nil {
		return err
	}
	*s = JobSpec(v.plain)
	s.ProjectID, s.Project = id, settings
	return nil
}

// UnmarshalJSON reads project_id, or its alias app_id.
func (d *Defect) UnmarshalJSON(b []byte) error {
	type plain Defect
	var v struct {
		plain
		OldProjectID string `json:"app_id"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	id, err := aliasID("project_id", v.ProjectID, "app_id", v.OldProjectID)
	if err != nil {
		return err
	}
	*d = Defect(v.plain)
	d.ProjectID = id
	return nil
}

// UnmarshalJSON reads project_id, or its alias app_id.
func (bt *Batch) UnmarshalJSON(b []byte) error {
	type plain Batch
	var v struct {
		plain
		OldProjectID string `json:"app_id"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	id, err := aliasID("project_id", v.ProjectID, "app_id", v.OldProjectID)
	if err != nil {
		return err
	}
	*bt = Batch(v.plain)
	bt.ProjectID = id
	return nil
}

// UnmarshalJSON reads project_id, or its alias app_id.
func (r *TestRun) UnmarshalJSON(b []byte) error {
	type plain TestRun
	var v struct {
		plain
		OldProjectID string `json:"app_id"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	id, err := aliasID("project_id", v.ProjectID, "app_id", v.OldProjectID)
	if err != nil {
		return err
	}
	*r = TestRun(v.plain)
	r.ProjectID = id
	return nil
}

// aliasID picks an id sent under key or its alias. An empty value counts as
// absent: ids are never empty.
func aliasID(key, val, alias, old string) (string, error) {
	switch {
	case old == "" || old == val:
		return val, nil
	case val == "":
		return old, nil
	default:
		return "", &KeyConflictError{Key: key, Alias: alias}
	}
}

// aliasSettings picks the settings sent as "project" or as "app". Both sent
// must encode the same.
func aliasSettings(raw, old json.RawMessage) (ProjectSettings, error) {
	var out ProjectSettings
	if present(raw) {
		if err := json.Unmarshal(raw, &out); err != nil {
			return out, err
		}
	}
	if !present(old) {
		return out, nil
	}
	var prev ProjectSettings
	if err := json.Unmarshal(old, &prev); err != nil {
		return out, err
	}
	if !present(raw) {
		return prev, nil
	}
	a, errA := json.Marshal(out)
	c, errC := json.Marshal(prev)
	if errA != nil || errC != nil || !bytes.Equal(a, c) {
		return out, &KeyConflictError{Key: "project", Alias: "app"}
	}
	return out, nil
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}
