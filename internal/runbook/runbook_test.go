package runbook

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	os.Exit(m.Run())
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testRepo builds a small multi-language repository and returns its directory
// and HEAD commit.
func testRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "Makefile", ".PHONY: build test\nbuild:\n\tgo build ./...\ntest: build\n\tgo test ./...\nlint:\n\tgo vet ./...\n")
	writeFile(t, dir, "go.mod", "module example.com/demo\n\ngo 1.24\n")
	writeFile(t, dir, "package.json", `{"name":"demo","scripts":{"test":"node --test","fmt":"prettier -w ."}}`)
	writeFile(t, dir, "AGENTS.md", "# Agents\n\n## Testing\n\nRun focused tests.\n\n**Never** edit generated files.\n\n## Release\n")
	writeFile(t, dir, "internal/core/core.go", "package core\n")
	writeFile(t, dir, "internal/core/core_test.go", "package core\n")
	writeFile(t, dir, "internal/cli/cli.go", "package cli\n")
	writeFile(t, dir, "bin/tool.sh", "#!/bin/sh\n")
	writeFile(t, dir, "tests/tool.test.sh", "#!/bin/sh\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "init")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

func fakeLookPath(available ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, a := range available {
			if a == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
}

func TestBuildDescribesLayoutCommandsAndToolchain(t *testing.T) {
	dir, head := testRepo(t)
	pack, err := Build(context.Background(), Inputs{
		RepoDir:            dir,
		Commit:             head,
		IncludeConventions: true,
		Commands:           map[string]string{"test": "make test"},
		MaxBytes:           6000,
		LookPath:           fakeLookPath("go", "git", "make"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"internal/ (3 files",
		"internal/core (2)",
		"commands.test: `make test`",
		"Makefile targets: build, test, lint",
		"package.json scripts: fmt (`prettier -w .`), test (`node --test`)",
		"Go module example.com/demo",
		"1 `*_test.go`",
		"1 `*.test.sh`",
		"- go: available",
		"- node: not on PATH",
		"- shellcheck: not on PATH",
		"- ruby: not on PATH",
		"AGENTS.md sections: Agents; Testing; Release",
	} {
		if !strings.Contains(pack.Text, want) {
			t.Errorf("run-book missing %q:\n%s", want, pack.Text)
		}
	}
	if pack.Cached {
		t.Fatal("a pack built without a cache directory cannot be a cache hit")
	}
}

func TestBuildOmitsProjectInstructionsUnderTheOptOut(t *testing.T) {
	dir, head := testRepo(t)
	pack, err := Build(context.Background(), Inputs{RepoDir: dir, Commit: head, IncludeConventions: false, MaxBytes: 6000, LookPath: fakeLookPath()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pack.Text, "AGENTS.md") || strings.Contains(pack.Text, "Testing") {
		t.Fatalf("disable_project_settings must keep AGENTS.md content out of the run-book:\n%s", pack.Text)
	}
}

func TestBuildReadsTheTrustedCommitNotTheWorktree(t *testing.T) {
	dir, head := testRepo(t)
	writeFile(t, dir, "Makefile", "pwned:\n\ttrue\n")
	writeFile(t, dir, "internal/extra/x.go", "package extra\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "pushed change")
	pack, err := Build(context.Background(), Inputs{RepoDir: dir, Commit: head, MaxBytes: 6000, LookPath: fakeLookPath()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pack.Text, "pwned") || strings.Contains(pack.Text, "internal/extra") {
		t.Fatalf("run-book must come from the trusted commit only:\n%s", pack.Text)
	}
}

func TestBuildCachesByContentAndRefreshesWhenInputsChange(t *testing.T) {
	dir, head := testRepo(t)
	cache := filepath.Join(t.TempDir(), "runbooks")
	in := Inputs{RepoDir: dir, Commit: head, CacheDir: cache, MaxBytes: 6000, LookPath: fakeLookPath("go")}
	first, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached || first.Path == "" {
		t.Fatalf("first build = %+v, want a fresh persisted pack", first)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("pack not persisted: %v", err)
	}

	// An unrelated commit (no layout, manifest, or toolchain change) reuses
	// the same pack.
	writeFile(t, dir, "internal/core/core.go", "package core\n\nconst X = 1\n")
	gitRun(t, dir, "commit", "-qam", "edit a source file")
	second, err := Build(context.Background(), Inputs{RepoDir: dir, Commit: gitRun(t, dir, "rev-parse", "HEAD"), CacheDir: cache, MaxBytes: 6000, LookPath: fakeLookPath("go")})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached || second.Key != first.Key {
		t.Fatalf("second build = %+v, want a cache hit on key %s", second, first.Key)
	}

	// A manifest change refreshes it.
	writeFile(t, dir, "Makefile", "e2e:\n\tgo test -tags e2e ./...\n")
	gitRun(t, dir, "commit", "-qam", "new target")
	third, err := Build(context.Background(), Inputs{RepoDir: dir, Commit: gitRun(t, dir, "rev-parse", "HEAD"), CacheDir: cache, MaxBytes: 6000, LookPath: fakeLookPath("go")})
	if err != nil {
		t.Fatal(err)
	}
	if third.Cached || third.Key == first.Key || !strings.Contains(third.Text, "e2e") {
		t.Fatalf("third build = %+v, want a refreshed pack", third)
	}

	// A toolchain change refreshes it too.
	fourth, err := Build(context.Background(), Inputs{RepoDir: dir, Commit: gitRun(t, dir, "rev-parse", "HEAD"), CacheDir: cache, MaxBytes: 6000, LookPath: fakeLookPath()})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Cached || fourth.Key == third.Key || !strings.Contains(fourth.Text, "- go: not on PATH") {
		t.Fatalf("fourth build = %+v, want a refreshed pack after the toolchain changed", fourth)
	}
}

func TestBuildHonorsTheByteCap(t *testing.T) {
	dir, _ := testRepo(t)
	for i := 0; i < 200; i++ {
		writeFile(t, dir, filepath.Join("pkg", "dir"+strings.Repeat("x", i%7)+string(rune('a'+i%26)), "f.go"), "package p\n")
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "many dirs")
	pack, err := Build(context.Background(), Inputs{RepoDir: dir, Commit: gitRun(t, dir, "rev-parse", "HEAD"), MaxBytes: 900, LookPath: fakeLookPath()})
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Text) > 900 {
		t.Fatalf("pack is %d bytes, cap 900", len(pack.Text))
	}
	if !strings.Contains(pack.Text, "truncated") {
		t.Fatalf("a capped pack must say it was truncated:\n%s", pack.Text)
	}
}
