//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPreparedContextBriefJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", ContextBrief: true})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "prepared-context"
	h.CommitChange(branch, "feature.go", "package feature\nfunc New() int { return 1 }\n", "add feature")
	h.PushToGate(branch)
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusCompleted, 30*time.Second)
	prompt := reviewPrompt(t, h)
	for _, want := range []string{"Prepared change context", "feature.go", "return 1", "Running log (outcomes and decisions only)"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("review prompt omitted %q", want)
		}
	}
	foundReview := false
	for _, inv := range h.AgentInvocations() {
		if !strings.Contains(inv.Prompt, reviewStepPromptMarker) {
			continue
		}
		foundReview = true
		safe, oneTurn, toolFree := false, false, false
		for i, arg := range inv.Args {
			if arg == "--safe-mode" {
				safe = true
			}
			if i+1 < len(inv.Args) {
				oneTurn = oneTurn || arg == "--max-turns" && inv.Args[i+1] == "1"
				toolFree = toolFree || arg == "--tools" && inv.Args[i+1] == ""
			}
			if arg == "--resume" || arg == "--json-schema" {
				t.Fatalf("bounded review used a native session or output tool: %v", inv.Args)
			}
		}
		if !safe || !oneTurn || !toolFree {
			t.Fatalf("review omitted safe cold-call controls: %v", inv.Args)
		}
	}
	if !foundReview {
		t.Fatal("no bounded review invocation recorded")
	}
}
