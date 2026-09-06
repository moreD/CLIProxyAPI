package auth

import "testing"

func TestMergeQuotaInfoPreservesAndOverlaysResetCredits(t *testing.T) {
	base := &QuotaInfo{
		RateLimitResetCredits: &QuotaResetCredits{
			AvailableCount:           3,
			ApplicableAvailableCount: 0,
		},
	}

	merged := MergeQuotaInfo(base, &QuotaInfo{FiveHour: QuotaWindow{UsedPercentKnown: true}})
	if merged.RateLimitResetCredits == nil || merged.RateLimitResetCredits.AvailableCount != 3 {
		t.Fatalf("merged reset credits = %#v, want preserved count 3", merged.RateLimitResetCredits)
	}

	update := &QuotaInfo{RateLimitResetCredits: &QuotaResetCredits{
		AvailableCount:           2,
		ApplicableAvailableCount: 1,
	}}
	merged = MergeQuotaInfo(merged, update)
	if merged.RateLimitResetCredits == nil || merged.RateLimitResetCredits.AvailableCount != 2 || merged.RateLimitResetCredits.ApplicableAvailableCount != 1 {
		t.Fatalf("merged reset credits = %#v, want 2 available and 1 applicable", merged.RateLimitResetCredits)
	}

	update.RateLimitResetCredits.AvailableCount = 9
	if merged.RateLimitResetCredits.AvailableCount != 2 {
		t.Fatalf("merged reset credits share update storage: %#v", merged.RateLimitResetCredits)
	}
}
