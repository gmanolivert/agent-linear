package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskCapacityThresholdAndMeasurement(t *testing.T) {
	const floor uint64 = 8000000000
	for _, tc := range []struct {
		name    string
		free    uint64
		err     error
		blocked bool
	}{
		{"below", floor - 1, nil, true},
		{"equal", floor, nil, false},
		{"above", floor + 1, nil, false},
		{"unavailable", floor + 1, errors.New("private probe detail"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(filepath.Join(t.TempDir(), "agent-linear"))
			r.availableBytes = func(path string) (uint64, error) {
				if path != filepath.Join(r.root, ".runs") {
					t.Fatal("measured wrong filesystem", path)
				}
				return tc.free, tc.err
			}
			err := r.checkCapacity(&config{MinFreeBytes: floor})
			var wait *capacityWait
			if errors.As(err, &wait) != tc.blocked {
				t.Fatalf("blocked=%v, error=%v", tc.blocked, err)
			}
			if err != nil && strings.Contains(err.Error(), "private probe detail") {
				t.Fatal("raw measurement error leaked")
			}
		})
	}
	r := newRunner("/unused/agent-linear")
	r.availableBytes = func(string) (uint64, error) { t.Fatal("disabled reserve measured disk"); return 0, nil }
	if err := r.checkCapacity(&config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := availableDiskBytes(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := availableDiskBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing filesystem accepted")
	}
}

func TestDiskCapacityConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"0", true}, {"8000000000", true}, {"18446744073709551615", true},
		{"-1", false}, {"1.5", false}, {"null", false}, {"\"8000000000\"", false}, {"true", false}, {"18446744073709551616", false},
	} {
		_, err := decodeConfig([]byte(`{"min_free_bytes":` + tc.value + `}`))
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.value, err)
		}
	}
}

func TestDiskCapacityDropsBetweenQueuedIssues(t *testing.T) {
	root, r, c, api := gapRunner(t, "ready")
	second := testIssue()
	secondID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	second["id"], second["identifier"] = secondID, "APP-2"
	api.moreIssues = []map[string]any{second}
	c.MinFreeBytes = 8000000000
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	r.availableBytes = func(string) (uint64, error) {
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.liveState == reviewID {
			return 7999999999, nil
		}
		return 12000000000, nil
	}
	var out bytes.Buffer
	r.out = &out
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
		t.Fatal("first run not completed", status, err)
	}
	if status, err := claimedStatus(db, secondID); err != nil || status != "" {
		t.Fatal("second run claimed", status, err)
	}
	if exists(filepath.Join(root, ".runs", secondID)) {
		t.Fatal("second run directory created")
	}
	if !strings.Contains(out.String(), "disk capacity low") {
		t.Fatal("missing capacity explanation", out.String())
	}
	var health pollHealth
	if err := readJSON(filepath.Join(root, ".runs", "poll-health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if health.Outcome != "deferred" {
		t.Fatal("poll did not record deferral", health)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "mutation") && textAt(call.Variables, "id") == secondID {
			t.Fatal("second issue mutated")
		}
	}
}

func TestDiskCapacityDropsDuringPreflight(t *testing.T) {
	root, r, c, api := gapRunner(t, "ready")
	c.MinFreeBytes = 8000000000
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	r.availableBytes = func(string) (uint64, error) {
		if exists(filepath.Join(root, "preflight-calls")) {
			return 7999999999, nil
		}
		return 12000000000, nil
	}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, ".runs", issueID)) {
		t.Fatal("provider probe consumed reserve but issue was claimed")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "mutation") {
			t.Fatal("capacity refusal mutated Linear")
		}
	}
}

