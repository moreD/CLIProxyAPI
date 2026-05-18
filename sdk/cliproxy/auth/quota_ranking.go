package auth

func quotaRankingKnown(auth *Auth) bool {
	return auth != nil && auth.RuntimeQuota != nil && auth.RuntimeQuota.HasWeeklyRemaining()
}

// compareQuotaForSticky returns -1 when left should be preferred over right.
func compareQuotaForSticky(left, right *Auth) int {
	leftKnown := quotaRankingKnown(left)
	rightKnown := quotaRankingKnown(right)
	switch {
	case leftKnown && !rightKnown:
		return -1
	case !leftKnown && rightKnown:
		return 1
	}

	if leftKnown && rightKnown {
		leftWeekly := left.RuntimeQuota.Weekly
		rightWeekly := right.RuntimeQuota.Weekly
		if leftWeekly.UsagePercent != rightWeekly.UsagePercent {
			if leftWeekly.UsagePercent > rightWeekly.UsagePercent {
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
