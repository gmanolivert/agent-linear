package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// goalIssue records a Linear issue created for a goal.
type goalIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// goalRecord persists one goal and the issues it produced.
type goalRecord struct {
	GoalID         string      `json:"goal_id"`
	ProjectID      string      `json:"project_id"`
	Identifier     string      `json:"identifier"`
	Prompt         string      `json:"prompt"`
	Folder         string      `json:"folder"`
	Status         string      `json:"status"`
	Created        []goalIssue `json:"created_issues"`
	Fingerprint    string      `json:"plan_fingerprint"`
	LastReviewedAt int64       `json:"last_reviewed_at"`
}

func goalFingerprint(project, prompt string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + prompt))
	return hex.EncodeToString(sum[:])
}

func validateGoalPlan(plan map[string]any) error {
	var schema map[string]any
	if err := json.Unmarshal([]byte(asset("goal-plan.schema.json")), &schema); err != nil {
		return err
	}
	if err := validateReport(plan, schema, "goal"); err != nil {
		return err
	}
	for i, item := range planIssues(plan) {
		issue, _ := item.(map[string]any)
		if strings.TrimSpace(textAt(issue, "title")) == "" || strings.TrimSpace(textAt(issue, "description")) == "" {
			return fmt.Errorf("goal plan issue %d needs a title and description", i+1)
		}
	}
	for i, item := range planRefinements(plan) {
		refinement, _ := item.(map[string]any)
		if strings.TrimSpace(textAt(refinement, "issue")) == "" || strings.TrimSpace(textAt(refinement, "title")) == "" && strings.TrimSpace(textAt(refinement, "description")) == "" {
			return fmt.Errorf("goal plan refinement %d needs an issue and a title or description", i+1)
		}
	}
	for i, item := range planFlags(plan) {
		flag, _ := item.(map[string]any)
		if strings.TrimSpace(textAt(flag, "issue")) == "" || strings.TrimSpace(textAt(flag, "reason")) == "" {
			return fmt.Errorf("goal plan flag %d needs an issue and a reason", i+1)
		}
	}
	// Reject dependencies that point forward, at the issue itself, or (for a
	// refinement) at a new follow-up. Goal issues may only depend on issues
	// that already exist, so the dependency graph stays acyclic: an issue must
	// never require something from a future issue, and A requiring B while B
	// requires A is impossible.
	titleIndex := map[string]int{}
	for i, item := range planIssues(plan) {
		if title := textAt(item, "title"); title != "" {
			if _, exists := titleIndex[title]; !exists {
				titleIndex[title] = i
			}
		}
	}
	for i, item := range planIssues(plan) {
		for _, dep := range planDependsOn(item) {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if idx, ok := titleIndex[dep]; ok && idx >= i {
				return fmt.Errorf("goal issue %d must not depend on itself or a later issue (%q)", i+1, dep)
			}
		}
	}
	for i, item := range planRefinements(plan) {
		for _, dep := range planDependsOn(item) {
			if _, ok := titleIndex[strings.TrimSpace(dep)]; ok {
				return fmt.Errorf("goal refinement %d must not depend on a new issue (%q)", i+1, dep)
			}
		}
	}
	return nil
}

func readGoalPlan(folder string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(folder, "result.json"))
	if err != nil {
		return nil, err
	}
	var plan map[string]any
	if err = decodeObject(data, &plan); err != nil {
		return nil, err
	}
	if err = validateGoalPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func planIssues(plan map[string]any) []any {
	issues, _ := plan["issues"].([]any)
	return issues
}

func planRefinements(plan map[string]any) []any {
	refinements, _ := plan["refinements"].([]any)
	return refinements
}

func planFlags(plan map[string]any) []any {
	flags, _ := plan["flags"].([]any)
	return flags
}

