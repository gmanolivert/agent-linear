package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

//go:embed AGENT.md result.schema.json goal-plan.schema.json config.example.json
var assets embed.FS

type runner struct {
	acceptChanges    bool
	root, executable string
	in               io.Reader
	out, errOut      io.Writer
	client           *http.Client
	endpoint         string
	command          func(string, ...string) (string, error)
	preflightTimeout time.Duration
	featureTimeout   time.Duration
	ctx              context.Context
}

func newRunner(executable string) *runner {
	r := &runner{root: filepath.Dir(executable), executable: executable, in: os.Stdin,
		out: os.Stdout, errOut: os.Stderr, client: &http.Client{Timeout: 30 * time.Second}, endpoint: "https://api.linear.app/graphql"}
	r.ctx = context.Background()
	r.command = func(dir string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Minute)
		defer cancel()
		return commandContext(ctx, dir, args...)
	}
	r.preflightTimeout = 90 * time.Second
	r.featureTimeout = 2 * time.Hour
	return r
}

func command(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return commandContext(ctx, dir, args...)
}
func commandContext(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := runProcess(ctx, cmd)
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		return out, fmt.Errorf("command %s: %w\n%s", shellJoin(args), err, stderr.String())
	}
	return out, nil
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\r\n'\"\\$`!#&;|<>()*?[]{}~%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, s := range args {
		out[i] = shellQuote(s)
	}
	return strings.Join(out, " ")
}
func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func asset(name string) string {
	b, err := assets.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func exists(path string) bool { _, err := os.Stat(path); return err == nil }
func textAt(v any, keys ...string) string {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}
func boolAt(v any, key string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	b, _ := m[key].(bool)
	return b
}
func redact(s string) string {
	if key := os.Getenv("LINEAR_API_KEY"); key != "" {
		s = strings.ReplaceAll(s, key, "[REDACTED]")
	}
	return s
}
func absPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Resolve symlinks where possible, matching the old launcher's trusted paths.
	if resolved, e := filepath.EvalSymlinks(p); e == nil {
		p = resolved
	}
	return p, nil
}

