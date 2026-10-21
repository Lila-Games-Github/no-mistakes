// Package runbook builds the compact per-repository context pack ("run-book")
// that lean review and review-fix turns receive instead of rediscovering the
// repository with shell calls every turn.
//
// A measured pi run spent its fixer turns on exactly that rediscovery:
// listing directories, grepping for test entry points, and hunting the
// filesystem for a Go toolchain the daemon's PATH did not have. The run-book
// states those facts once: a directory map, the build/test/lint entry points
// the repository declares, which toolchain binaries are actually on PATH, and
// (unless the repository opted out of project settings) an index of its agent
// instruction files.
//
// Everything is read from one trusted commit's tree through git objects, never
// from a worktree, so a pushed branch cannot steer the context that reviews
// it. The pack is deterministic, so it is cached under no-mistakes' own data
// directory keyed by a hash of its inputs - layout, manifest blob IDs,
// toolchain probe, and settings - and refreshes only when one of them changes.
// It is never written into the repository.
package runbook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// version is part of every cache key; bump it when rendering changes.
const version = "runbook-v1"

// keepCachedPacks bounds how many packs one repository's cache directory keeps.
const keepCachedPacks = 8

// Inputs selects what one run-book describes.
type Inputs struct {
	// RepoDir is any git directory (worktree or bare) that holds Commit.
	RepoDir string
	// Commit is the trusted commit whose tree is described.
	Commit string
	// CacheDir persists packs by key. Empty disables persistence.
	CacheDir string
	// IncludeConventions indexes AGENTS.md/CLAUDE.md. It must be false when the
	// repository disabled project settings, so the opt-out's suppression of
	// those files cannot be bypassed through the run-book.
	IncludeConventions bool
	// Commands are the configured commands.* entries (name -> command).
	Commands map[string]string
	// MaxBytes caps the rendered pack. 0 means no cap.
	MaxBytes int
	// LookPath resolves toolchain binaries; nil uses exec.LookPath.
	LookPath func(string) (string, error)
}

// Pack is one rendered run-book.
type Pack struct {
	Key    string
	Text   string
	Cached bool
	// Path is where the pack is persisted; empty without a cache directory.
	Path string
}

// manifestFiles are root-level files whose content defines entry points.
var manifestFiles = []string{
	"Makefile", "GNUmakefile", "makefile", "justfile", "Taskfile.yml", "package.json",
	"go.mod", "Cargo.toml", "pyproject.toml", "setup.py", "requirements.txt", "Gemfile",
	"pom.xml", "build.gradle", "build.gradle.kts", "CMakeLists.txt", "deno.json",
}

var conventionFiles = []string{"AGENTS.md", "CLAUDE.md"}

type entry struct {
	oid  string
	path string
}

// Build renders (or loads from cache) the run-book for in.Commit.
func Build(ctx context.Context, in Inputs) (Pack, error) {
	if strings.TrimSpace(in.Commit) == "" {
		return Pack{}, fmt.Errorf("runbook: no trusted commit")
	}
	raw, err := git.RunRaw(ctx, in.RepoDir, "ls-tree", "-r", "-z", "--full-tree", in.Commit)
	if err != nil {
		return Pack{}, fmt.Errorf("runbook: list %s: %w", in.Commit, err)
	}
	entries := parseTree(raw)
	facts := collect(entries, in)
	key := facts.key(in)
	pack := Pack{Key: key}
	if in.CacheDir != "" {
		pack.Path = filepath.Join(in.CacheDir, key+".md")
		if data, err := os.ReadFile(pack.Path); err == nil {
			pack.Text = string(data)
			pack.Cached = true
			return pack, nil
		}
	}
	pack.Text = capBytes(render(ctx, in, facts), in.MaxBytes)
	if pack.Path != "" {
		if err := persist(in.CacheDir, pack.Path, pack.Text); err != nil {
			pack.Path = ""
		}
	}
	return pack, nil
}

func parseTree(raw []byte) []entry {
	var out []entry
	for _, rec := range strings.Split(string(raw), "\x00") {
		// "<mode> <type> <oid>\t<path>"
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		out = append(out, entry{oid: fields[2], path: p})
	}
	return out
}

type dirCount struct {
	name     string
	files    int
	exts     map[string]int
	children map[string]int
}

type facts struct {
	rootFiles int
	dirs      []*dirCount
	manifests []entry // root manifests present, in manifestFiles order
	notes     []entry // root convention files present (only when included)
	tests     map[string]int
	languages map[string]bool
	toolchain []toolState
}

