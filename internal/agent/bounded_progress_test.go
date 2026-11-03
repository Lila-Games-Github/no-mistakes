package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBoundedWork_RepeatedVerificationAfterIndependentRepairs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	dir := t.TempDir()
	calls, commands := 0, 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "Apply three independent repairs and run the same focused verification after each. Return only after the third passes.", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(call RunOpts) (*Result, error) {
		calls++
		if calls == 7 {
			return boundedTestResult(boundedAction{Checkpoint: "three repairs independently verified", Output: "all three repairs verified"}), nil
		}
		command := "test -s repairs.txt"
		if calls%2 == 1 {
			command = fmt.Sprintf("printf 'authorized repair %d\\n' >> repairs.txt", 1+calls/2)
		}
		return boundedTestResult(boundedAction{Checkpoint: strings.Repeat("repair progress ", calls), Command: command}), nil
	}, func(command string) (string, error) {
		commands++
		return runBoundedCommand(context.Background(), dir, gitSafeEnv(dir), command)
	})
	contents, readErr := os.ReadFile(filepath.Join(dir, "repairs.txt"))
	if readErr != nil || strings.Count(string(contents), "authorized repair") != 3 {
		t.Fatalf("fixture did not make real progress: %q, %v", contents, readErr)
	}
	if err != nil || calls != 7 || commands != 6 || result == nil || result.Text != "all three repairs verified" {
		t.Fatalf("progressing repair falsely rejected: calls=%d commands=%d result=%+v err=%v", calls, commands, result, err)
	}
	if result.PeakContextTokens != 30000 || result.Usage.InputTokens+result.Usage.CacheReadTokens+result.Usage.CacheCreationTokens != 210000 {
		t.Fatalf("progressing repair lost per-call or aggregate usage: %+v", result)
	}
}

func TestBoundedWork_FinalAttemptRetainsStructuredFindings(t *testing.T) {
	final := `{"summary":"complete review","findings":[{"id":"material-source-bug"}]}`
	var last Attempt
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review all assigned scope", JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"findings":{"type":"array"}},"required":["summary","findings"]}`), MaxInputTokens: 150000, MaxTotalInputTokens: 1500000, OnAttempt: func(a Attempt) { last = a }}, func(RunOpts) (*Result, error) {
		return boundedTestResult(boundedAction{Checkpoint: "all assigned scope checked", Output: final}), nil
	}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
	if err != nil || string(result.Output) != final {
		t.Fatalf("fixture failed: result=%+v err=%v", result, err)
	}
	if last.Result == nil {
		t.Fatal("successful final attempt had no result")
	}
	if string(last.Result.Output) != final {
		t.Fatalf("successful final attempt lost findings for the invocation recorder: output=%q", last.Result.Output)
	}
}