func (r *runner) cli() *cobra.Command {
	var configPath string
	root := &cobra.Command{Use: "agent-linear", Short: "One Linear issue, one fresh Codex or OpenCode session", SilenceUsage: true, SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true}}
	root.SetIn(r.in)
	root.SetOut(r.out)
	root.SetErr(r.errOut)
	root.PersistentFlags().StringVar(&configPath, "config", filepath.Join(r.root, "config.json"), "Project routing JSON configuration")
	commands := []struct{ name, description string }{
		{"setup", "Collect and validate project routes without scheduling work"},
		{"smoke-test", "Check CLI, repository, signing-profile and Linear read access"},
		{"poll", "Process the eligible Linear queue serially"},
		{"run", "Run one issue by identifier or UUID"},
		{"reset", "Reset a claimed issue to ready and clear its local run state"},
		{"resume", "Continue one recorded session as its assigned owner"},
		{"reconcile", "Verify and finish one recorded run without relaunching work"},
		{"status", "Print health and the five most recently started issues"},
		{"prune-sessions", "Preview session deletion for Done, Canceled or Duplicate issues"},
		{"cron-line", "Preview the five-minute polling entry"},
		{"install-cron", "Install or replace only this workflow's cron entry"},
		{"uninstall-cron", "Remove this workflow's cron entry without stopping active work"},
	}
	for _, spec := range commands {
		name := spec.name
		var dryRun, setup, all, apply, acceptChanges bool
		var issueRef string
		cmd := &cobra.Command{Use: name, Short: spec.description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			path, err := absPath(configPath)
			if err != nil {
				return err
			}
			if cmd.Name() == "prune-sessions" {
				dryRun = !apply
			}
			r.acceptChanges = acceptChanges
			return r.execute(cmd.Name(), path, issueRef, dryRun, setup, all)
		}}
		switch name {
		case "poll", "run", "reset":
			cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Read eligible/claimed issues without starting work")
		case "install-cron":
			cmd.Flags().BoolVar(&setup, "setup", false, "Collect project routes before installing cron")
		case "status":
			cmd.Flags().BoolVar(&all, "all", false, "Show every recorded issue, including deferred work")
		case "prune-sessions":
			cmd.Flags().BoolVar(&apply, "apply", false, "Permanently delete the eligible recorded CLI sessions")
		}
		if name == "run" {
			cmd.Flags().StringVar(&issueRef, "issue", "", "Issue identifier or UUID to fetch and run")
		}
		if name == "reset" {
			cmd.Flags().StringVar(&issueRef, "issue", "", "Issue identifier or UUID to reset")
		}
		if name == "reconcile" || name == "resume" {
			cmd.Flags().StringVar(&issueRef, "issue", "", "Issue identifier or UUID of the recorded run")
		}
		if name == "resume" {
			cmd.Flags().BoolVar(&acceptChanges, "accept-changes", false, "Accept an amended title, description or relations and re-freeze the run on the current contract")
		}
		cmd.PreRun = func(cmd *cobra.Command, args []string) { r.ctx = cmd.Context() }
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{
		Use: "install-skill <repository>", Short: "Install the ad hoc progressive-issue skill in another Git checkout",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return r.installSkill(args[0]) },
	})
	goal := &cobra.Command{Use: "goal", Short: "Turn a prompt into Linear issues via a read-only investigation"}
	var goalProject, goalPrompt, goalPromptFile string
	var goalDryRun, goalAutostart bool
	goal.Flags().StringVar(&goalProject, "project", "", "Configured Linear project UUID")
	goal.Flags().StringVar(&goalPrompt, "prompt", "", "Goal prompt")
	goal.Flags().StringVar(&goalPromptFile, "prompt-file", "", "File containing the goal prompt")
	goal.Flags().BoolVar(&goalDryRun, "dry-run", false, "Investigate and print the plan without creating issues")
	goal.Flags().BoolVar(&goalAutostart, "autostart", false, "Create issues directly in ready + agent-ready instead of triage")
	goal.PreRun = func(cmd *cobra.Command, args []string) { r.ctx = cmd.Context() }
	goal.RunE = func(cmd *cobra.Command, args []string) error {
		path, err := absPath(configPath)
		if err != nil {
			return err
		}
		return r.executeGoal(path, goalProject, goalPrompt, goalPromptFile, goalDryRun, goalAutostart)
	}
	root.AddCommand(goal)
	review := &cobra.Command{Use: "review", Short: "Read feedback on a goal and create follow-ups or refine triage issues"}
	var reviewGoal string
	var reviewDryRun, reviewAll bool
	review.Flags().StringVar(&reviewGoal, "goal", "", "Recorded goal Linear UUID")
	review.Flags().BoolVar(&reviewAll, "all", false, "Review every recorded goal that has new feedback")
	review.Flags().BoolVar(&reviewDryRun, "dry-run", false, "Read feedback and print the revised plan without applying it")
	review.PreRun = func(cmd *cobra.Command, args []string) { r.ctx = cmd.Context() }
	review.RunE = func(cmd *cobra.Command, args []string) error {
		path, err := absPath(configPath)
		if err != nil {
			return err
		}
		if reviewAll {
			return r.executeGoalReviewAll(path, reviewDryRun)
		}
		return r.executeGoalReview(path, reviewGoal, reviewDryRun)
	}
	goal.AddCommand(review)
	return root
}

