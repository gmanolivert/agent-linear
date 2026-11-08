package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func (r *runner) resume(c *config, id, runs string, db *sql.DB, lock *os.File) (runErr error) {
	resolved, err := r.resolveIssueID(id)
	if err != nil {
		return err
	}
	id = resolved
	// Consume automatic eligibility before validation or any external action.
	if err := stopProviderRecovery(db, id, "Resume interrupted or failed; inspect retained checkout and logs before manual resume/reconcile"); err != nil {
		return err
	}
	saved, err := loadRecovery(c, id, runs, db)
	if err != nil {
		return err
	}
	viewer, _, err := r.viewer()
	if err != nil {
		return err
	}
	current, err := r.issue(id)
	if err != nil {
		return err
	}
	if viewer != saved.Owner || textAt(current, "assignee", "id") != viewer {
		return errors.New("only the original owner, while still assigned, may resume this run; handoff is not supported")
	}
	if textAt(current, "project", "id") != saved.ProjectID || textAt(current, "team", "id") != saved.Route.TeamID || textAt(current, "state", "id") != saved.ProgressState {
		return errors.New("Linear route, status or requirements changed; inspect before resuming")
	}
	// An amended contract is refused unless the operator accepts it explicitly; acceptance
	// re-freezes the run on the current title, description and relations.
	accepted := contractChanges(saved.Issue, current)
	if len(accepted) != 0 {
		if !r.acceptChanges {
			return errors.New("Linear route, status or requirements changed; inspect before resuming")
		}
		if err := acceptContractChanges(db, id, &saved, current, accepted); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "%s: accepted contract changes to %s\n", textAt(current, "identifier"), strings.Join(accepted, ", "))
	}
	if exists(filepath.Join(saved.Scratch, "finish-state-request.json")) || exists(filepath.Join(saved.Scratch, "finish-state.json")) {
		return errors.New("completion was already requested or confirmed; inspect and reconcile instead of resuming")
	}
	session := sessionID(saved.Scratch, saved.Engine)
	if session == "" {
		return errors.New("no recorded session ID; refusing to start a replacement session")
	}
	branch, err := r.command(saved.Checkout, "git", "branch", "--show-current")
	if err != nil {
		return err
	}
	remote, err := r.command(saved.Checkout, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	origin, err := githubName(remote)
	if err != nil {
		return err
	}
	if branch != saved.Branch || origin != strings.ToLower(saved.Route.GitHubRepo) {
		return errors.New("recorded checkout branch or origin changed")
	}
	profile, err := r.gitProfile(saved.Checkout)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(profile, saved.Profile) {
		return errors.New("recorded Git signing profile changed")
	}
	if saved.Engine != "codex" && saved.Engine != "opencode" || saved.Executable == "" {
		return errors.New("invalid recorded agent")
	}
	resumed := recordedProvider(c, saved)
	parent := filepath.Join(saved.Scratch, "resumes")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	archive, err := os.MkdirTemp(parent, "attempt-")
	if err != nil {
		return err
	}
	for _, name := range []string{"events.jsonl", "stderr.log", "result.json", "failure.txt", "provider-limit.json", "AGENT.md"} {
		raw, e := os.ReadFile(filepath.Join(saved.Scratch, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if e = writeFile(filepath.Join(archive, name), raw); e != nil {
			return e
		}
	}
	if err = writeFile(filepath.Join(saved.Scratch, "AGENT.md"), []byte(asset("AGENT.md"))); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(saved.Scratch, "resume-session.json"), map[string]any{"engine": saved.Engine, "session_id": session}); err != nil {
		return err
	}
	// A failed continuation must never leave an old report eligible for reconciliation.
	if err = os.Remove(filepath.Join(saved.Scratch, "result.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err = db.Exec("UPDATE runs SET status='running' WHERE issue_id=?", id); err != nil {
		return err
	}
	defer func() {
		if runErr != nil {
			status := "failed"
			var limit *providerLimit
			if errors.As(runErr, &limit) {
				status = "blocked"
				if e := errors.Join(saveCooldown(db, &resumed, limit), writeJSON(filepath.Join(saved.Scratch, "provider-limit.json"), map[string]any{"status": status, "engine": saved.Engine, "retry_at": limit.RetryAt, "recovery": "Cron will check availability and automatically resume the recorded session, subject to recovery checks."})); e != nil {
					limit.recordingFailed = true
					runErr = errors.Join(runErr, e)
				}
			}
			_, e := db.Exec("UPDATE runs SET status=? WHERE issue_id=?", status, id)
			e = errors.Join(e, recordActivity(saved.Scratch, status), writeFile(filepath.Join(saved.Scratch, "failure.txt"), []byte("Managed resume failed; inspect stderr.log, events.jsonl and retained resume attempt.\n")))
			if e != nil {
				if limit != nil {
					limit.recordingFailed = true
				}
				runErr = errors.Join(runErr, e)
			}
			if limit != nil && !limit.recordingFailed {
				if e = queueProviderRecovery(db, id, limit); e != nil {
					limit.recordingFailed = true
					runErr = errors.Join(runErr, e)
				}
			}
		}
	}()
	if err = recordActivity(saved.Scratch, "resuming"); err != nil {
		return err
	}
	context, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Continue only this recorded issue and session in its existing checkout. Inspect partial work and the existing remote branch/PR before changing anything; do not repeat completed external actions. Read the current runner instructions at %s and the retained worklog. These instructions supersede prior runner policy; reassess workflow-only blockers without changing the original issue requirements. Trusted launch context: %s\nPreserve every original requirement and Git signing profile. Return only JSON matching this schema:\n%s\n", filepath.Join(saved.Scratch, "AGENT.md"), context, asset("result.schema.json"))
	if len(accepted) != 0 {
		prompt += " The Linear contract (" + strings.Join(accepted, ", ") + ") was amended after launch and the amendment is accepted: re-read issue.json and requirements.json in the scratch folder and satisfy the amended Requirements and Acceptance criteria; accepted-changes.json records what changed."
	}
	if err = writeFile(filepath.Join(archive, "prompt.txt"), []byte(prompt)); err != nil {
		return err
	}
	lessons, err := priorLessons(db, saved.ProjectID)
	if err != nil {
		return err
	}
	report, err := r.launchAgent(&resumed, saved.Checkout, saved.Scratch, prompt, agentEnvironment(saved.Profile), lessons, lock, session)
	if err != nil {
		if e := os.Remove(filepath.Join(saved.Scratch, "result.json")); e != nil && !os.IsNotExist(e) {
			return errors.Join(err, e)
		}
		return err
	}
	if err = r.finishRun(db, saved, report, c.GH); err != nil {
		return err
	}
	body := fmt.Sprintf("Recorded session resumed with status %s. View pull request at %s.", textAt(report, "status"), textAt(report, "pr_url"))
	if textAt(report, "status") == "ready" && noCodeCompletion(report) {
		body = "Recorded session resumed with status ready. No code changes; PR not required."
	}
	if err = r.postComment(id, body, archive, "resume"); err != nil {
		fmt.Fprintln(r.errOut, "Resume outcome preserved; inspect resume comment delivery evidence")
	}
	fmt.Fprintf(r.out, "%s: resumed %s %s\n", textAt(saved.Issue, "identifier"), textAt(report, "status"), textAt(report, "pr_url"))
	return nil
}
