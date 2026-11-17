package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type config struct {
	Engine          string            `json:"engine"`
	Codex           string            `json:"codex"`
	OpenCode        string            `json:"opencode"`
	Model           *string           `json:"model,omitempty"`
	ReasoningEffort *string           `json:"model_reasoning_effort,omitempty"`
	GH              string            `json:"gh"`
	MinFreeBytes    uint64            `json:"min_free_bytes,omitempty"`
	Projects        map[string]*route `json:"projects"`
	projectOrder    []string
}
type route struct {
	TeamID               string `json:"team_id"`
	ReadyStateID         string `json:"ready_state_id"`
	TriageStateID        string `json:"triage_state_id"`
	Label                string `json:"label,omitempty"`
	RepoPath             string `json:"repo_path"`
	GitHubRepo           string `json:"github_repo"`
	BaseBranch           string `json:"base_branch"`
	RequireSignedCommits bool   `json:"require_signed_commits"`
	SkipCIChecks         bool   `json:"skip_ci_checks"`
	AttachmentLimitBytes int64  `json:"attachment_limit_bytes,omitempty"`
	Deployment           struct {
		Mode         string `json:"mode"`
		Environment  string `json:"environment"`
		Instructions string `json:"instructions"`
	} `json:"deployment"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var repoRE = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
var identifierRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-\d+$`)

func (c *config) agent() (string, string) {
	engine := c.Engine
	if engine == "" {
		engine = "codex"
	}
	binary := c.Codex
	if engine == "opencode" {
		binary = c.OpenCode
	}
	if binary == "" {
		binary = engine
	}
	return engine, binary
}

func (c *config) modelArgs() []string {
	var args []string
	if c.Model != nil {
		args = append(args, "--model", *c.Model)
	}
	if c.ReasoningEffort != nil {
		value, _ := json.Marshal(*c.ReasoningEffort)
		args = append(args, "-c", "model_reasoning_effort="+string(value))
	}
	return args
}
func projectIDs(c *config) []string {
	if len(c.projectOrder) == len(c.Projects) {
		return append([]string{}, c.projectOrder...)
	}
	ids := make([]string, 0, len(c.Projects))
	for id := range c.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func githubName(remote string) (string, error) {
	re := regexp.MustCompile(`^(?:git@github\.com:|https://github\.com/)([\w.-]+/[\w.-]+?)(?:\.git)?$`)
	m := re.FindStringSubmatch(remote)
	if m == nil {
		return "", errors.New("origin must be a GitHub SSH or HTTPS URL without credentials")
	}
	return strings.ToLower(m[1]), nil
}
func (r *runner) loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := decodeConfig(data)
	if err != nil {
		return nil, err
	}
	if err := r.validateConfig(c); err != nil {
		return nil, err
	}
	return c, nil
}

func decodeConfig(data []byte) (*config, error) {
	var raw map[string]any
	if err := decodeObject(data, &raw); err != nil {
		return nil, err
	}
	engine := "codex"
	if value, present := raw["engine"]; present {
		var ok bool
		engine, ok = value.(string)
		if !ok || (engine != "codex" && engine != "opencode") {
			return nil, errors.New("engine must be codex or opencode")
		}
	}
	for _, key := range []string{engine, "model", "model_reasoning_effort", "gh"} {
		if value, present := raw[key]; present {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("%s must be a nonempty string, or omitted", key)
			}
		}
	}
	var c config
	if value, present := raw["min_free_bytes"]; present && value == nil {
		return nil, errors.New("min_free_bytes must be a nonnegative integer, or omitted")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	order, err := configuredProjectOrder(data)
	if err != nil {
		return nil, err
	}
	c.projectOrder = order
	return &c, nil
}

