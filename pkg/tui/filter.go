package tui

import "strings"

// boundaryBonus outranks any distance penalty, so a hit at a word start always
// beats one buried mid-word however much earlier the latter sits.
const boundaryBonus = 1 << 16

// verbatimScore rates a case-insensitive substring hit of query in text,
// returning false when query does not appear verbatim. A higher score is a
// better match: a word boundary start is rewarded, distance penalised. Every
// hit spans the whole query, so length does not enter the score.
func verbatimScore(text, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	lowText, lowQuery := strings.ToLower(text), strings.ToLower(query)

	best, found := 0, false
	for i := 0; i < len(lowText); {
		idx := strings.Index(lowText[i:], lowQuery)
		if idx < 0 {
			break
		}
		pos := i + idx
		score := -pos // distance skipped
		if pos == 0 || isBoundary(lowText[pos-1]) {
			score += boundaryBonus
		}
		if !found || score > best {
			best, found = score, true
		}
		i = pos + 1
	}
	return best, found
}

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
