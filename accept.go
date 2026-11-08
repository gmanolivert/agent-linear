package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"time"
)

// contractChanges names the contract fields that differ between the recorded issue and the
// current one: title, description and relations. Project, team and workflow state are never
// accepted; a run that moved is still refused.
func contractChanges(saved, current map[string]any) []string {
	var changed []string
	if textAt(current, "title") != textAt(saved, "title") {
		changed = append(changed, "title")
	}
	if textAt(current, "description") != textAt(saved, "description") {
		changed = append(changed, "description")
	}
	if !reflect.DeepEqual(current["relations"], saved["relations"]) {
		changed = append(changed, "relations")
	}
	return changed
}

// acceptContractChanges re-freezes a recorded run on the amended issue. The recorded issue and
// the requirement checklist are replaced by the current ones, the previous issue stays in the
// run folder next to a record of what changed, and the stored recovery context is updated so
// reconciliation and later resumes agree with the amended contract.
func acceptContractChanges(db *sql.DB, id string, saved *runContext, current map[string]any, changed []string) error {
	digest := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	before := saved.Issue
	record := map[string]any{
		"accepted_at":               time.Now().UTC().Format(time.RFC3339),
		"changed":                   changed,
		"title_before":              textAt(before, "title"),
		"title_after":               textAt(current, "title"),
		"description_sha256_before": digest(textAt(before, "description")),
		"description_sha256_after":  digest(textAt(current, "description")),
		"requirements_before":       len(saved.Requirements),
	}
	saved.Issue = current
	saved.Requirements = issueRequirements(current)
	record["requirements_after"] = len(saved.Requirements)
	for name, value := range map[string]any{
		"issue-before-accept.json": before,
		"issue.json":               current,
		"requirements.json":        saved.Requirements,
		"accepted-changes.json":    record,
		"context.json":             saved,
	} {
		if err := writeJSON(filepath.Join(saved.Scratch, name), value); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	_, err = db.Exec("UPDATE run_context SET context=? WHERE issue_id=?", string(raw), id)
	return err
}
