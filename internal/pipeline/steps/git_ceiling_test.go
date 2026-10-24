package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A child test binary simulates validation with TMPDIR under a checkout.
// Its TestMain must install the ceiling before any Git fixture command runs.
func TestGitDiscoveryStopsAtValidationTempRoot(t *testing.T) {
	if os.Getenv("NM_TEST_GIT_CEILING_CHILD") == "1" {
		fixture := filepath.Join(os.TempDir(), "empty-fixture")
		if err := os.MkdirAll(fixture, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "rev-parse", "--show-toplevel")
		cmd.Dir = fixture
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "not a git repository") {
			t.Fatalf("empty fixture Git discovery was not stopped by the ceiling: %v: %s", err, out)
		}
		return
	}

	checkout := t.TempDir()
	if out, err := exec.Command("git", "-C", checkout, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	tempRoot := filepath.Join(checkout, "validation-temp")
	if err := os.Mkdir(tempRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(tempRoot, "empty-fixture")
	if err := os.Mkdir(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	// Prove this layout would discover the containing checkout without a
	// ceiling; the child below must reject the same discovery.
	probe := exec.Command("git", "rev-parse", "--show-toplevel")
	probe.Dir = fixture
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_CEILING_DIRECTORIES=") {
			probe.Env = append(probe.Env, entry)
		}
	}
	if out, err := probe.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != checkout {
		t.Fatalf("unsafe fixture did not expose ancestor checkout: %v: %s", err, out)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestGitDiscoveryStopsAtValidationTempRoot$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TMPDIR=") && !strings.HasPrefix(entry, "GIT_CEILING_DIRECTORIES=") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "TMPDIR="+tempRoot, "NM_TEST_GIT_CEILING_CHILD=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("validation subprocess: %v: %s", err, out)
	}
}