func (r *runner) execute(action, configPath, issueRef string, dryRun, setup, all bool) (runErr error) {
	if action == "setup" {
		return r.configureProjects(configPath)
	}
	if action == "smoke-test" {
		c, err := r.loadConfig(configPath)
		if err != nil {
			return err
		}
		return r.smokeTest(c)
	}
	runs := filepath.Join(r.root, ".runs")
	if action == "status" {
		return r.readStatus(runs, all)
	}
	if err := os.MkdirAll(runs, 0700); err != nil {
		return err
	}
	if action == "uninstall-cron" {
		return r.uninstallCron(runs)
	}
	monitorPoll := action == "poll" && !dryRun
	if monitorPoll {
		if err := writeJSON(filepath.Join(runs, "poll-attempt.json"), map[string]any{"last_attempt": time.Now().UTC()}); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if action == "reconcile" || action == "resume" || action == "prune-sessions" || action == "reset" {
				return errors.New("worker still active; recovery refused")
			}
			fmt.Fprintln(r.out, "Worker already active; skipping this tick")
			return nil
		}
		return err
	}
	// Do not explicitly unlock: the inherited descriptor must keep orphaned agent work locked.
	pollOutcome, pollPhase := "success", "state initialization"
	if monitorPoll {
		health, err := beginPoll(runs)
		if err != nil {
			return err
		}
		defer func() { runErr = errors.Join(runErr, finishPoll(runs, health, pollOutcome, pollPhase, runErr)) }()
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	if setup {
		if err = r.configureProjects(configPath); err != nil {
			return err
		}
	}
	pollPhase = "configuration"
	c, err := r.loadConfig(configPath)
	if err != nil {
		return err
	}
	if action == "reconcile" {
		return r.reconcile(c, issueRef, runs, db)
	}
	if action == "resume" {
		return r.resume(c, issueRef, runs, db, lock)
	}
	if action == "prune-sessions" {
		return r.pruneSessions(c, runs, db, dryRun)
	}
	if action == "reset" {
		return r.resetIssue(c, issueRef, runs, db, dryRun)
	}
	if action == "poll" && !dryRun {
		pollPhase = "provider recovery"
		handled, deferred, err := r.recoverProvider(c, runs, db, lock)
		if handled || err != nil {
			if deferred {
				pollOutcome = "deferred"
			}
			return err
		}
	}
	pollPhase = "executable preflight"
	_, binary := c.agent()
	for _, exe := range []string{binary, c.GH} {
		if _, err = exec.LookPath(exe); err != nil {
			return fmt.Errorf("required executable unavailable: %s", exe)
		}
	}
	if action == "cron-line" || action == "install-cron" {
		return r.installCron(c, configPath, runs, action == "cron-line")
	}
	if !dryRun {
		pollPhase = "provider cooldown"
		limit, err := activeCooldown(db, c)
		if err != nil {
			return err
		}
		if limit != nil {
			pollOutcome = "deferred"
			fmt.Fprintln(r.out, limit.Error())
			return nil
		}
	}
	var queue []queuedIssue
	pollPhase = "Linear queue read"
	if action == "run" {
		if issueRef == "" {
			return errors.New("run requires --issue with an issue identifier or UUID")
		}
		fetched, err := r.issue(issueRef)
		if err != nil {
			return err
		}
		projectID := textAt(fetched, "project", "id")
		if c.Projects[projectID] == nil {
			return fmt.Errorf("issue %s belongs to project %s, which is not a configured route", textAt(fetched, "identifier"), projectID)
		}
		queue = append(queue, queuedIssue{projectID, fetched})
	} else {
		for _, id := range projectIDs(c) {
			items, e := r.issues(id, c.Projects[id], "")
			if e != nil {
				return e
			}
			for _, issue := range items {
				queue = append(queue, queuedIssue{id, issue})
			}
		}
	}
	for _, item := range queue {
		pollPhase = "issue processing"
		route := c.Projects[item.project]
		issue := item.issue
		if err = validateIssue(issue, item.project, route); err != nil {
			if action == "poll" && errors.Is(err, errIssueBody) {
				fmt.Fprintf(r.errOut, "%s: skipped: %v\n", textAt(issue, "identifier"), err)
				continue
			}
			return err
		}
		seen, e := claimedStatus(db, textAt(issue, "id"))
		if e != nil {
			return e
		}
		if dryRun {
			if seen == "" {
				seen = "eligible"
			}
			fmt.Fprintf(r.out, "%s: %s\n", textAt(issue, "identifier"), seen)
			continue
		}
		if isClaimed(seen) {
			continue
		}
		if action == "poll" {
			fresh, e := r.issues(item.project, route, textAt(issue, "id"))
			if e != nil {
				return e
			}
			if len(fresh) == 0 {
				continue
			}
			issue = fresh[0]
		}
		if err = r.runIssue(c, item.project, issue, runs, db, lock, action == "poll"); err != nil {
			if action == "poll" && errors.Is(err, errIssueBody) {
				fmt.Fprintf(r.errOut, "%s: skipped: %v\n", textAt(issue, "identifier"), err)
				continue
			}
			var limit *providerLimit
			if errors.As(err, &limit) && !limit.recordingFailed {
				pollOutcome = "deferred"
				fmt.Fprintln(r.out, limit.Error())
				return nil
			}
			return err
		}
	}
	if action == "poll" && !dryRun {
		if err = r.reviewAll(c, false, db, lock); err != nil {
			var limit *providerLimit
			if errors.As(err, &limit) && !limit.recordingFailed {
				pollOutcome = "deferred"
				fmt.Fprintln(r.out, limit.Error())
				return nil
			}
			return err
		}
	}
	return nil
}

type queuedIssue struct {
	project string
	issue   map[string]any
}

func main() {
	syscall.Umask(0077)
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = newRunner(executable).cli().ExecuteContext(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, redact(err.Error())+": run failed; inspect local artifacts/configuration")
		os.Exit(1)
	}
}

// A shared reader preserves piped answers across successive setup prompts.
func prompt(reader *bufio.Reader, out io.Writer, label string) (string, error) {
	fmt.Fprint(out, label)
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
