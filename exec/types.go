package exec

import "time"

// Plain mirrors of the inventory records the executor reads. They have the
// same fields, in the same order, as their store counterparts, so the
// orchestrator converts with a type conversion. The executor never imports
// the store (boundary_test.go).

// Defect mirrors store.DefectRecord: the defect text a brief is built from.
type Defect struct {
	ID           string   `json:"id"`
	AppID        string   `json:"app_id"`
	Title        string   `json:"title"`
	Severity     string   `json:"severity"`
	Status       string   `json:"status"`
	Area         string   `json:"area"`
	Client       string   `json:"client"`
	Surface      string   `json:"surface"`
	Repos        []string `json:"repos"`
	Labels       []string `json:"labels"`
	Related      []string `json:"related"`
	Source       string   `json:"source"`
	Reporter     string   `json:"reporter"`
	KeyFiles     []string `json:"key_files"`
	Evidence     []string `json:"evidence"`
	FixEvidence  []string `json:"fix_evidence"`
	Reported     string   `json:"reported"`
	Summary      string   `json:"summary"`
	ResolvedDate string   `json:"resolved_date,omitempty"`
	Resolution   string   `json:"resolution,omitempty"`
	DuplicateOf  string   `json:"duplicate_of,omitempty"`
	Body         string   `json:"body"`
	Bucket       string   `json:"bucket"`
	CreatedAt    string   `json:"created_at"`
	Path         string   `json:"path"`
}

// Batch mirrors jobs.BatchRecord, the batch row a fix brief names.
type Batch struct {
	ID        string   `json:"id"`
	AppID     string   `json:"app_id"`
	Title     string   `json:"title"`
	Goal      string   `json:"goal"`
	Status    string   `json:"status"`
	DefectIDs []string `json:"defect_ids"`
	Mode      string   `json:"mode,omitempty"`
	Created   string   `json:"created"`
	UpdatedAt string   `json:"updated_at"`
	Path      string   `json:"path"`
}

// RepoEntry mirrors store.AppRepoEntry.
type RepoEntry struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	BaseSource string `json:"base_source,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// VerifyCommand mirrors store.VerifyCommand: one operator-authored check.
type VerifyCommand struct {
	Repo    string `json:"repo"`
	Command string `json:"command"`
}

// TestRun is the part of store.TestRun a test job reads.
type TestRun struct {
	ID          string         `json:"id"`
	AppID       string         `json:"app_id"`
	PlanID      *string        `json:"plan_id"`
	PlanVersion *int           `json:"plan_version"`
	Title       string         `json:"title"`
	Env         map[string]any `json:"env"`
	Refs        []RunRef       `json:"refs"`
	Status      string         `json:"status"`
	Steps       []RunStep      `json:"steps,omitempty"`
}

// RunRef mirrors store.RunRef.
type RunRef struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha,omitempty"`
}

// RunStep mirrors store.RunStep.
type RunStep struct {
	Key          string   `json:"key"`
	Position     int      `json:"position"`
	Section      string   `json:"section"`
	Action       string   `json:"action"`
	Expected     string   `json:"expected"`
	Exploratory  bool     `json:"exploratory"`
	Status       string   `json:"status"`
	Actual       string   `json:"actual"`
	Evidence     []string `json:"evidence"`
	DefectID     *string  `json:"defect_id"`
	DefectStatus *string  `json:"defect_status"`
	Review       string   `json:"review"`
	ReviewNote   string   `json:"review_note"`
	UpdatedAt    string   `json:"updated_at"`
}

// nowIso is db.NowIso, which the executor may not import.
func nowIso() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