func TestDiskCapacityProtectsRecovery(t *testing.T) {
	for _, mode := range []string{"manual", "automatic", "after-probe", "measurement-error"} {
		t.Run(mode, func(t *testing.T) {
			root, r, c, _, db := providerRecoveryFixture(t, "codex")
			expireProviderRecovery(t, db)
			folder := filepath.Join(root, ".runs", issueID)
			before, err := os.ReadFile(filepath.Join(folder, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			c.MinFreeBytes = 8000000000
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(root, "outcome"), "ready")
			r.availableBytes = func(string) (uint64, error) {
				if mode == "measurement-error" {
					return 0, errors.New("probe failed")
				}
				if mode == "after-probe" {
					data, _ := os.ReadFile(filepath.Join(root, "preflight-calls"))
					if bytes.Count(data, []byte("\n")) < 2 {
						return 12000000000, nil
					}
				}
				return 7999999999, nil
			}
			if mode == "manual" {
				var wait *capacityWait
				if err := executeTest(r, "resume", "--issue", issueID); !errors.As(err, &wait) {
					t.Fatal("manual resume not refused", err)
				}
			} else if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := db.QueryRow("SELECT state FROM provider_recoveries WHERE issue_id=?", issueID).Scan(&state); err != nil || state != "waiting" {
				t.Fatal("lost continuation eligibility", state, err)
			}
			if status, err := claimedStatus(db, issueID); err != nil || status != "blocked" {
				t.Fatal("changed retained run", status, err)
			}
			after, err := os.ReadFile(filepath.Join(folder, "events.jsonl"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("changed recorded session", err)
			}
			if exists(filepath.Join(root, "resume-calls.json")) || exists(filepath.Join(folder, "resumes")) {
				t.Fatal("continuation started or archived logs")
			}
			if err := executeTest(r, "status"); err != nil {
				t.Fatal("read-only status blocked", err)
			}
		})
	}
}

func TestDiskCapacityAllowsQueueInspectionAndReconciliation(t *testing.T) {
	root, r, c, _ := gapRunner(t, "ready")
	c.MinFreeBytes = 8000000000
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	r.availableBytes = func(string) (uint64, error) { t.Fatal("inspection measured capacity"); return 0, nil }
	if err := executeTest(r, "poll", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, ".runs", issueID)) {
		t.Fatal("queue inspection started work")
	}
	r.availableBytes = func(string) (uint64, error) { return 12000000000, nil }
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
	if err != nil {
		t.Fatal(err)
	}
	r.availableBytes = func(string) (uint64, error) { t.Fatal("reconciliation measured capacity"); return 0, nil }
	if err := executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reconciliation launched work", err)
	}
}

func TestDiskCapacityProtectsGoalWork(t *testing.T) {
	for _, mode := range []string{"new", "new-dry-run", "review", "review-dry-run", "sweep"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			source := testRepo(t, root)
			var r *runner
			var api *linearFixture
			args := []string{"goal", "--project", projectID, "--prompt", "A scoped goal"}
			if strings.HasPrefix(mode, "new") {
				fixtureBinaries(t, root)
				api = &linearFixture{issue: testIssue()}
				r = newFixtureRunner(t, root, source, api)
				writeGoalConfig(t, root, source, "codex")
			} else {
				r, api, db := setupGoalReview(t, root, source, "codex")
				defer db.Close()
				// Use the existing recorded goal without changing its feedback marker.
				testCapacityGoalReview(t, root, r, api, mode)
				return
			}
			if strings.HasSuffix(mode, "dry-run") {
				args = append(args, "--dry-run")
			}
			enableCapacityFixture(t, root, r)
			var wait *capacityWait
			if err := executeTest(r, args...); !errors.As(err, &wait) {
				t.Fatal("goal work not refused", err)
			}
			if exists(filepath.Join(root, ".runs", "goals")) {
				t.Fatal("low space created goal work")
			}
			assertNoCapacityMutation(t, api)
		})
	}
}

func enableCapacityFixture(t *testing.T, root string, r *runner) {
	t.Helper()
	path := filepath.Join(root, "config.json")
	c, err := r.loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.MinFreeBytes = 8000000000
	if err = writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	r.availableBytes = func(string) (uint64, error) { return 7999999999, nil }
}

func assertNoCapacityMutation(t *testing.T, api *linearFixture) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "mutation") {
			t.Fatal("low capacity mutated Linear")
		}
	}
}

func testCapacityGoalReview(t *testing.T, root string, r *runner, api *linearFixture, mode string) {
	t.Helper()
	enableCapacityFixture(t, root, r)
	args := []string{"goal", "review", "--goal", reviewGoalID}
	if mode == "sweep" {
		args = []string{"goal", "review", "--all"}
	}
	if mode == "review-dry-run" {
		args = append(args, "--dry-run")
	}
	var wait *capacityWait
	if err := executeTest(r, args...); !errors.As(err, &wait) {
		t.Fatal("goal review not refused", err)
	}
	if exists(filepath.Join(root, ".runs", reviewGoalID, "goal-feedback.json")) {
		t.Fatal("low space wrote goal feedback")
	}
	assertNoCapacityMutation(t, api)
}
