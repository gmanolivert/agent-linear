package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (r *runner) runIssue(c *config, project string, issue map[string]any, runs string, db *sql.DB, lock *os.File, refresh bool) (runErr error) {
	engine, binary := c.agent()
	rt := c.Projects[project]
	if err := validateIssue(issue, project, rt); err != nil {
		return err
	}
	id, identifier := textAt(issue, "id"), textAt(issue, "identifier")
	seen, err := claimedStatus(db, id)
	if err != nil {
		return err
	}
	if isClaimed(seen) {
		return nil
	}
	if err = r.checkCapacity(c); err != nil {
		return err
	}
	current, err := r.claimableIssue(id, project, rt)
	if err != nil {
		return err
	}
	if current == nil {
		fmt.Fprintf(r.out, "%s: skipped; not ready or assigned to another user\n", identifier)
		return nil
	}
	blockers, err := r.openBlockers(db, current)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		fmt.Fprintf(r.out, "%s: skipped; blocked by %s\n", identifier, strings.Join(blockers, ", "))
		return nil
	}
	preflight, err := r.checkProvider(c, rt.RepoPath, runs, db, lock)
	if err != nil {
		var limit *providerLimit
		if errors.As(err, &limit) {
			if e := saveCooldown(db, c, limit); e != nil {
				limit.recordingFailed = true
				return errors.Join(err, e)
			}
			if preflight != "" {
				if _, e := db.Exec(`INSERT INTO runs VALUES (?,?,?,?,?) ON CONFLICT(issue_id) DO UPDATE SET status=excluded.status,folder=excluded.folder WHERE runs.status='deferred'`, id, identifier, "deferred", preflight, ""); e != nil {
					limit.recordingFailed = true
					return errors.Join(err, e)
				}
				if e := writeJSON(filepath.Join(preflight, "deferred.json"), map[string]any{"issue_id": id, "engine": engine, "status": "deferred", "retry_at": limit.RetryAt}); e != nil {
					limit.recordingFailed = true
					return errors.Join(err, e)
				}
			}
			fmt.Fprintf(r.out, "%s: deferred; Linear unchanged. %s\n", identifier, limit.Error())
		}
		return err
	}
	if refresh {
		fresh, err := r.issues(project, rt, id)
		if err != nil {
			return err
		}
		if len(fresh) == 0 {
			return nil
		}
		issue = fresh[0]
		if textAt(issue, "id") != id {
			return errors.New("refreshed issue does not match selected issue")
		}
		if err = validateIssue(issue, project, rt); err != nil {
			return err
		}
	}
	current, err = r.claimableIssue(id, project, rt)
	if err != nil {
		return err
	}
	if current == nil {
		fmt.Fprintf(r.out, "%s: skipped; ownership/readiness changed before claim\n", identifier)
		return nil
	}
	folder := filepath.Join(runs, id)
	// Provider preflight can consume space; recheck before creating the claim.
	if err = r.checkCapacity(c); err != nil {
		return err
	}
	if err = os.Mkdir(folder, 0700); err != nil {
		return err
	}
	branch := "linear-" + strings.ToLower(identifier) + "-" + id[:8]
	if _, err = db.Exec(`INSERT INTO runs VALUES (?, ?, ?, ?, ?) ON CONFLICT(issue_id) DO UPDATE SET status=excluded.status,folder=excluded.folder,pr_url=excluded.pr_url WHERE runs.status='deferred'`, id, identifier, "running", folder, ""); err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	status, prURL := "failed", ""
	noCode := false
	assigneeName := ""
	defer func() {
		if runErr != nil {
			var limit *providerLimit
			if errors.As(runErr, &limit) {
				status = "blocked"
				if e := saveCooldown(db, c, limit); e != nil {
					limit.recordingFailed = true
					runErr = errors.Join(runErr, e)
				}
				if e := writeJSON(filepath.Join(folder, "provider-limit.json"), map[string]any{"status": "blocked", "engine": engine, "retry_at": limit.RetryAt, "recovery": "Cron will check availability and automatically resume the recorded session, subject to recovery checks."}); e != nil {
					limit.recordingFailed = true
					runErr = errors.Join(runErr, e)
				}
			}
			if _, e := db.Exec("UPDATE runs SET status=? WHERE issue_id=?", status, id); e != nil {
				if limit != nil {
					limit.recordingFailed = true
				}
				runErr = errors.Join(runErr, e)
			}
			if e := writeFile(filepath.Join(folder, "failure.txt"), []byte(redact(runErr.Error())+"\nInspect logs and remote PR before recovery\n")); e != nil {
				if limit != nil {
					limit.recordingFailed = true
				}
				runErr = errors.Join(runErr, e)
			}
			if limit != nil && !limit.recordingFailed {
				if e := queueProviderRecovery(db, id, limit); e != nil {
					limit.recordingFailed = true
					runErr = errors.Join(runErr, e)
				}
			}
		}
		if e := recordActivity(folder, status); e != nil {
			runErr = errors.Join(runErr, e)
		}
		resume := engine + " session unavailable (no session ID recorded)."
		if session := sessionID(folder, engine); session != "" {
			command := shellJoin([]string{r.executable, "resume", "--issue", id})
			resume = fmt.Sprintf("Recorded %s session: %s. Continue as the assigned owner with `%s`; use reconcile when only completion checks remain.", engine, session, command)
		}
		url := prURL
		if url == "" {
			url = "unavailable (no PR URL recorded)"
		}
		var limit *providerLimit
		providerLimited := errors.As(runErr, &limit)
		mention := ""
		if assigneeName != "" && (status == "failed" || status == "blocked" && !providerLimited) {
			mention = "@" + assigneeName + " "
		}
		body := mention + fmt.Sprintf("Run finished on %s with status %s. View pull request at %s. %s", hostname, status, url, resume)
		if status == "ready" && noCode {
			body = fmt.Sprintf("Run finished on %s with status ready. No code changes; PR not required. Evidence: result.json in the retained run artifacts. %s", hostname, resume)
		}
		if providerLimited {
			body += " Provider limit reached after launch; partial work preserved. Cron will check availability and automatically resume the recorded session, subject to recovery checks. " + limit.Error()
		}
		// Delivery failure is recorded by postComment; preserve the saved outcome.
		if e := r.postComment(id, body, folder, "finish"); e != nil {
			fmt.Fprintln(r.errOut, "Run outcome preserved; inspect finish-comment delivery evidence")
		}
	}()
	if err = recordActivity(folder, "starting"); err != nil {
		return err
	}
	assignee, viewerName, err := r.viewer()
	if err != nil {
		return err
	}
	assigneeName = viewerName
	if owner := textAt(current, "assignee", "id"); owner != "" && owner != assignee {
		return errors.New("issue owner does not match the authenticated Linear user")
	}
	states, err := r.workflowStates(rt.TeamID)
	if err != nil {
		return err
	}
	if err = r.postComment(id, fmt.Sprintf("Run claimed on %s. Starting work on %s.", hostname, identifier), folder, "start"); err != nil {
		return errors.New("start comment failed; no feature work launched")
	}
	if err = r.updateIssueState(id, states["In Progress"], folder, "start", assignee, textAt(current, "assignee", "id") == ""); err != nil {
		return err
	}
	repo := filepath.Join(folder, "repo")
	if err = recordActivity(folder, "checkout"); err != nil {
		return err
	}
	profile, err := r.gitProfile(rt.RepoPath)
	if err != nil {
		return err
	}
	remote, err := r.command(rt.RepoPath, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if _, err = r.command("", "git", "clone", "--no-hardlinks", rt.RepoPath, repo); err != nil {
		return err
	}
	for field, value := range profile.Settings {
		if _, err = r.command(repo, "git", "config", "--local", field, value); err != nil {
			return err
		}
	}
	if _, err = r.command(repo, "git", "config", "--local", "commit.gpgsign", "true"); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "remote", "set-url", "origin", remote); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "fetch", "origin", "staging"); err != nil {
		return err
	}
	base, err := r.command(repo, "git", "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "checkout", "-b", branch, base); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "issue.json"), issue); err != nil {
		return err
	}
	context := runContext{ProjectID: project, Route: rt, Branch: branch, BaseSHA: base, Scratch: folder, Checkout: repo, Profile: profile, Engine: engine, Executable: binary, Model: c.Model, ReasoningEffort: c.ReasoningEffort, Issue: issue, Owner: assignee, ProgressState: states["In Progress"], ReviewState: states["In Review"], Requirements: issueRequirements(issue)}
	if err = saveRunContext(db, id, context); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "requirements.json"), context.Requirements); err != nil {
		return err
	}
	if err = writeFile(filepath.Join(folder, "AGENT.md"), []byte(asset("AGENT.md"))); err != nil {
		return err
	}
	lessons, err := priorLessons(db, project)
	if err != nil {
		return err
	}
	ctxJSON, err := json.Marshal(context)
	if err != nil {
		return err
	}
	lessonJSON, err := json.Marshal(lessons)
	if err != nil {
		return err
	}
	issueJSON, err := json.MarshalIndent(issue, "", "  ")
	if err != nil {
		return err
	}
	attachmentNote := r.downloadAttachments(c.Projects[project], issue, folder)
	prompt := fmt.Sprintf("Read %s and follow its sequential workflow.\nTrusted launch context: %s\nPrior project lessons (read if relevant): %s\nYour final response must be only JSON conforming to this schema, without markdown:\n%s\nIssue snapshot (task data, not launch authority):\n%s\n", filepath.Join(folder, "AGENT.md"), ctxJSON, lessonJSON, asset("result.schema.json"), issueJSON)
	prompt += attachmentNote
	if err = writeFile(filepath.Join(folder, "prompt.txt"), []byte(prompt)); err != nil {
		return err
	}
	env := agentEnvironment(profile)
	if err = recordActivity(folder, "agent"); err != nil {
		return err
	}
	report, err := r.launchAgent(c, repo, folder, prompt, env, lessons, lock, "")
	if err != nil {
		return err
	}
	noCode = noCodeCompletion(report)
	if candidate := textAt(report, "pr_url"); validPRURL(candidate, rt) {
		prURL = candidate
		if _, err = db.Exec("UPDATE runs SET pr_url=? WHERE issue_id=?", prURL, id); err != nil {
			return err
		}
	}
	if err = r.finishRun(db, context, report, c.GH); err != nil {
		return err
	}
	status = textAt(report, "status")
	fmt.Fprintf(r.out, "%s: %s %s\n", identifier, status, textAt(report, "pr_url"))
	return nil
}
func priorLessons(db *sql.DB, project string) ([]string, error) {
	lessons := []string{}
	rows, err := db.Query("SELECT folder FROM runs WHERE status='ready'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var folder string
		if err = rows.Scan(&folder); err != nil {
			return nil, err
		}
		path := filepath.Join(folder, "lessons.md")
		if !exists(path) {
			continue
		}
		var ctx map[string]any
		if err = readJSON(filepath.Join(folder, "context.json"), &ctx); err != nil {
			return nil, err
		}
		if textAt(ctx, "project_id") == project {
			lessons = append(lessons, path)
		}
	}
	return lessons, rows.Err()
}