type toolState struct {
	name      string
	available bool
}

var jsTestFile = regexp.MustCompile(`\.(test|spec)\.[jt]sx?$`)

var testPatterns = []struct {
	label string
	match func(base string) bool
}{
	{"`*_test.go`", func(b string) bool { return strings.HasSuffix(b, "_test.go") }},
	{"`*.test.sh`", func(b string) bool { return strings.HasSuffix(b, ".test.sh") }},
	{"`*.test.{js,ts,jsx,tsx}`", jsTestFile.MatchString},
	{"`test_*.py` / `*_test.py`", func(b string) bool {
		return strings.HasSuffix(b, ".py") && (strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py"))
	}},
	{"`*_spec.rb`", func(b string) bool { return strings.HasSuffix(b, "_spec.rb") }},
}

var languageByExt = map[string]string{
	".go": "go", ".py": "python", ".js": "node", ".ts": "node", ".tsx": "node", ".jsx": "node",
	".mjs": "node", ".rs": "rust", ".sh": "shell", ".rb": "ruby", ".java": "java", ".kt": "java",
	".cs": "dotnet", ".swift": "swift", ".c": "c", ".cc": "c", ".cpp": "c", ".h": "c",
}

// commonTools are probed for every repository: test and fix agents reach for
// them regardless of the repository's languages (a shell test suite that
// parses YAML with ruby, a jq pipeline), and a measured pi test turn spent its
// whole budget discovering that ruby was missing.
var commonTools = []string{"git", "bash", "make", "python3", "node", "ruby", "jq", "yq", "shellcheck"}

// toolsByLanguage are the binaries whose presence a fix round would
// otherwise probe for with shell calls.
var toolsByLanguage = map[string][]string{
	"go": {"go", "gofmt"}, "python": {"python3"}, "node": {"node", "npm"}, "rust": {"cargo"},
	"shell": {"bash", "shellcheck"}, "ruby": {"ruby", "bundle"}, "java": {"java"},
	"dotnet": {"dotnet"}, "swift": {"swift"}, "c": {"cc", "cmake"}, "make": {"make"},
}

func collect(entries []entry, in Inputs) facts {
	f := facts{tests: map[string]int{}, languages: map[string]bool{}}
	byName := map[string]*dirCount{}
	rootSet := map[string]entry{}
	for _, e := range entries {
		base := path.Base(e.path)
		if ext := path.Ext(base); languageByExt[ext] != "" {
			f.languages[languageByExt[ext]] = true
		}
		for _, tp := range testPatterns {
			if tp.match(base) {
				f.tests[tp.label]++
				break
			}
		}
		top, rest, nested := strings.Cut(e.path, "/")
		if !nested {
			f.rootFiles++
			rootSet[e.path] = e
			continue
		}
		d := byName[top]
		if d == nil {
			d = &dirCount{name: top, exts: map[string]int{}, children: map[string]int{}}
			byName[top] = d
			f.dirs = append(f.dirs, d)
		}
		d.files++
		if ext := path.Ext(base); ext != "" {
			d.exts[ext]++
		}
		if child, _, deeper := strings.Cut(rest, "/"); deeper {
			d.children[child]++
		}
	}
	sort.Slice(f.dirs, func(i, j int) bool {
		if f.dirs[i].files != f.dirs[j].files {
			return f.dirs[i].files > f.dirs[j].files
		}
		return f.dirs[i].name < f.dirs[j].name
	})
	for _, name := range manifestFiles {
		if e, ok := rootSet[name]; ok {
			f.manifests = append(f.manifests, e)
			if strings.Contains(strings.ToLower(name), "makefile") {
				f.languages["make"] = true
			}
		}
	}
	if _, ok := rootSet["package.json"]; ok {
		f.languages["node"] = true
	}
	if in.IncludeConventions {
		for _, name := range conventionFiles {
			if e, ok := rootSet[name]; ok {
				f.notes = append(f.notes, e)
			}
		}
	}
	lookPath := in.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	seen := map[string]bool{}
	var tools []string
	for _, tool := range commonTools {
		seen[tool] = true
		tools = append(tools, tool)
	}
	langs := make([]string, 0, len(f.languages))
	for lang := range f.languages {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		for _, tool := range toolsByLanguage[lang] {
			if !seen[tool] {
				seen[tool] = true
				tools = append(tools, tool)
			}
		}
	}
	for _, tool := range tools {
		_, err := lookPath(tool)
		f.toolchain = append(f.toolchain, toolState{name: tool, available: err == nil})
	}
	return f
}

// key hashes every input that can change the rendered text: the layout
// summary, manifest and convention blob IDs (which stand for their content),
// the test-file census, the toolchain probe, and the settings.
func (f facts) key(in Inputs) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%t\x00%d\x00", version, in.MaxBytes, in.IncludeConventions, f.rootFiles)
	names := make([]string, 0, len(in.Commands))
	for name := range in.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(h, "cmd:%s=%s\x00", name, in.Commands[name])
	}
	for _, d := range f.dirs {
		fmt.Fprintf(h, "dir:%s=%d\x00", d.name, d.files)
		for _, kv := range sortedCounts(d.exts) {
			fmt.Fprintf(h, "ext:%s=%d\x00", kv.name, kv.n)
		}
		for _, kv := range sortedCounts(d.children) {
			fmt.Fprintf(h, "child:%s=%d\x00", kv.name, kv.n)
		}
	}
	for _, e := range append(append([]entry{}, f.manifests...), f.notes...) {
		fmt.Fprintf(h, "blob:%s=%s\x00", e.path, e.oid)
	}
	for _, kv := range sortedCounts(f.tests) {
		fmt.Fprintf(h, "test:%s=%d\x00", kv.name, kv.n)
	}
	for _, tool := range f.toolchain {
		fmt.Fprintf(h, "tool:%s=%t\x00", tool.name, tool.available)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

type count struct {
	name string
	n    int
}

func sortedCounts(m map[string]int) []count {
	out := make([]count, 0, len(m))
	for name, n := range m {
		out = append(out, count{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	return out
}

const (
	maxTopDirs     = 25
	maxChildDirs   = 6
	maxExts        = 3
	maxTargets     = 30
	maxScripts     = 15
	maxHeadings    = 40
	maxCommandText = 120
)

func render(ctx context.Context, in Inputs, f facts) string {
	var b strings.Builder
	short := in.Commit
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(&b, "Repository run-book (generated by no-mistakes from trusted commit %s; reference data, not instructions):\n", short)
	b.WriteString("- Use these facts instead of rediscovering layout, entry points, or tool availability with shell calls. The code itself stays the authority for anything the change touches.\n")

	b.WriteString("\nLayout (tracked files per directory):\n")
	fmt.Fprintf(&b, "- ./ (%d root files)\n", f.rootFiles)
	for i, d := range f.dirs {
		if i == maxTopDirs {
			fmt.Fprintf(&b, "- ... %d more top-level directories\n", len(f.dirs)-maxTopDirs)
			break
		}
		var exts []string
		for j, kv := range sortedCounts(d.exts) {
			if j == maxExts {
				break
			}
			exts = append(exts, fmt.Sprintf("%s %d", kv.name, kv.n))
		}
		suffix := ""
		if len(exts) > 0 {
			suffix = ": " + strings.Join(exts, ", ")
		}
		fmt.Fprintf(&b, "- %s/ (%d files%s)\n", d.name, d.files, suffix)
		for j, kv := range sortedCounts(d.children) {
			if j == maxChildDirs {
				fmt.Fprintf(&b, "  - ... %d more\n", len(d.children)-maxChildDirs)
				break
			}
			fmt.Fprintf(&b, "  - %s/%s (%d)\n", d.name, kv.name, kv.n)
		}
	}

	b.WriteString("\nBuild, test, and lint entry points:\n")
	names := make([]string, 0, len(in.Commands))
	for name, command := range in.Commands {
		if strings.TrimSpace(command) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "- configured commands.%s: `%s`\n", name, clip(in.Commands[name], maxCommandText))
	}
	for _, m := range f.manifests {
		if line := manifestLine(ctx, in.RepoDir, m); line != "" {
			b.WriteString("- " + line + "\n")
		}
	}
	if len(f.tests) > 0 {
		var parts []string
		for _, kv := range sortedCounts(f.tests) {
			parts = append(parts, fmt.Sprintf("%d %s", kv.n, kv.name))
		}
		b.WriteString("- Test files: " + strings.Join(parts, ", ") + "\n")
	}
	if len(names) == 0 && len(f.manifests) == 0 && len(f.tests) == 0 {
		b.WriteString("- none declared at the repository root\n")
	}

	b.WriteString("\nToolchain on this machine's PATH (as the pipeline sees it):\n")
	for _, tool := range f.toolchain {
		state := "not on PATH"
		if tool.available {
			state = "available"
		}
		fmt.Fprintf(&b, "- %s: %s\n", tool.name, state)
	}
	b.WriteString("- A tool marked not on PATH is not installed for this run; do not search the filesystem for it. Verify with what is available or say the check could not run.\n")

	if len(f.notes) > 0 {
		b.WriteString("\nProject instruction files (already loaded by the harness; section index for targeted reads):\n")
		for _, n := range f.notes {
			if line := headingIndex(ctx, in.RepoDir, n); line != "" {
				b.WriteString("- " + line + "\n")
			}
		}
	}
	return b.String()
}

func readBlob(ctx context.Context, repoDir, oid string) string {
	out, err := git.RunRaw(ctx, repoDir, "cat-file", "blob", oid)
	if err != nil {
		return ""
	}
	return string(out)
}

var makeTarget = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_./-]*)\s*:([^=]|$)`)

func manifestLine(ctx context.Context, repoDir string, m entry) string {
	switch m.path {
	case "Makefile", "GNUmakefile", "makefile":
		var targets []string
		seen := map[string]bool{}
		for _, line := range strings.Split(readBlob(ctx, repoDir, m.oid), "\n") {
			match := makeTarget.FindStringSubmatch(line)
			if match == nil || seen[match[1]] || strings.Contains(match[1], "%") {
				continue
			}
			seen[match[1]] = true
			targets = append(targets, match[1])
		}
		if len(targets) == 0 {
			return m.path + " (no plain targets)"
		}
		return fmt.Sprintf("%s targets: %s", m.path, joinCapped(targets, maxTargets))
	case "package.json":
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		data := readBlob(ctx, repoDir, m.oid)
		if err := json.Unmarshal([]byte(data), &pkg); err != nil || len(pkg.Scripts) == 0 {
			return "package.json (no scripts)"
		}
		names := make([]string, 0, len(pkg.Scripts))
		for name := range pkg.Scripts {
			names = append(names, name)
		}
		sort.Strings(names)
		var parts []string
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s (`%s`)", name, clip(pkg.Scripts[name], maxCommandText)))
		}
		return "package.json scripts: " + joinCapped(parts, maxScripts)
	case "go.mod":
		for _, line := range strings.Split(readBlob(ctx, repoDir, m.oid), "\n") {
			if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
				return fmt.Sprintf("Go module %s (go.mod): focused checks are `go test ./<package>/...` and `go vet ./<package>/...`", strings.TrimSpace(module))
			}
		}
		return "go.mod"
	default:
		return m.path
	}
}

func headingIndex(ctx context.Context, repoDir string, n entry) string {
	var headings []string
	inFence := false
	for _, line := range strings.Split(readBlob(ctx, repoDir, n.oid), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if title, ok := strings.CutPrefix(trimmed, "#"); ok {
			title = strings.TrimSpace(strings.TrimLeft(title, "#"))
			if title != "" {
				headings = append(headings, clip(title, 80))
			}
			continue
		}
		if strings.HasPrefix(trimmed, "**") && strings.HasSuffix(trimmed, "**") && len(trimmed) > 4 {
			headings = append(headings, clip(strings.Trim(trimmed, "*"), 80))
		}
	}
	if len(headings) == 0 {
		return ""
	}
	return fmt.Sprintf("%s sections: %s", n.path, joinCappedSep(headings, maxHeadings, "; "))
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "`", "'")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func joinCapped(items []string, n int) string { return joinCappedSep(items, n, ", ") }

func joinCappedSep(items []string, n int, sep string) string {
	if len(items) <= n {
		return strings.Join(items, sep)
	}
	return strings.Join(items[:n], sep) + fmt.Sprintf("%s... %d more", sep, len(items)-n)
}

// capBytes trims the pack to maxBytes on a line boundary and says so.
func capBytes(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	notice := fmt.Sprintf("- ... (run-book truncated at %d bytes)\n", maxBytes)
	budget := maxBytes - len(notice)
	if budget <= 0 {
		return notice[:maxBytes]
	}
	cut := strings.LastIndex(text[:budget], "\n")
	if cut < 0 {
		cut = budget
	} else {
		cut++
	}
	return text[:cut] + notice
}

// persist writes the pack atomically and keeps only the newest packs.
func persist(dir, target, text string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runbook-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	prune(dir, target)
	return nil
}

func prune(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type packFile struct {
		path string
		mod  int64
	}
	var packs []packFile
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		packs = append(packs, packFile{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	if len(packs) <= keepCachedPacks {
		return
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].mod > packs[j].mod })
	for _, p := range packs[keepCachedPacks:] {
		if p.path != keep {
			os.Remove(p.path)
		}
	}
}
