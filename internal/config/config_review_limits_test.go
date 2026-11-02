package config

import "testing"

func TestReviewLimitsUseTrustedConfigAndDefaultToTwo(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("review:\n  max_passes: 8\n  low_risk_model: untrusted\n"))
	if err != nil {
		t.Fatal(err)
	}
	defaultReview := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, nil, true)).Review
	if got := defaultReview.ReviewPassLimit(); got != 2 || defaultReview.LowRiskModel != "" {
		t.Fatalf("pushed review settings escaped trust boundary: %+v", defaultReview)
	}
	trusted, err := LoadRepoFromBytes([]byte("review:\n  max_passes: 3\n  low_risk_model: sonnet\n"))
	if err != nil {
		t.Fatal(err)
	}
	review := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, trusted, true)).Review
	if got := review.ReviewPassLimit(); got != 3 || review.LowRiskModel != "sonnet" {
		t.Fatalf("trusted review settings lost: %+v", review)
	}
	if _, err := LoadRepoFromBytes([]byte("review:\n  max_passes: -1\n")); err == nil {
		t.Fatal("negative review limit accepted")
	}
}