func planLabels(issue any) []string {
	var out []string
	labels, _ := issue.(map[string]any)["labels"].([]any)
	for _, label := range labels {
		if s, ok := label.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// planDependsOn returns the titles of the issues a plan item declares as
// prerequisites. An empty list means the issue is not blocked.
func planDependsOn(item any) []string {
	var out []string
	depends, _ := item.(map[string]any)["depends_on"].([]any)
	for _, d := range depends {
		if s, ok := d.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// linearPriority maps the plan's urgency onto Linear's 0-4 integer priority.
func linearPriority(p string) int {
	switch p {
	case "urgent":
		return 1
	case "high":
		return 2
	case "medium":
		return 3
	case "low":
		return 4
	default:
		return 0
	}
}

func goalTitle(prompt string) string {
	first := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	if first == "" {
		return "Goal"
	}
	if len(first) > 200 {
		first = first[:200]
	}
	return "Goal: " + first
}

func goalPrompt(goal string) string {
	return fmt.Sprintf("Investigate the repository at the current directory to plan work toward this goal. Do not modify any files or create commits; this is read-only analysis.\n\nGoal: %s\n\nIdentify concrete problems and improvements that advance the goal, and break them into small, well-scoped Linear issues. Each issue must have a clear title and a description that states requirements and acceptance criteria. List issues in dependency order: set each issue's \"depends_on\" to the titles of the issues that must be completed before it can begin, or an empty array if it has none. Never depend on a later issue or create circular dependencies. Leave \"refinements\" and \"flags\" as empty arrays; they are used only when reviewing an existing goal.\n\nReturn only JSON matching this schema, with no markdown fences:\n%s\n", goal, asset("goal-plan.schema.json"))
}

func printGoalPlan(out io.Writer, plan map[string]any) {
	data, _ := json.MarshalIndent(plan, "", "  ")
	fmt.Fprintln(out, string(data))
}

func goalSummaryBody(prompt string, plan map[string]any, created []goalIssue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Investigated goal: %s\n\n", prompt)
	if findings, _ := plan["findings"].([]any); len(findings) > 0 {
		b.WriteString("Findings:\n")
		for _, finding := range findings {
			fmt.Fprintf(&b, "- [%s] %s (%s)\n", textAt(finding, "severity"), textAt(finding, "summary"), textAt(finding, "evidence"))
		}
		b.WriteString("\n")
	}
	b.WriteString("Created issues:\n")
	for i, item := range planIssues(plan) {
		identifier := ""
		if i < len(created) {
			identifier = created[i].Identifier
		}
		if identifier == "" {
			fmt.Fprintf(&b, "- %s\n", textAt(item, "title"))
		} else {
			fmt.Fprintf(&b, "- %s: %s\n", identifier, textAt(item, "title"))
		}
	}
	if questions, _ := plan["follow_up_questions"].([]any); len(questions) > 0 {
		b.WriteString("\nFollow-up questions:\n")
		for _, q := range questions {
			fmt.Fprintf(&b, "- %s\n", q.(string))
		}
	}
	return b.String()
}

func existingGoal(db *sql.DB, fingerprint string) (goalRecord, error) {
	var g goalRecord
	var created string
	err := db.QueryRow("SELECT goal_id,project_id,identifier,prompt,folder,status,created_issues,plan_fingerprint,last_reviewed_at FROM goals WHERE plan_fingerprint=?", fingerprint).Scan(&g.GoalID, &g.ProjectID, &g.Identifier, &g.Prompt, &g.Folder, &g.Status, &created, &g.Fingerprint, &g.LastReviewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(created), &g.Created)
	return g, nil
}

func saveGoal(db *sql.DB, g goalRecord) error {
	created, err := json.Marshal(g.Created)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO goals VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(goal_id) DO UPDATE SET created_issues=excluded.created_issues, last_reviewed_at=excluded.last_reviewed_at`, g.GoalID, g.ProjectID, g.Identifier, g.Prompt, g.Folder, g.Status, string(created), g.Fingerprint, g.LastReviewedAt)
	return err
}

func loadGoal(db *sql.DB, id string) (goalRecord, error) {
	var g goalRecord
	var created string
	err := db.QueryRow("SELECT goal_id,project_id,identifier,prompt,folder,status,created_issues,plan_fingerprint,last_reviewed_at FROM goals WHERE goal_id=?", id).Scan(&g.GoalID, &g.ProjectID, &g.Identifier, &g.Prompt, &g.Folder, &g.Status, &created, &g.Fingerprint, &g.LastReviewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, errors.New("no recorded goal for this issue")
	}
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(created), &g.Created)
	return g, nil
}

func listGoals(db *sql.DB) ([]goalRecord, error) {
	rows, err := db.Query("SELECT goal_id,project_id,identifier,prompt,folder,status,created_issues,plan_fingerprint,last_reviewed_at FROM goals WHERE status='ready' ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var goals []goalRecord
	for rows.Next() {
		var g goalRecord
		var created string
		if err = rows.Scan(&g.GoalID, &g.ProjectID, &g.Identifier, &g.Prompt, &g.Folder, &g.Status, &created, &g.Fingerprint, &g.LastReviewedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(created), &g.Created)
		goals = append(goals, g)
	}
	return goals, rows.Err()
}

// commentTime parses a Linear comment's createdAt; unparseable values yield zero time.
func commentTime(comment map[string]any) time.Time {
	t, _ := time.Parse(time.RFC3339, textAt(comment, "createdAt"))
	return t
}

// feedbackLatest returns the newest comment time in the set.
func feedbackLatest(comments []map[string]any) time.Time {
	var latest time.Time
	for _, comment := range comments {
		if t := commentTime(comment); t.After(latest) {
			latest = t
		}
	}
	return latest
}

// feedbackNewer returns comments newer than the given unix timestamp.
func feedbackNewer(comments []map[string]any, since int64) []map[string]any {
	out := []map[string]any{}
	for _, comment := range comments {
		if commentTime(comment).Unix() > since {
			out = append(out, comment)
		}
	}
	return out
}

// createIssue posts an issueCreate mutation and confirms the returned issue.
func (r *runner) createIssue(input map[string]any, folder, stage string) (map[string]any, error) {
	if err := writeJSON(filepath.Join(folder, stage+"-create-request.json"), input); err != nil {
		return nil, err
	}
	var data struct {
		Create map[string]any `json:"issueCreate"`
	}
	err := r.graphql(`mutation GoalIssueCreate($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier title state { id name } parent { id } } } }`, map[string]any{"input": input}, &data)
	if err != nil {
		return nil, err
	}
	if !boolAt(data.Create, "success") || !uuidRE.MatchString(textAt(data.Create, "issue", "id")) || textAt(data.Create, "issue", "identifier") == "" {
		return nil, errors.New("Linear issue creation was not confirmed")
	}
	if err := writeJSON(filepath.Join(folder, stage+"-create.json"), data.Create); err != nil {
		return nil, err
	}
	issue, _ := data.Create["issue"].(map[string]any)
	return issue, nil
}

// createRelation records a "blocks" relation so blockerID blocks dependentID.
// Linear stores "X is blocked by Y" as a directional "blocks" edge from Y to X.
func (r *runner) createRelation(blockerID, dependentID, folder, stage string) error {
	variables := map[string]any{"input": map[string]any{"issueId": blockerID, "relatedIssueId": dependentID, "type": "blocks"}}
	if err := writeJSON(filepath.Join(folder, stage+"-relation-request.json"), variables); err != nil {
		return err
	}
	var data struct {
		Create map[string]any `json:"issueRelationCreate"`
	}
	err := r.graphql(`mutation GoalRelationCreate($input: IssueRelationCreateInput!) { issueRelationCreate(input: $input) { success issueRelation { id type issue { id } relatedIssue { id } } } }`, variables, &data)
	if err != nil {
		return err
	}
	if !boolAt(data.Create, "success") || textAt(data.Create, "issueRelation", "type") != "blocks" || textAt(data.Create, "issueRelation", "issue", "id") != blockerID || textAt(data.Create, "issueRelation", "relatedIssue", "id") != dependentID {
		return errors.New("Linear relation creation was not confirmed")
	}
	return writeJSON(filepath.Join(folder, stage+"-relation.json"), data.Create)
}

// linkDependencies creates a "blocks" relation from each prerequisite title to
// the dependent issue, resolving titles against titleToID.
func (r *runner) linkDependencies(dependentID string, prereqs []string, titleToID map[string]string, folder, stage string) error {
	for i, title := range uniqueStrings(prereqs) {
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		blockerID := titleToID[title]
		if blockerID == "" {
			return fmt.Errorf("goal dependency %q does not match any issue", title)
		}
		if blockerID == dependentID {
			continue
		}
		if err := r.createRelation(blockerID, dependentID, folder, fmt.Sprintf("%s-%d", stage, i)); err != nil {
			return err
		}
	}
	return nil
}

// labelIDs resolves Linear label UUIDs by name.
func (r *runner) labelIDs(names []string) (map[string]string, error) {
	result := map[string]string{}
	if len(names) == 0 {
		return result, nil
	}
	var data struct {
		Labels *struct {
			Nodes []struct{ ID, Name string } `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := r.graphql(`query GoalLabels($names: [String!]!) { issueLabels(filter: { name: { in: $names } }) { nodes { id name } } }`, map[string]any{"names": names}, &data); err != nil {
		return nil, err
	}
	if data.Labels == nil {
		return nil, errors.New("Linear returned no labels")
	}
	for _, node := range data.Labels.Nodes {
		result[node.Name] = node.ID
	}
	return result, nil
}

// goalReviewRead fetches a goal's comments and its children (each with their own
// nested comments) in a single request, collapsing what would otherwise be 3+N
// separate reads per goal per poll tick.
func (r *runner) goalReviewRead(goalID string) (goalComments, children, childComments []map[string]any, err error) {
	var data struct {
		Issue struct {
			Comments struct {
				Nodes []map[string]any `json:"nodes"`
			} `json:"comments"`
			Children struct {
				Nodes []map[string]any `json:"nodes"`
			} `json:"children"`
		} `json:"issue"`
	}
	if err = r.graphql(`query GoalReviewRead($id: String!) { issue(id: $id) { comments { nodes { id body user { id name } createdAt } } children(first: 50) { nodes { id identifier title state { id name } comments { nodes { id body user { id name } createdAt } } } } } }`, map[string]any{"id": goalID}, &data); err != nil {
		return nil, nil, nil, err
	}
	for _, node := range data.Issue.Children.Nodes {
		children = append(children, node)
		if comments, ok := node["comments"].(map[string]any); ok {
			if nodes, ok := comments["nodes"].([]any); ok {
				for _, c := range nodes {
					if m, ok := c.(map[string]any); ok {
						childComments = append(childComments, m)
					}
				}
			}
		}
	}
	return data.Issue.Comments.Nodes, children, childComments, nil
}

// refineIssue edits an existing issue's title/description in place.
func (r *runner) refineIssue(id, title, description, folder, stage string) error {
	input := map[string]any{}
	if strings.TrimSpace(title) != "" {
		input["title"] = title
	}
	if strings.TrimSpace(description) != "" {
		input["description"] = description
	}
	if len(input) == 0 {
		return nil
	}
	variables := map[string]any{"id": id, "input": input}
	if err := writeJSON(filepath.Join(folder, stage+"-refine-request.json"), variables); err != nil {
		return err
	}
	var data struct {
		Update map[string]any `json:"issueUpdate"`
	}
	if err := r.graphql(`mutation GoalRefine($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success issue { id title description state { id } } } }`, variables, &data); err != nil {
		return err
	}
	if !boolAt(data.Update, "success") || textAt(data.Update, "issue", "id") != id {
		return errors.New("Linear issue refinement was not confirmed")
	}
	return writeJSON(filepath.Join(folder, stage+"-refine.json"), data.Update)
}

// launchGoalAgent runs a read-only investigation and returns the validated plan.
func (r *runner) launchGoalAgent(c *config, repo, folder, prompt string, lock *os.File) (map[string]any, error) {
	engine, binary := c.agent()
	schemaPath := filepath.Join(folder, "goal-plan.schema.json")
	if err := writeFile(schemaPath, []byte(asset("goal-plan.schema.json"))); err != nil {
		return nil, err
	}
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "LINEAR_API_KEY=") {
			env = append(env, v)
		}
	}
	var args []string
	if engine == "codex" {
		args = []string{"-a", "never", "exec", "--sandbox", "read-only", "--ephemeral", "--cd", repo, "--json", "--output-schema", schemaPath, "--output-last-message", filepath.Join(folder, "result.json")}
	} else {
		permission := map[string]any{"bash": "allow", "task": "deny", "question": "deny", "edit": map[string]string{"*": "deny"}, "external_directory": map[string]string{"*": "deny"}}
		settings := map[string]any{"share": "disabled", "autoupdate": false, "agent": map[string]any{"linear-goal": map[string]any{"description": "Investigate a repository goal and plan issues", "mode": "primary", "permission": permission}}}
		data, err := json.Marshal(settings)
		if err != nil {
			return nil, err
		}
		env = setEnv(env, "OPENCODE_CONFIG_CONTENT", string(data))
		if err = writeFile(filepath.Join(folder, "opencode.json"), append(data, '\n')); err != nil {
			return nil, err
		}
		args = []string{"run", "--format", "json", "--dir", repo, "--agent", "linear-goal"}
	}
	args = append(args, c.modelArgs()...)
	if engine == "codex" {
		args = append(args, "-")
	}
	out, err := os.OpenFile(filepath.Join(folder, "events.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	stderr, err := os.OpenFile(filepath.Join(folder, "stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd := exec.Command(binary, args...)
	cmd.Dir = repo
	cmd.Env = env
	cmd.Stdin = bytes.NewBufferString(prompt)
	cmd.Stdout = out
	cmd.Stderr = stderr
	if lock != nil {
		cmd.ExtraFiles = []*os.File{lock}
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.featureTimeout)
	defer cancel()
	if err = runModel(ctx, cmd, out, stderr); err != nil {
		return nil, fmt.Errorf("goal %s failed: %w; inspect stderr.log", engine, err)
	}
	if engine == "opencode" {
		if err = opencodeReport(folder); err != nil {
			return nil, err
		}
	}
	return readGoalPlan(folder)
}

// runGoal investigates a prompt and creates a parent goal issue with sub-issues.
func (r *runner) runGoal(c *config, project, promptText string, dryRun, autostart bool, runs string, db *sql.DB, lock *os.File) error {
	rt := c.Projects[project]
	if rt == nil {
		return errors.New("goal requires a configured --project UUID")
	}
	if rt.TriageStateID == "" {
		return errors.New("triage_state_id is required in the project route to create goal issues")
	}
	promptText = strings.TrimSpace(promptText)
	if promptText == "" {
		return errors.New("goal requires a --prompt or --prompt-file with content")
	}
	fingerprint := goalFingerprint(project, promptText)
	if existing, err := existingGoal(db, fingerprint); err != nil {
		return err
	} else if existing.GoalID != "" {
		fmt.Fprintf(r.out, "%s: goal already processed; use `goal review --goal %s` to continue from feedback\n", existing.Identifier, existing.GoalID)
		return nil
	}
	goalsDir := filepath.Join(runs, "goals")
	if err := r.checkCapacity(c); err != nil {
		return err
	}
	if err := os.MkdirAll(goalsDir, 0700); err != nil {
		return err
	}
	folder, err := os.MkdirTemp(goalsDir, "goal-")
	if err != nil {
		return err
	}
	if err = recordActivity(folder, "investigating"); err != nil {
		return err
	}
	if err = writeFile(filepath.Join(folder, "goal.json"), []byte(promptText)); err != nil {
		return err
	}
	repo := filepath.Join(folder, "repo")
	if _, err = r.command("", "git", "clone", "--no-hardlinks", rt.RepoPath, repo); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "fetch", "origin", "staging"); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}
	plan, err := r.launchGoalAgent(c, repo, folder, goalPrompt(promptText), lock)
	if err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "goal-plan.json"), plan); err != nil {
		return err
	}
	if len(planIssues(plan)) == 0 {
		return errors.New("goal plan must include at least one issue")
	}
	if dryRun {
		printGoalPlan(r.out, plan)
		fmt.Fprintln(r.out, "Dry run: no issues were created")
		return nil
	}
	goal, err := r.createIssue(map[string]any{"teamId": rt.TeamID, "title": goalTitle(promptText), "description": promptText, "stateId": rt.TriageStateID}, folder, "goal")
	if err != nil {
		return err
	}
	goalID := textAt(goal, "id")
	identifier := textAt(goal, "identifier")
	finalFolder := filepath.Join(runs, goalID)
	if err = os.Rename(folder, finalFolder); err != nil {
		return err
	}
	folder = finalFolder
	labelNames := []string{}
	if autostart && rt.Label != "" {
		labelNames = append(labelNames, rt.Label)
	}
	for _, item := range planIssues(plan) {
		labelNames = append(labelNames, planLabels(item)...)
	}
	labels, err := r.labelIDs(uniqueStrings(labelNames))
	if err != nil {
		return err
	}
	var created []goalIssue
	for i, item := range planIssues(plan) {
		stateID := rt.TriageStateID
		var issueLabelIDs []string
		if autostart {
			stateID = rt.ReadyStateID
			if rt.Label != "" {
				issueLabelIDs = append(issueLabelIDs, labels[rt.Label])
			}
		}
		for _, name := range planLabels(item) {
			if id := labels[name]; id != "" {
				issueLabelIDs = append(issueLabelIDs, id)
			}
		}
		input := map[string]any{"teamId": rt.TeamID, "title": textAt(item, "title"), "description": textAt(item, "description"), "stateId": stateID, "parentId": goalID}
		if p := linearPriority(textAt(item, "priority")); p != 0 {
			input["priority"] = p
		}
		if len(issueLabelIDs) > 0 {
			input["labelIds"] = uniqueStrings(issueLabelIDs)
		}
		child, err := r.createIssue(input, folder, fmt.Sprintf("child-%d", i+1))
		if err != nil {
			return err
		}
		created = append(created, goalIssue{ID: textAt(child, "id"), Identifier: textAt(child, "identifier")})
	}
	titleToID := map[string]string{}
	for i, item := range planIssues(plan) {
		if i < len(created) {
			titleToID[textAt(item, "title")] = created[i].ID
		}
	}
	for i, item := range planIssues(plan) {
		if i >= len(created) {
			break
		}
		if err = r.linkDependencies(created[i].ID, planDependsOn(item), titleToID, folder, fmt.Sprintf("dep-child-%d", i+1)); err != nil {
			return err
		}
	}
	record := goalRecord{GoalID: goalID, ProjectID: project, Identifier: identifier, Prompt: promptText, Folder: folder, Status: "ready", Created: created, Fingerprint: fingerprint}
	if err = saveGoal(db, record); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "goal-record.json"), record); err != nil {
		return err
	}
	if err = r.postComment(goalID, goalSummaryBody(promptText, plan, created), folder, "goal"); err != nil {
		fmt.Fprintln(r.errOut, "Goal created; inspect goal comment delivery evidence")
	}
	printGoalPlan(r.out, plan)
	fmt.Fprintf(r.out, "%s: created goal with %d issue(s)\n", identifier, len(created))
	return nil
}

