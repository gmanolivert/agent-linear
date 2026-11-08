package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestContractChanges(t *testing.T) {
	saved := map[string]any{"title": "Fix it", "description": "## Requirements\n\nR-a", "relations": map[string]any{"nodes": []any{}}}
	if got := contractChanges(saved, map[string]any{"title": "Fix it", "description": "## Requirements\n\nR-a", "relations": map[string]any{"nodes": []any{}}}); len(got) != 0 {
		t.Fatalf("unchanged issue reported %v", got)
	}
	current := map[string]any{"title": "Fix it now", "description": "## Requirements\n\nR-a\n\nR-b", "relations": map[string]any{"nodes": []any{map[string]any{"type": "blocks"}}}}
	got := contractChanges(saved, current)
	if len(got) != 3 || got[0] != "title" || got[1] != "description" || got[2] != "relations" {
		t.Fatalf("contractChanges = %v", got)
	}
}

func TestAcceptContractChangesRefreezesTheRun(t *testing.T) {
	root := t.TempDir()
	db, err := openState(filepath.Join(root, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scratch := filepath.Join(root, "run")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	before := map[string]any{"id": "issue-1", "title": "Fix it", "description": "## Requirements\n\nR-a"}
	saved := runContext{Scratch: scratch, Issue: before, Requirements: issueRequirements(before)}
	if err := saveRunContext(db, "issue-1", saved); err != nil {
		t.Fatal(err)
	}
	current := map[string]any{"id": "issue-1", "title": "Fix it", "description": "## Requirements\n\nR-a\n\nR-b (amended)"}
	if err := acceptContractChanges(db, "issue-1", &saved, current, []string{"description"}); err != nil {
		t.Fatal(err)
	}
	if len(saved.Requirements) != 4 || saved.Requirements[3].Requirement != "R-b (amended)" {
		t.Fatalf("requirements not re-frozen: %+v", saved.Requirements)
	}
	if textAt(saved.Issue, "description") != textAt(current, "description") {
		t.Fatal("recorded issue not replaced")
	}
	for _, name := range []string{"issue-before-accept.json", "issue.json", "requirements.json", "accepted-changes.json"} {
		if _, err := os.Stat(filepath.Join(scratch, name)); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	var record map[string]any
	raw, _ := os.ReadFile(filepath.Join(scratch, "accepted-changes.json"))
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["requirements_before"].(float64) != 3 || record["requirements_after"].(float64) != 4 {
		t.Fatalf("change record = %v", record)
	}
	var stored string
	if err := db.QueryRow("SELECT context FROM run_context WHERE issue_id=?", "issue-1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var reloaded runContext
	if err := json.Unmarshal([]byte(stored), &reloaded); err != nil {
		t.Fatal(err)
	}
	if textAt(reloaded.Issue, "description") != textAt(current, "description") || len(reloaded.Requirements) != 4 {
		t.Fatal("stored recovery context not updated")
	}
	var _ *sql.DB = db
}
