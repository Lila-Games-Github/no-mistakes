package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runmetrics"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMetricsExportsLegacyRunReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	d, err := db.Open(filepath.Join(home, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/tmp/private", "https://example.com/private", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(s.ID, 0, 22605, ""); err != nil {
		t.Fatal(err)
	}
	d.Close()
	out, err := executeCmd("metrics", run.ID, "--json")
	if err != nil {
		t.Fatalf("metrics: %v %s", err, out)
	}
	var a runmetrics.Artifact
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	if a.SchemaVersion != 1 || a.RunID != run.ID || *a.Phases[0].Timing.ExecutionMS != 22605 || a.JournalPresent {
		t.Fatalf("artifact: %+v", a)
	}
	out, err = executeCmd("metrics", run.ID)
	if err != nil || !strings.Contains(out, "review 22.6s") {
		t.Fatalf("summary: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "logs")); !os.IsNotExist(err) {
		t.Fatalf("read-only export created logs: %v", err)
	}
}

func TestMetricsRejectsPathAndMissingRunWithoutCreatingState(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv("NM_HOME", home)
	for _, arg := range []string{"../../elsewhere", "01M3K2DF487NMDMRYQ09438557"} {
		if _, err := executeCmd("metrics", arg, "--json"); err == nil {
			t.Fatalf("expected error for %q", arg)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("command created state: %v", err)
	}
}