func (r *runner) executeGoal(configPath, project, promptText, promptFile string, dryRun, autostart bool) error {
	if promptText == "" && promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return err
		}
		promptText = string(data)
	}
	runs := filepath.Join(r.root, ".runs")
	if err := os.MkdirAll(runs, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("worker still active; goal refused")
		}
		return err
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := r.loadConfig(configPath)
	if err != nil {
		return err
	}
	return r.runGoal(c, project, promptText, dryRun, autostart, runs, db, lock)
}

// isDirective reports whether a comment is an explicit directive: it starts with
// "/feedback" or mentions "@agent".
func isDirective(comment map[string]any) bool {
	body := strings.ToLower(strings.TrimSpace(textAt(comment, "body")))
	return strings.HasPrefix(body, "/feedback") || strings.Contains(body, "@agent")
}

func reviewPrompt(goal string, feedback, children []map[string]any) string {
	var directives, context []map[string]any
	for _, comment := range feedback {
		if isDirective(comment) {
			directives = append(directives, comment)
		} else {
			context = append(context, comment)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are reviewing a goal and its issues to incorporate stakeholder feedback. Do not modify any files or create commits; this is read-only analysis.\n\nGoal: %s\n\nCurrent issues:\n", goal)
	for _, child := range children {
		fmt.Fprintf(&b, "- %s [%s]: %s\n", textAt(child, "identifier"), textAt(child, "state", "name"), textAt(child, "title"))
	}
	if len(directives) > 0 {
		b.WriteString("\nExplicit directives (act on every one):\n")
		for _, comment := range directives {
			fmt.Fprintf(&b, "- %s: %s\n", textAt(comment, "user", "name"), textAt(comment, "body"))
		}
	}
	if len(context) > 0 {
		b.WriteString("\nOther team feedback (context):\n")
		for _, comment := range context {
			fmt.Fprintf(&b, "- %s: %s\n", textAt(comment, "user", "name"), textAt(comment, "body"))
		}
	}
	if len(feedback) == 0 {
		b.WriteString("\nNo new stakeholder feedback.\n")
	}
	b.WriteString("\nProduce a plan with:\n- issues: new follow-up issues to create, each with a title and a description stating requirements and acceptance criteria.\n- refinements: edits to EXISTING issues (referenced by identifier), only when they are still in triage; give the new title and/or description.\n- flags: stale, duplicate or superseded issues to flag for a human to close.\nSet \"depends_on\" (on issues and refinements) to the titles of the issues that must be completed first, or an empty array if there are none. Never depend on a new follow-up or create circular dependencies.\n\nReturn only JSON matching this schema, with no markdown fences:\n")
	b.WriteString(asset("goal-plan.schema.json"))
	b.WriteString("\n")
	return b.String()
}

func reviewSummaryBody(refined, flagged []string, created []goalIssue) string {
	var b strings.Builder
	b.WriteString("Reviewed goal with stakeholder feedback.\n")
	if len(created) > 0 {
		b.WriteString("\nCreated follow-ups:\n")
		for _, item := range created {
			fmt.Fprintf(&b, "- %s\n", item.Identifier)
		}
	}
	if len(refined) > 0 {
		b.WriteString("\nRefined in place (triage):\n")
		for _, identifier := range refined {
			fmt.Fprintf(&b, "- %s\n", identifier)
		}
	}
	if len(flagged) > 0 {
		b.WriteString("\nFlagged for human review (not closed):\n")
		for _, flag := range flagged {
			fmt.Fprintf(&b, "- %s\n", flag)
		}
	}
	return b.String()
}

// reviewGoal reads stakeholder feedback on a goal and its children, then applies
// follow-ups (new issues), in-place refinements (triage only) and flags (never closed).
// In sweep mode it only acts on feedback newer than the recorded high-water mark.
// A non-empty viewer is reused across goals; an empty viewer is resolved here.
func (r *runner) reviewGoal(c *config, goalID string, dryRun, sweep bool, viewer string, db *sql.DB, lock *os.File) error {
	if !uuidRE.MatchString(goalID) {
		return errors.New("review requires --goal with a recorded goal UUID")
	}
	record, err := loadGoal(db, goalID)
	if err != nil {
		return err
	}
	rt := c.Projects[record.ProjectID]
	if rt == nil {
		return errors.New("recorded goal project is not configured")
	}
	if rt.TriageStateID == "" {
		return errors.New("triage_state_id is required in the project route to review goal issues")
	}
	if viewer == "" {
		viewer, _, err = r.viewer()
		if err != nil {
			return err
		}
	}
	goalComments, children, childComments, err := r.goalReviewRead(goalID)
	if err != nil {
		return err
	}
	// Keep only team feedback; drop the runner's own bookkeeping comments.
	var allFeedback []map[string]any
	for _, comment := range goalComments {
		if textAt(comment, "user", "id") != viewer {
			allFeedback = append(allFeedback, comment)
		}
	}
	for _, comment := range childComments {
		if textAt(comment, "user", "id") != viewer {
			allFeedback = append(allFeedback, comment)
		}
	}
	latest := feedbackLatest(allFeedback)
	var feedback []map[string]any
	if sweep {
		if latest.Unix() <= record.LastReviewedAt {
			fmt.Fprintf(r.out, "%s: skipped; no new feedback\n", record.Identifier)
			return nil
		}
		feedback = feedbackNewer(allFeedback, record.LastReviewedAt)
	} else {
		feedback = allFeedback
	}
	if err = r.checkCapacity(c); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(record.Folder, "goal-feedback.json"), map[string]any{"goal": goalID, "children": children, "comments": feedback, "all_comments": allFeedback}); err != nil {
		return err
	}
	plan, err := r.launchGoalAgent(c, filepath.Join(record.Folder, "repo"), record.Folder, reviewPrompt(record.Prompt, feedback, children), lock)
	if err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(record.Folder, "goal-review-plan.json"), plan); err != nil {
		return err
	}
	if dryRun {
		printGoalPlan(r.out, plan)
		fmt.Fprintln(r.out, "Dry run: no follow-ups or refinements were applied")
		return nil
	}
	// Consume the feedback before any Linear mutation: advance the high-water mark
	// and persist it first, so a failure partway through cannot be re-processed by
	// the next sweep and duplicate already-created work.
	if latest.Unix() > record.LastReviewedAt {
		record.LastReviewedAt = latest.Unix()
		if err = saveGoal(db, record); err != nil {
			return err
		}
	}
	byIdentifier := map[string]map[string]any{}
	for _, child := range children {
		if identifier := textAt(child, "identifier"); identifier != "" {
			byIdentifier[identifier] = child
		}
	}
	titleToID := map[string]string{}
	for _, child := range children {
		if title := textAt(child, "title"); title != "" {
			titleToID[title] = textAt(child, "id")
		}
	}
	type dependency struct {
		dependent string
		prereqs   []string
	}
	var dependencies []dependency
	labelNames := []string{}
	for _, item := range planIssues(plan) {
		labelNames = append(labelNames, planLabels(item)...)
	}
	labels, err := r.labelIDs(uniqueStrings(labelNames))
	if err != nil {
		return err
	}
	var created []goalIssue
	var refined, flagged []string
	for i, item := range planIssues(plan) {
		input := map[string]any{"teamId": rt.TeamID, "title": textAt(item, "title"), "description": textAt(item, "description"), "stateId": rt.TriageStateID, "parentId": goalID}
		if p := linearPriority(textAt(item, "priority")); p != 0 {
			input["priority"] = p
		}
		var issueLabelIDs []string
		for _, name := range planLabels(item) {
			if id := labels[name]; id != "" {
				issueLabelIDs = append(issueLabelIDs, id)
			}
		}
		if len(issueLabelIDs) > 0 {
			input["labelIds"] = uniqueStrings(issueLabelIDs)
		}
		child, err := r.createIssue(input, record.Folder, fmt.Sprintf("followup-%d", i+1))
		if err != nil {
			return err
		}
		created = append(created, goalIssue{ID: textAt(child, "id"), Identifier: textAt(child, "identifier")})
		titleToID[textAt(item, "title")] = textAt(child, "id")
		dependencies = append(dependencies, dependency{dependent: textAt(child, "id"), prereqs: planDependsOn(item)})
	}
	for i, item := range planRefinements(plan) {
		identifier := textAt(item, "issue")
		child := byIdentifier[identifier]
		if child == nil {
			flagged = append(flagged, identifier+" (not found)")
			continue
		}
		if textAt(child, "state", "id") == rt.TriageStateID {
			if err = r.refineIssue(textAt(child, "id"), textAt(item, "title"), textAt(item, "description"), record.Folder, fmt.Sprintf("refine-%d", i+1)); err != nil {
				return err
			}
			refined = append(refined, identifier)
			dependencies = append(dependencies, dependency{dependent: textAt(child, "id"), prereqs: planDependsOn(item)})
		} else {
			// Started/completed work has frozen requirements: express the refinement as a follow-up.
			title := textAt(item, "title")
			if title == "" {
				title = "Refine " + identifier
			}
			description := textAt(item, "description")
			if description == "" {
				description = "Refine " + identifier + " per stakeholder feedback."
			} else {
				description = "Refines " + identifier + " (already started): " + description
			}
			child, err := r.createIssue(map[string]any{"teamId": rt.TeamID, "title": title, "description": description, "stateId": rt.TriageStateID, "parentId": goalID}, record.Folder, fmt.Sprintf("refine-followup-%d", i+1))
			if err != nil {
				return err
			}
			created = append(created, goalIssue{ID: textAt(child, "id"), Identifier: textAt(child, "identifier")})
			titleToID[title] = textAt(child, "id")
			dependencies = append(dependencies, dependency{dependent: textAt(child, "id"), prereqs: planDependsOn(item)})
		}
	}
	for _, item := range planFlags(plan) {
		flagged = append(flagged, fmt.Sprintf("%s: %s", textAt(item, "issue"), textAt(item, "reason")))
	}
	for i, dep := range dependencies {
		if err = r.linkDependencies(dep.dependent, dep.prereqs, titleToID, record.Folder, fmt.Sprintf("dep-%d", i+1)); err != nil {
			return err
		}
	}
	record.Created = append(record.Created, created...)
	if err = saveGoal(db, record); err != nil {
		return err
	}
	if len(created) > 0 || len(refined) > 0 || len(flagged) > 0 {
		if err = r.postComment(goalID, reviewSummaryBody(refined, flagged, created), record.Folder, "review"); err != nil {
			fmt.Fprintln(r.errOut, "Review applied; inspect review comment delivery evidence")
		}
	}
	printGoalPlan(r.out, plan)
	fmt.Fprintf(r.out, "%s: reviewed; %d follow-up(s), %d refined, %d flagged\n", record.Identifier, len(created), len(refined), len(flagged))
	return nil
}

