package exec

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The old keys (rc6 and before) decode until 1.0; a key and its alias that
// differ are a KeyConflictError naming both (worker protocol v1 §18).
func TestProjectKeyAliases(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		decode    func([]byte) (string, error)
		wantID    string
		wantClash string // "" for none
	}{
		{"spec new", `{"projectId":"p1"}`, decodeSpecID, "p1", ""},
		{"spec old", `{"appId":"p1"}`, decodeSpecID, "p1", ""},
		{"spec both equal", `{"projectId":"p1","appId":"p1"}`, decodeSpecID, "p1", ""},
		{"spec both differ", `{"projectId":"p1","appId":"p2"}`, decodeSpecID, "", "conflicting keys: projectId and appId differ"},
		{"defect new", `{"project_id":"p1"}`, decodeOf[Defect], "p1", ""},
		{"defect old", `{"app_id":"p1"}`, decodeOf[Defect], "p1", ""},
		{"defect both equal", `{"project_id":"p1","app_id":"p1"}`, decodeOf[Defect], "p1", ""},
		{"defect both differ", `{"project_id":"p1","app_id":"p2"}`, decodeOf[Defect], "", "conflicting keys: project_id and app_id differ"},
		{"batch new", `{"project_id":"p1"}`, decodeOf[Batch], "p1", ""},
		{"batch old", `{"app_id":"p1"}`, decodeOf[Batch], "p1", ""},
		{"batch both equal", `{"project_id":"p1","app_id":"p1"}`, decodeOf[Batch], "p1", ""},
		{"batch both differ", `{"project_id":"p1","app_id":"p2"}`, decodeOf[Batch], "", "conflicting keys: project_id and app_id differ"},
		{"test run new", `{"project_id":"p1"}`, decodeOf[TestRun], "p1", ""},
		{"test run old", `{"app_id":"p1"}`, decodeOf[TestRun], "p1", ""},
		{"test run both equal", `{"project_id":"p1","app_id":"p1"}`, decodeOf[TestRun], "p1", ""},
		{"test run both differ", `{"project_id":"p1","app_id":"p2"}`, decodeOf[TestRun], "", "conflicting keys: project_id and app_id differ"},
		// A nested record's conflict fails the whole spec.
		{"spec nested defect differs", `{"projectId":"p1","fix":{"defects":[{"project_id":"p1","app_id":"p2"}]}}`, decodeSpecID, "", "conflicting keys: project_id and app_id differ"},
		{"spec nested test run old", `{"appId":"p1","testRun":{"run":{"app_id":"p1"}}}`, decodeSpecID, "p1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, err := c.decode([]byte(c.json))
			if c.wantClash != "" {
				var conflict *KeyConflictError
				if !errors.As(err, &conflict) || err.Error() != c.wantClash {
					t.Fatalf("err %v, want %q", err, c.wantClash)
				}
				return
			}
			if err != nil || id != c.wantID {
				t.Fatalf("got %q, %v; want %q", id, err, c.wantID)
			}
		})
	}
}

func TestProjectSettingsAlias(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		wantSand  string
		wantClash bool
	}{
		{"new", `{"project":{"sandbox":"workspace"}}`, "workspace", false},
		{"old", `{"app":{"sandbox":"workspace"}}`, "workspace", false},
		{"both equal", `{"project":{"sandbox":"workspace","verifyCommands":[]},"app":{"sandbox":"workspace"}}`, "workspace", false},
		{"both differ", `{"project":{"sandbox":"workspace"},"app":{"sandbox":"strict"}}`, "", true},
		{"zero project and old", `{"project":{},"app":{"sandbox":"strict"}}`, "", true},
		{"null project, old", `{"project":null,"app":{"sandbox":"strict"}}`, "strict", false},
		{"neither", `{}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s JobSpec
			err := json.Unmarshal([]byte(c.json), &s)
			if c.wantClash {
				var conflict *KeyConflictError
				if !errors.As(err, &conflict) || conflict.Key != "project" || conflict.Alias != "app" {
					t.Fatalf("err %v, want a project/app conflict", err)
				}
				return
			}
			if err != nil || s.Project.Sandbox != c.wantSand {
				t.Fatalf("sandbox %q, %v; want %q", s.Project.Sandbox, err, c.wantSand)
			}
		})
	}
}

// The worker's own encoding carries only the new keys.
func TestProjectKeysEncodeNewOnly(t *testing.T) {
	spec := JobSpec{ProjectID: "p1", Project: ProjectSettings{Sandbox: "strict"},
		Fix:     &FixSpec{Batch: Batch{ProjectID: "p1"}, Defects: []Defect{{ProjectID: "p1"}}},
		TestRun: &TestRunSpec{Run: TestRun{ProjectID: "p1"}}}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, old := range []string{`"appId"`, `"app"`, `"app_id"`} {
		if strings.Contains(got, old) {
			t.Errorf("encoding has %s: %s", old, got)
		}
	}
	for _, key := range []string{`"projectId":"p1"`, `"project":{`, `"project_id":"p1"`} {
		if !strings.Contains(got, key) {
			t.Errorf("encoding lacks %s: %s", key, got)
		}
	}
}

func decodeSpecID(b []byte) (string, error) {
	var s JobSpec
	err := json.Unmarshal(b, &s)
	return s.ProjectID, err
}

func decodeOf[T Defect | Batch | TestRun](b []byte) (string, error) {
	var v T
	err := json.Unmarshal(b, &v)
	switch r := any(v).(type) {
	case Defect:
		return r.ProjectID, err
	case Batch:
		return r.ProjectID, err
	case TestRun:
		return r.ProjectID, err
	}
	return "", err
}

// A test job's brief names the project.
func TestTestBriefSaysProject(t *testing.T) {
	got := BuildTestBrief(TestBriefInput{TestRunID: "TRUN-1", ProjectID: "p1", Run: TestRun{ID: "TRUN-1", Title: "login"}})
	if !strings.Contains(got, "\n- Project: p1\n") || strings.Contains(got, "- App:") {
		t.Fatalf("test brief:\n%s", got)
	}
}
