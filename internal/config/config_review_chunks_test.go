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