func (r *runner) executeGoalReview(configPath, goalID string, dryRun bool) error {
	runs := filepath.Join(r.root, ".runs")
	if err := os.MkdirAll(runs, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("worker still active; review refused")
		}
		return err
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := r.loadConfig(configPath)
	if err != nil {
		return err
	}
	return r.reviewGoal(c, goalID, dryRun, false, "", db, lock)
}

// reviewAll sweeps every ready goal, reviewing only those with new feedback.
func (r *runner) reviewAll(c *config, dryRun bool, db *sql.DB, lock *os.File) error {
	goals, err := listGoals(db)
	if err != nil {
		return err
	}
	if len(goals) == 0 {
		return nil
	}
	viewer, _, err := r.viewer()
	if err != nil {
		return err
	}
	for _, record := range goals {
		if err = r.reviewGoal(c, record.GoalID, dryRun, true, viewer, db, lock); err != nil {
			var capacity *capacityWait
			if errors.As(err, &capacity) {
				return err
			}
			var limit *providerLimit
			if errors.As(err, &limit) {
				if e := saveCooldown(db, c, limit); e != nil {
					return errors.Join(err, e)
				}
				return err
			}
			// One un-reviewable goal must not abort the whole sweep.
			fmt.Fprintf(r.errOut, "%s: review skipped: %v\n", record.Identifier, redact(err.Error()))
		}
	}
	return nil
}

func (r *runner) executeGoalReviewAll(configPath string, dryRun bool) error {
	runs := filepath.Join(r.root, ".runs")
	if err := os.MkdirAll(runs, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("worker still active; review refused")
		}
		return err
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := r.loadConfig(configPath)
	if err != nil {
		return err
	}
	if !dryRun {
		limit, err := activeCooldown(db, c)
		if err != nil {
			return err
		}
		if limit != nil {
			fmt.Fprintln(r.out, limit.Error())
			return nil
		}
	}
	return r.reviewAll(c, dryRun, db, lock)
}
