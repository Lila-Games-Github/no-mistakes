package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var reaskTestSchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"risk_scope":{"type":"string","enum":["source","source-or-external"]}},"required":["summary","risk_scope"],"additionalProperties":false}`)

func TestFinalizeTextResult_RepairsMechanicalDefectsLocally(t *testing.T) {
	// A raw newline inside a string and a trailing comma: both are fixable
	// without guessing any content, so no re-ask is spent on them.
	text := "{\"summary\":\"line one\nline two\",\"risk_scope\":\"source\",}"
	result, err := finalizeTextResult("pi", text, reaskTestSchema, TokenUsage{})
	if err != nil {
		t.Fatalf("mechanically repairable output failed: %v", err)
	}
	if !result.StructuredOutputRepaired {
		t.Fatal("repaired output not reported as repaired")
	}
	var got struct{ Summary, RiskScope string }
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary != "line one\nline two" {
		t.Fatalf("repair changed content: summary = %q", got.Summary)
	}
}

func TestFinalizeTextResult_WellFormedOutputIsNotMarkedRepaired(t *testing.T) {
	result, err := finalizeTextResult("pi", `{"summary":"ok","risk_scope":"source"}`, reaskTestSchema, TokenUsage{})
	if err != nil {
		t.Fatal(err)
	}
	if result.StructuredOutputRepaired {
		t.Fatal("well-formed output reported as repaired")
	}
}

func TestFinalizeTextResult_UnescapedQuoteIsLeftForTheReask(t *testing.T) {
	// The live pi failure: an unescaped quote inside a description. Repairing
	// it means guessing where the string ends, so it must surface as a typed
	// error carrying the rejected text instead.
	text := `{"summary":"drops ["failed","recalled"] lanes","risk_scope":"source"}`
	_, err := finalizeTextResult("pi", text, reaskTestSchema, TokenUsage{})
	var soErr *StructuredOutputError
	if !errors.As(err, &soErr) {
		t.Fatalf("err = %v, want *StructuredOutputError", err)
	}
	if soErr.Text != text || soErr.Agent != "pi" {
		t.Fatalf("structured output error = %+v", soErr)
	}
	if !strings.Contains(err.Error(), "pi output parse: invalid character 'f' after object key:value pair") {
		t.Fatalf("error text changed shape: %v", err)
	}
}

func TestRunWithStructuredOutputReask_LeavesOtherFailuresAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx    func() context.Context
		err    error
		schema json.RawMessage
	}{
		"non-structured error": {ctx: context.Background, err: errors.New("pi exited: exit status 1"), schema: reaskTestSchema},
		"no schema requested":  {ctx: context.Background, err: &StructuredOutputError{Agent: "pi", Err: errors.New("bad")}},
		"cancelled": {ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, err: &StructuredOutputError{Agent: "pi", Err: errors.New("bad")}, schema: reaskTestSchema},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			_, err := runWithStructuredOutputReask(tc.ctx(), "pi", RunOpts{JSONSchema: tc.schema}, func(RunOpts) (*Result, error) {
				calls++
				return nil, tc.err
			})
			if calls != 1 || err != tc.err {
				t.Fatalf("calls = %d, err = %v; want exactly one call returning the original error", calls, err)
			}
		})
	}
}

// writeSequencedFakePi installs a fake pi that answers its first invocation
// with first and every later one with second, recording each invocation's
// argv and stdin prompt as argv-<n>.txt and prompt-<n>.txt in the run CWD.
func writeSequencedFakePi(t *testing.T, first, second []string) string {
	t.Helper()
	posix := []string{
		"#!/bin/sh",
		"n=1",
		"[ -f call-1 ] && n=2",
		": > call-$n",
		"cat > prompt-$n.txt",
		`printf '%s\n' "$*" > argv-$n.txt`,
		`if [ "$n" = 1 ]; then`,
	}
	for _, line := range first {
		posix = append(posix, "printf '%s\\n' '"+line+"'")
	}
	posix = append(posix, "else")
	for _, line := range second {
		posix = append(posix, "printf '%s\\n' '"+line+"'")
	}
	posix = append(posix, "fi", "")
	windows := []string{
		"@echo off",
		"if exist call-1 (set n=2) else (set n=1)",
		"echo x> call-%n%",
		"more > prompt-%n%.txt",
		"echo %* > argv-%n%.txt",
		"if %n%==2 goto second",
	}
	for _, line := range first {
		windows = append(windows, "echo "+line)
	}
	windows = append(windows, "exit /b 0", ":second")
	for _, line := range second {
		windows = append(windows, "echo "+line)
	}
	return writeFakePi(t, t.TempDir(), strings.Join(posix, "\n"), strings.Join(windows, "\r\n"))
}

// piFinalTextEvents renders the JSONL a pi turn emits for one final assistant
// text, optionally preceded by the durable-session header.
func piFinalTextEvents(t *testing.T, sessionID, text string) []string {
	t.Helper()
	var lines []string
	if sessionID != "" {
		header, err := json.Marshal(map[string]any{"type": "session", "version": 3, "id": sessionID})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(header))
	}
	end, err := json.Marshal(map[string]any{
		"type":     "agent_end",
		"messages": []any{map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": text}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(lines, string(end))
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPiAgent_SchemaMissIsFixedByOneColdReask(t *testing.T) {
	workDir := t.TempDir()
	missing := `{"summary":"one finding"}`
	bin := writeSequencedFakePi(t,
		piFinalTextEvents(t, "", missing),
		piFinalTextEvents(t, "", `{"summary":"one finding","risk_scope":"source"}`))

	var attempts []Attempt
	var lifecycle []string
	result, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:      "review the change",
		CWD:         workDir,
		JSONSchema:  reaskTestSchema,
		OnAttempt:   func(a Attempt) { attempts = append(attempts, a) },
		OnLifecycle: func(e LifecycleEvent) { lifecycle = append(lifecycle, e.Message) },
	})
	if err != nil {
		t.Fatalf("schema miss was not recovered by the re-ask: %v", err)
	}
	if string(result.Output) != `{"summary":"one finding","risk_scope":"source"}` {
		t.Fatalf("output = %s", result.Output)
	}
	if result.StructuredOutputReasks != 1 {
		t.Fatalf("StructuredOutputReasks = %d, want 1", result.StructuredOutputReasks)
	}
	if len(attempts) != 2 || attempts[0].StructuredOutputReask || !attempts[1].StructuredOutputReask {
		t.Fatalf("attempts = %+v, want the original then one flagged re-ask", attempts)
	}
	var soErr *StructuredOutputError
	if !errors.As(attempts[0].Err, &soErr) {
		t.Fatalf("first attempt err = %v, want the structured output failure", attempts[0].Err)
	}
	if !strings.Contains(strings.Join(lifecycle, "\n"), "re-asking once") {
		t.Fatalf("re-ask not logged: %q", lifecycle)
	}

	reask := readTestFile(t, filepath.Join(workDir, "prompt-2.txt"))
	for _, want := range []string{
		`missing required field "risk_scope"`, // the exact validation error
		missing,                               // a cold re-ask hands back its own response
		`"risk_scope"`,                        // and the required schema
		"Do not run tools",
	} {
		if !strings.Contains(reask, want) {
			t.Errorf("re-ask prompt missing %q:\n%s", want, reask)
		}
	}
	if strings.Contains(reask, "review the change") {
		t.Error("cold re-ask replayed the original task instead of asking for a reformat")
	}
	if argv := strings.TrimSpace(readTestFile(t, filepath.Join(workDir, "argv-2.txt"))); argv != "--mode json --no-session" {
		t.Fatalf("cold re-ask argv = %q", argv)
	}
}

func TestPiAgent_ReaskResumesTheDurableSession(t *testing.T) {
	const sessionID = "019ff2f3-5f31-744b-90b8-679074ff7686"
	workDir := t.TempDir()
	bin := writeSequencedFakePi(t,
		piFinalTextEvents(t, sessionID, `{"summary":"drops ["failed"] lanes","risk_scope":"source"}`),
		piFinalTextEvents(t, sessionID, `{"summary":"drops [\"failed\"] lanes","risk_scope":"source"}`))

	result, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt: "fix the findings", CWD: workDir, JSONSchema: reaskTestSchema, Session: &SessionRef{},
	})
	if err != nil {
		t.Fatalf("re-ask in the durable session failed: %v", err)
	}
	if result.SessionID != sessionID || result.Resumed {
		t.Fatalf("result session = %q resumed=%v, want the started session %q", result.SessionID, result.Resumed, sessionID)
	}
	if argv := strings.TrimSpace(readTestFile(t, filepath.Join(workDir, "argv-2.txt"))); argv != "--mode json --session "+sessionID {
		t.Fatalf("re-ask argv = %q, want a resume of the same session", argv)
	}
	reask := readTestFile(t, filepath.Join(workDir, "prompt-2.txt"))
	if !strings.Contains(reask, "invalid character 'f' after object key:value pair") {
		t.Fatalf("re-ask prompt does not carry the validation error:\n%s", reask)
	}
	if strings.Contains(reask, "BEGIN PREVIOUS RESPONSE") {
		t.Fatal("resumed re-ask resent a response the session already holds")
	}
}

func TestPiAgent_DoubleStructuredOutputFailureFailsCleanly(t *testing.T) {
	workDir := t.TempDir()
	bad := piFinalTextEvents(t, "", `{"summary":"still missing a field"}`)
	bin := writeSequencedFakePi(t, bad, bad)

	var attempts []Attempt
	result, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt: "review", CWD: workDir, JSONSchema: reaskTestSchema,
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) },
	})
	if err == nil || result != nil {
		t.Fatalf("double failure returned result=%+v err=%v, want a clean failure", result, err)
	}
	var soErr *StructuredOutputError
	if !errors.As(err, &soErr) {
		t.Fatalf("err = %v, want the re-ask's structured output failure", err)
	}
	if !strings.Contains(err.Error(), "pi output parse") || !strings.Contains(err.Error(), "after one structured-output re-ask") {
		t.Fatalf("err = %v, want a parse failure that names the re-ask", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want exactly one re-ask", len(attempts))
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "call-2")); statErr != nil {
		t.Fatal("re-ask never ran")
	}
}

// TestCodexAgent_StructuredOutputFailureIsReaskedInItsThread: codex keeps its
// behavior except that a rejected structured answer earns the same single
// re-ask, resuming the thread the failed turn ran in.
func TestCodexAgent_StructuredOutputFailureIsReaskedInItsThread(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	const threadID = "thread-7"
	workDir := t.TempDir()
	bin := writeFakeCodex(t, t.TempDir(), `#!/bin/sh
