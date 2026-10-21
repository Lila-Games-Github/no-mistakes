package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplayEnvKeepsHarnessConfigReachableUnderIsolatedHome(t *testing.T) {
	realHome := t.TempDir()
	for _, dir := range []string{".claude", ".codex", filepath.Join(".pi", "agent")} {
		if err := os.MkdirAll(filepath.Join(realHome, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", realHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")

	env := replayEnv("/sandbox/nmhome", "/sandbox/home")
	got := map[string]string{}
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				got[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	want := map[string]string{
		"NM_HOME":             "/sandbox/nmhome",
		"HOME":                "/sandbox/home",
		"CLAUDE_CONFIG_DIR":   filepath.Join(realHome, ".claude"),
		"CODEX_HOME":          filepath.Join(realHome, ".codex"),
		"PI_CODING_AGENT_DIR": filepath.Join(realHome, ".pi", "agent"),
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q (env %v)", key, got[key], value, env)
		}
	}
}

func TestReplayEnvRespectsExplicitHarnessConfigAndSkipsMissingDirs(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "/operator/claude")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")

	env := replayEnv("/sandbox/nmhome", "/sandbox/home")
	for _, kv := range env {
		switch {
		case kv == "CLAUDE_CONFIG_DIR=/operator/claude":
		case len(kv) > len("CLAUDE_CONFIG_DIR=") && kv[:len("CLAUDE_CONFIG_DIR=")] == "CLAUDE_CONFIG_DIR=":
			t.Errorf("explicit CLAUDE_CONFIG_DIR was replaced: %q", kv)
		case len(kv) > len("CODEX_HOME=") && kv[:len("CODEX_HOME=")] == "CODEX_HOME=":
			t.Errorf("missing ~/.codex must not be passed through: %q", kv)
		case len(kv) > len("PI_CODING_AGENT_DIR=") && kv[:len("PI_CODING_AGENT_DIR=")] == "PI_CODING_AGENT_DIR=":
			t.Errorf("missing ~/.pi/agent must not be passed through: %q", kv)
		}
	}
}
