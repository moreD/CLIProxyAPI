package auth

import "time"

const (
	weeklyQuotaRefreshWeightMin = 1.0
	weeklyQuotaRefreshWeightMax = 3.0
	weeklyQuotaRefreshHorizon   = 7 * 24 * time.Hour
)

type quotaRankingValue struct {
	known          bool
	usedPercent    float64
	weightedUsage  float64
	maxUsedPercent float64
}

func quotaRanking(auth *Auth) quotaRankingValue {
	return quotaRankingAt(auth, time.Now())
}

func quotaRankingAt(auth *Auth, now time.Time) quotaRankingValue {
	if auth == nil || auth.RuntimeQuota == nil {
		return quotaRankingValue{}
	}
	if auth.RuntimeQuota.Weekly.usedKnown() {
		usedPercent := auth.RuntimeQuota.Weekly.UsedPercent
		refreshWeight := weeklyQuotaRefreshWeight(auth.RuntimeQuota.Weekly.NextFreshAt, now)
		return quotaRankingValue{
			known:          true,
			usedPercent:    usedPercent,
			weightedUsage:  usedPercent / refreshWeight,
			maxUsedPercent: weeklyQuotaMaxUsedPercent,
		}
	}
	return quotaRankingValue{}
}

func weeklyQuotaRefreshWeight(nextFreshAt, now time.Time) float64 {
	if nextFreshAt.IsZero() {
		return weeklyQuotaRefreshWeightMin
	}
	remaining := nextFreshAt.Sub(now)
	if remaining <= 0 {
		return weeklyQuotaRefreshWeightMax
	}
	if remaining >= weeklyQuotaRefreshHorizon {
		return weeklyQuotaRefreshWeightMin
	}
	progress := float64(remaining) / float64(weeklyQuotaRefreshHorizon)
	return weeklyQuotaRefreshWeightMax - progress*(weeklyQuotaRefreshWeightMax-weeklyQuotaRefreshWeightMin)
}

func quotaRankingKnown(auth *Auth) bool {
	return quotaRanking(auth).known
}

// compareQuotaForSticky returns -1 when left should be preferred over right.
func compareQuotaForSticky(left, right *Auth) int {
	return compareQuotaForStickyAt(left, right, time.Now())
}

func compareQuotaForStickyAt(left, right *Auth, now time.Time) int {
	leftRank := quotaRankingAt(left, now)
	rightRank := quotaRankingAt(right, now)
	leftKnown := leftRank.known
	rightKnown := rightRank.known
	switch {
	case leftKnown && !rightKnown:
		if leftRank.usedPercent >= leftRank.maxUsedPercent {
			return 1
		}
		return -1
	case !leftKnown && rightKnown:
		if rightRank.usedPercent >= rightRank.maxUsedPercent {
			return -1
		}
		return 1
	}

	if leftKnown && rightKnown {
		if leftRank.weightedUsage != rightRank.weightedUsage {
			if leftRank.weightedUsage < rightRank.weightedUsage {
				return -1
			}
			return 1
		}
	}

	leftID := ""
	if left != nil {
		leftID = left.ID
	}
	rightID := ""
	if right != nil {
		rightID = right.ID
	}
	switch {
	case leftID < rightID:
		return -1
	case leftID > rightID:
		return 1
	default:
		return 0
	}
}

func bestStickyAuthIndex(auths []*Auth) int {
	best := -1
	for index, auth := range auths {
		if auth == nil {
			continue
		}
		if best < 0 || compareQuotaForSticky(auth, auths[best]) < 0 {
			best = index
		}
	}
	return best
}