n=1
[ -f call-1 ] && n=2
: > call-$n
cat > prompt-$n.txt
printf '%s\n' "$*" > argv-$n.txt
printf '%s\n' '{"type":"thread.started","thread_id":"`+threadID+`"}'
if [ "$n" = 1 ]; then
	printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"summary\":\"no scope\"}"}}'
else
	printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"summary\":\"no scope\",\"risk_scope\":\"source\"}"}}'
fi
`, "")

	result, err := (&codexAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt: "fix", CWD: workDir, JSONSchema: reaskTestSchema, Session: &SessionRef{},
	})
	if err != nil {
		t.Fatalf("codex re-ask failed: %v", err)
	}
	if result.StructuredOutputReasks != 1 || result.SessionID != threadID || result.Resumed {
		t.Fatalf("result = %+v, want one re-ask reported against the started thread", result)
	}
	argv := readTestFile(t, filepath.Join(workDir, "argv-2.txt"))
	if !strings.HasPrefix(argv, "exec resume "+threadID+" - --json") {
		t.Fatalf("codex re-ask argv = %q, want a resume of the failed turn's thread", argv)
	}
	if _, err := os.Stat(filepath.Join(workDir, "call-3")); err == nil {
		t.Fatal("codex was asked more than once after the failure")
	}
}
