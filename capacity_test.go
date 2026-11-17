package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The unmodified launcher ignores this configured reserve and starts a job.
// Use an impossibly large reserve so the regression test needs no disk filling.
func TestDiskCapacityBlocksDispatch(t *testing.T) {
	for _, action := range []string{"poll", "run"} {
		t.Run(action, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "ready")
			path := filepath.Join(root, "config.json")
			var raw map[string]any
			if err := readJSON(path, &raw); err != nil {
				t.Fatal(err)
			}
			raw["min_free_bytes"] = uint64(1) << 60
			if err := writeJSON(path, raw); err != nil {
				t.Fatal(err)
			}
			args := []string{action}
			if action == "run" {
				args = append(args, "--issue", issueID)
			}
			err := executeTest(r, args...)
			if action == "poll" && err != nil {
				t.Fatalf("capacity wait should be a deferred poll: %v", err)
			}
			if action == "run" && (err == nil || !strings.Contains(err.Error(), "disk")) {
				t.Fatalf("manual dispatch must explain the capacity refusal: %v", err)
			}
			if exists(filepath.Join(root, ".runs", issueID)) {
				t.Fatal("insufficient space still created an issue run")
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls {
				if strings.Contains(call.Query, "mutation") {
					t.Fatal("capacity refusal mutated Linear")
				}
			}
		})
	}
}
