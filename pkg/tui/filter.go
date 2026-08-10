package tui

import "strings"

// matchScore rates how well query matches text as a subsequence, returning
// false when it does not match at all. A higher score is a better match:
// contiguous runs and word boundary starts are rewarded, gaps are penalised.
func matchScore(text, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	lowText, lowQuery := strings.ToLower(text), strings.ToLower(query)

	var score, run int
	ti := 0
	for qi := 0; qi < len(lowQuery); qi++ {
		c := lowQuery[qi]
		idx := strings.IndexByte(lowText[ti:], c)
		if idx < 0 {
			return 0, false
		}
		pos := ti + idx
		switch {
		case idx == 0 && run > 0:
			run++
			score += 8 * run // contiguous, and increasingly so
		case pos == 0 || isBoundary(lowText[pos-1]):
			run = 1
			score += 6
		default:
			run = 0
			score += 1
		}
		score -= idx // distance skipped
		ti = pos + 1
	}
	return score, true
}

// isBoundary reports whether c precedes a word start.
func isBoundary(c byte) bool {
	switch c {
	case ' ', '/', '-', '_', '.', ':', '\t':
		return true
	default:
		return false
	}
}
