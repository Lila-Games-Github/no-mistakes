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
	foundSwitch := false
	for _, inv := range h.AgentInvocations() {
		if !strings.Contains(inv.Prompt, reviewStepPromptMarker) {
			continue
		}
		for i := 0; i+1 < len(inv.Args); i++ {
			if inv.Args[i] == "--setting-sources" && inv.Args[i+1] == "user" {
				foundSwitch = true
			}
		}
	}
	if !foundSwitch {
		t.Fatal("review invocation did not suppress repository instruction loading")
	}
}
