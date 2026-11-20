package workerproto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// N5: an old-shape hello (no tools) still decodes and validates; a hello
// with tools round-trips (rollback in either direction is safe).
func TestHelloToolsOptional(t *testing.T) {
	var old Hello
	raw := `{"v":1,"type":"hello","worker_id":"wrk_1","protocol":1,"max_jobs":1,"adapters":[{"name":"claude","ready":true}]}`
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		t.Fatal(err)
	}
	if err := old.Validate(); err != nil {
		t.Fatalf("hello without tools: %v", err)
	}
	if old.Tools != nil {
		t.Fatalf("tools = %+v, want nil", old.Tools)
	}
	b, _ := json.Marshal(old)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["tools"]; ok {
		t.Fatal("a hello with no tools must not encode a tools key")
	}

	var h Hello
	raw = `{"v":1,"type":"hello","worker_id":"wrk_1","protocol":1,"max_jobs":1,` +
		`"tools":[{"name":"git","version":"2.47.1","ready":true,"reason":""},{"name":"gh","version":"2.62.0","ready":false,"reason":"signed_out"}]}`
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("hello with tools: %v", err)
	}
	want := []ToolStatus{{Name: "git", Version: "2.47.1", Ready: true}, {Name: "gh", Version: "2.62.0", Reason: ReasonSignedOut}}
	if !reflect.DeepEqual(h.Tools, want) {
		t.Fatalf("tools = %+v", h.Tools)
	}
	roundTrip(t, h)
}

func TestValidateTools(t *testing.T) {
	ok := [][]ToolStatus{nil, {}, {{Name: ToolGit}, {Name: ToolGh, Reason: ReasonNoGitHelper}},
		{{Name: ToolGit}, {Name: ToolBrowser, Version: "154.0.8037", Ready: true}}}
	for _, tools := range ok {
		if err := ValidateTools(tools); err != nil {
			t.Errorf("ValidateTools(%+v): %v", tools, err)
		}
	}
	nine := make([]ToolStatus, MaxTools+1)
	for i := range nine {
		nine[i] = ToolStatus{Name: ToolGit}
	}
	bad := [][]ToolStatus{nine, {{Name: "svn"}}, {{Name: ""}}, {{Name: "GIT"}}}
	for _, tools := range bad {
		if ValidateTools(tools) == nil {
			t.Errorf("ValidateTools(%d entries, first %q) = nil", len(tools), tools[0].Name)
		}
	}
	h := Hello{Header: NewHeader(TypeHello), WorkerID: "wrk_1", Protocol: 1, MaxJobs: 1, Tools: []ToolStatus{{Name: "svn"}}}
	if h.Validate() == nil {
		t.Fatal("a hello with an unknown tool name must fail")
	}
}
