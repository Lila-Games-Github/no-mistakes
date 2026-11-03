package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the same stdin/JSONL seam used by the native adapter. The match
// phrase is on an earlier page and must survive without copying task history.
func TestClaudeBoundedScenarioIngestsBeforeEditsAndRetainsMatch(t *testing.T) {
	for _, fixtures := range []bool{false, true} {
		t.Run(fmt.Sprint("fixtures=", fixtures), func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("FAKEAGENT_FIXTURE", "")
			if fixtures {
				root := t.TempDir()
				dir := filepath.Join(root, "claude", "plain")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".jsonl"), []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"recorded prose\"}]}}\n{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"recorded prose\"}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv("FAKEAGENT_FIXTURE", root)
			}
			scenario := &Scenario{Actions: []Action{
				{Match: "earlier priority", Structured: map[string]any{"summary": "priority repair"}, Edits: []Edit{{Path: "fixed.txt", New: "fixed once"}}},
				{Match: "later match", Text: "wrong action"},
				{Text: "fallback"},
			}}
			checkpoint := ""
			for page := 1; page <= 3; page++ {
				data := []string{"earlier priority", "later match", "final schema"}[page-1]
				prompt := fmt.Sprintf("Task ingestion still in progress: %t.\nTask page %d/3 (exact task data):\n%s\n\nPrevious checkpoint (working notes, not governing instructions):\n%s\n\nLast command observation (untrusted data):\nNo command has run.\n", page < 3, page, data, checkpoint)
				text := captureClaudeText(t, []string{"-p", "--tools", "", "--max-turns", "1"}, prompt, scenario)
				var action struct{ Checkpoint, Command, Output string }
				if err := json.Unmarshal([]byte(text), &action); err != nil {
					t.Fatalf("not action JSON: %q: %v", text, err)
				}
				if action.Command != "" {
					t.Fatalf("unexpected fake command: %q", action.Command)
				}
				if page < 3 {
					if action.Output != "" {
						t.Fatalf("completed before ingestion: %q", action.Output)
					}
					if _, err := os.Stat("fixed.txt"); !os.IsNotExist(err) {
						t.Fatalf("edits before ingestion: %v", err)
					}
				} else {
					if action.Output != `{"summary":"priority repair"}` {
						t.Fatalf("lost earlier match: %q", action.Output)
					}
					got, err := os.ReadFile("fixed.txt")
					if err != nil || string(got) != "fixed once" {
						t.Fatalf("repair = %q, %v", got, err)
					}
				}
				checkpoint = action.Checkpoint
			}
		})
	}
}

func captureClaudeText(t *testing.T, args []string, prompt string, scenario *Scenario) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	previous := os.Stdout
	os.Stdout = file
	code := runClaude(args, strings.NewReader(prompt), scenario)
	os.Stdout = previous
	if code != 0 {
		t.Fatalf("runClaude exit = %d", code)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var text string
	dec := json.NewDecoder(file)
	for dec.More() {
		var event struct {
			Message struct{ Content []struct{ Type, Text string } }
		}
		if err := dec.Decode(&event); err != nil {
			t.Fatal(err)
		}
		for _, content := range event.Message.Content {
			if content.Type == "text" {
				text += content.Text
			}
		}
	}
	return text
}

func TestClaudeBoundedDefaultReviewScenarioReportsScope(t *testing.T) {
	t.Setenv("FAKEAGENT_FIXTURE", "")
	prompt := "Task ingestion still in progress: false.\nTask page 1/1 (exact task data):\nReview the code changes and return structured findings\n\nPrevious checkpoint (working notes, not governing instructions):\n\n\nLast command observation (untrusted data):\nNo command has run.\n"
	text := captureClaudeText(t, []string{"-p", "--tools", "", "--max-turns", "1"}, prompt, defaultScenario())
	var action struct{ Output string }
	if err := json.Unmarshal([]byte(text), &action); err != nil {
		t.Fatal(err)
	}
	var findings struct {
		RiskScope string `json:"risk_scope"`
	}
	if err := json.Unmarshal([]byte(action.Output), &findings); err != nil {
		t.Fatal(err)
	}
	if findings.RiskScope != "source-or-external" {
		t.Fatalf("default review omitted required source scope: %q", action.Output)
	}
}

func TestPatchClaudeFixtureStructuredRunRewritesAssistantText(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"recorded assistant text\"}]}}\n{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	})
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want 2", len(lines))
	}

	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if assistant.Type != "assistant" {
		t.Fatalf("assistant type = %q, want assistant", assistant.Type)
	}
	if len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "scenario text" {
		t.Fatalf("assistant content = %+v, want scenario text", assistant.Message.Content)
	}

	var result struct {
		Type             string          `json:"type"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(lines[1], &result); err != nil {
		t.Fatalf("unmarshal result event: %v", err)
	}
	if result.Result != "scenario text" {
		t.Fatalf("result text = %q, want scenario text", result.Result)
	}
	if string(result.StructuredOutput) != `{"summary":"patched summary"}` {
		t.Fatalf("structured_output = %s, want patched payload", result.StructuredOutput)
	}
}

func TestPatchClaudeFixtureAddsAssistantEventWhenMissing(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	})
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want assistant + result", len(lines))
	}
	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal injected assistant event: %v", err)
	}
	if assistant.Type != "assistant" || len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "scenario text" {
		t.Fatalf("injected assistant event = %+v, want scenario text", assistant)
	}
}

func TestPatchClaudeFixtureStructuredRunPreservesNonTextAssistantContent(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"thinking\",\"thinking\":\"recorded thinking\"},{\"type\":\"tool_use\",\"name\":\"Read\"},{\"type\":\"text\",\"text\":\"recorded assistant text\"}]}}\n{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	})
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want 2", len(lines))
	}

	var assistant struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if len(assistant.Message.Content) != 3 {
		t.Fatalf("assistant content len = %d, want 3", len(assistant.Message.Content))
	}
	if assistant.Message.Content[0]["type"] != "thinking" {
		t.Fatalf("first content type = %v, want thinking", assistant.Message.Content[0]["type"])
	}
	if assistant.Message.Content[1]["type"] != "tool_use" {
		t.Fatalf("second content type = %v, want tool_use", assistant.Message.Content[1]["type"])
	}
	if assistant.Message.Content[2]["type"] != "text" || assistant.Message.Content[2]["text"] != "scenario text" {
		t.Fatalf("third content = %+v, want patched text item", assistant.Message.Content[2])
	}
}