func configuredProjectOrder(data []byte) ([]string, error) {
	var raw struct {
		Projects json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw.Projects) == 0 || string(raw.Projects) == "null" {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw.Projects))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("projects must be an object")
	}
	var order []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
		order = append(order, key.(string))
	}
	return order, nil
}
func (r *runner) validateConfig(c *config) error {
	engine, binary := c.agent()
	if engine != "codex" && engine != "opencode" {
		return errors.New("engine must be codex or opencode")
	}
	if strings.TrimSpace(binary) == "" {
		return errors.New("agent must name an executable")
	}
	if c.Model != nil && strings.TrimSpace(*c.Model) == "" {
		return errors.New("model must be a nonempty model name, or omitted")
	}
	if c.ReasoningEffort != nil && (engine != "codex" || strings.TrimSpace(*c.ReasoningEffort) == "") {
		return errors.New("model_reasoning_effort must be a nonempty Codex reasoning level, or omitted")
	}
	if c.GH == "" {
		return errors.New("gh must name an executable")
	}
	if len(c.Projects) == 0 {
		return errors.New("configure at least one project route")
	}
	for id, rt := range c.Projects {
		if rt == nil || !uuidRE.MatchString(id) || !uuidRE.MatchString(rt.TeamID) || !uuidRE.MatchString(rt.ReadyStateID) {
			return errors.New("project/team/ready-state IDs must be UUIDs")
		}
		// triage_state_id is optional for implementation work; goals require it.
		if rt.TriageStateID != "" && !uuidRE.MatchString(rt.TriageStateID) {
			return errors.New("triage_state_id must be a UUID, or omitted")
		}
		if rt.TriageStateID != "" && rt.TriageStateID == rt.ReadyStateID {
			return errors.New("triage_state_id must differ from ready_state_id")
		}
		if rt.BaseBranch != "staging" {
			return errors.New("all feature PRs must target staging")
		}
		if !repoRE.MatchString(rt.GitHubRepo) {
			return errors.New("github_repo must be OWNER/REPO")
		}
		info, err := os.Stat(rt.RepoPath)
		if !filepath.IsAbs(rt.RepoPath) || err != nil || !info.IsDir() {
			return errors.New("repo_path must be an existing absolute directory")
		}
		remote, err := r.command(rt.RepoPath, "git", "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		actual, err := githubName(remote)
		if err != nil {
			return err
		}
		if actual != strings.ToLower(rt.GitHubRepo) {
			return errors.New("local origin does not match the configured GitHub repository")
		}
		if !rt.RequireSignedCommits {
			return errors.New("all projects require signed commits; require_signed_commits must be true")
		}
		if rt.Deployment.Mode != "pr-only" || rt.Deployment.Environment != "none" {
			return errors.New("all projects must use PR-only policy with environment none")
		}
		if strings.TrimSpace(rt.Deployment.Instructions) == "" {
			return errors.New("PR-only policy instructions are required")
		}
	}
	return nil
}

var errIssueBody = errors.New("issue needs a title and body with requirements and acceptance criteria")

// terminalStates are the Linear workflow state types that resolve a block.
// A blocked issue becomes eligible again only once every blocker reaches one
// of these states.
var terminalStates = map[string]bool{"completed": true, "canceled": true, "duplicate": true}

// blockedBy returns the identifiers of issues that currently block issue. A
// block is active while its blocker's workflow state type is not terminal.
// Linear stores "X is blocked by Y" as an incoming edge on X: X's
// inverseRelations node of type "blocks" points at Y via its "issue" field.
func blockedBy(issue map[string]any) []string {
	relations, _ := issue["inverseRelations"].(map[string]any)
	nodes, _ := relations["nodes"].([]any)
	var blockers []string
	for _, node := range nodes {
		relation, ok := node.(map[string]any)
		if !ok || textAt(relation, "type") != "blocks" {
			continue
		}
		if terminalStates[textAt(relation["issue"], "state", "type")] {
			continue
		}
		if id := textAt(relation["issue"], "identifier"); id != "" {
			blockers = append(blockers, id)
		}
	}
	return blockers
}

func validateIssue(issue map[string]any, project string, rt *route) error {
	if !uuidRE.MatchString(textAt(issue, "id")) {
		return errors.New("invalid Linear issue UUID")
	}
	if !identifierRE.MatchString(textAt(issue, "identifier")) {
		return errors.New("invalid Linear identifier")
	}
	if textAt(issue, "project", "id") != project || textAt(issue, "team", "id") != rt.TeamID || textAt(issue, "state", "id") != rt.ReadyStateID {
		return errors.New("issue does not match the trusted project/team/ready-state route")
	}
	if strings.TrimSpace(textAt(issue, "title")) == "" || strings.TrimSpace(textAt(issue, "description")) == "" {
		return errIssueBody
	}
	return nil
}
func (r *runner) configureProjects(path string) error {
	var template, raw map[string]any
	var order []string
	if err := json.Unmarshal([]byte(asset("config.example.json")), &template); err != nil {
		return err
	}
	defaults := map[string]any(nil)
	for _, v := range template["projects"].(map[string]any) {
		defaults = v.(map[string]any)
		break
	}
	if exists(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		order, err = configuredProjectOrder(data)
		if err != nil {
			return err
		}
	} else {
		raw = template
		raw["projects"] = map[string]any{}
	}
	projects, ok := raw["projects"].(map[string]any)
	if !ok {
		return errors.New("projects must be an object")
	}
	reader := bufio.NewReader(r.in)
	for {
		id, err := prompt(reader, r.out, "Linear project UUID (blank to finish): ")
		if err != nil {
			return err
		}
		if id == "" {
			break
		}
		if !uuidRE.MatchString(id) {
			return errors.New("invalid project UUID")
		}
		id = strings.ToLower(id)
		previous, _ := projects[id].(map[string]any)
		rt := map[string]any{}
		for k, v := range defaults {
			rt[k] = v
		}
		for k, v := range previous {
			rt[k] = v
		}
		for _, field := range []struct{ key, label string }{{"team_id", "Linear team UUID"}, {"ready_state_id", "Ready status UUID"}, {"triage_state_id", "Triage status UUID"}, {"repo_path", "Local repository path"}} {
			def := textAt(previous, field.key)
			value, e := prompt(reader, r.out, fmt.Sprintf("%s [%s]: ", field.label, def))
			if e != nil {
				return e
			}
			if value == "" {
				value = def
			}
			rt[field.key] = value
		}
		if textAt(rt, "repo_path") == "" {
			return errors.New("local repository path is required")
		}
		rt["repo_path"], err = absPath(textAt(rt, "repo_path"))
		if err != nil {
			return err
		}
		remote, err := r.command(textAt(rt, "repo_path"), "git", "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		rt["github_repo"], err = githubName(remote)
		if err != nil {
			return err
		}
		label := textAt(rt, "label")
		if label == "" {
			label = "-"
		}
		value, err := prompt(reader, r.out, fmt.Sprintf("Ready label [%s] (- for none): ", label))
		if err != nil {
			return err
		}
		if value == "-" {
			delete(rt, "label")
		} else if value != "" {
			rt["label"] = value
		}
		if _, present := projects[id]; !present {
			order = append(order, id)
		}
		projects[id] = rt
		fmt.Fprintf(r.out, "%s -> %s (base: %s)\n", id, textAt(rt, "github_repo"), textAt(rt, "base_branch"))
		answer, err := prompt(reader, r.out, "Add another project? [y/N]: ")
		if err != nil {
			return err
		}
		if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
			break
		}
	}
	// Project configuration order determines queue processing order.
	var ordered bytes.Buffer
	ordered.WriteByte('{')
	for i, id := range order {
		if i > 0 {
			ordered.WriteByte(',')
		}
		key, err := json.Marshal(id)
		if err != nil {
			return err
		}
		value, err := json.Marshal(projects[id])
		if err != nil {
			return err
		}
		ordered.Write(key)
		ordered.WriteByte(':')
		ordered.Write(value)
	}
	ordered.WriteByte('}')
	raw["projects"] = json.RawMessage(ordered.Bytes())
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	c, err := decodeConfig(b)
	if err != nil {
		return err
	}
	if err = r.validateConfig(c); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Saved project routes to %s\n", path)
	return nil
}
