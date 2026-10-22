package config

import "testing"

func TestReviewChunkThreshold_TrustedOnlyAndOffByDefault(t *testing.T) {
	const pushed = "review:\n  chunk_threshold_lines: 501\n"
	feature, err := LoadRepoFromBytes([]byte(pushed))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(feature, nil, false)).Review.ChunkThresholdLines; got != 0 {
		t.Fatalf("pushed-only chunk threshold = %d, want disabled", got)
	}
	trusted, err := LoadRepoFromBytes([]byte("review:\n  chunk_threshold_lines: 5000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(feature, trusted, false)).Review.ChunkThresholdLines; got != 5000 {
		t.Fatalf("trusted chunk threshold = %d, want 5000", got)
	}
	for _, bad := range []string{"-1", "1", "500"} {
		if _, err := LoadRepoFromBytes([]byte("review:\n  chunk_threshold_lines: " + bad + "\n")); err == nil {
			t.Errorf("accepted invalid chunk threshold %s", bad)
		}
	}
}

func TestReviewChunkBudget_TrustedOnlyValidatedAndDefaultOff(t *testing.T) {
	feature, err := LoadRepoFromBytes([]byte("review:\n  chunk_max_lines: 300\n  chunk_max_bytes: 40000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(feature, nil, false)).Review; got.ChunkMaxLines != 0 || got.ChunkMaxBytes != 0 {
		t.Fatalf("pushed-only chunk budget = %+v, want the defaults", got)
	}
	trusted, err := LoadRepoFromBytes([]byte("review:\n  chunk_threshold_lines: 800\n  chunk_max_lines: 400\n  chunk_max_bytes: 65536\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(&GlobalConfig{}, EffectiveRepoConfig(feature, trusted, false)).Review
	if got.ChunkMaxLines != 400 || got.ChunkMaxBytes != 65536 {
		t.Fatalf("trusted chunk budget = %+v, want 400 lines / 65536 bytes", got)
	}
	for _, bad := range []string{"chunk_max_lines: -1", "chunk_max_lines: 99", "chunk_max_bytes: -1", "chunk_max_bytes: 4096"} {
		if _, err := LoadRepoFromBytes([]byte("review:\n  " + bad + "\n")); err == nil {
			t.Errorf("accepted invalid chunk budget %q", bad)
		}
	}
}
