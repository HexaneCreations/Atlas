package monitor

import (
	"strconv"
	"strings"
)

// StatusCodes is a set of accepted HTTP status code ranges.
type StatusCodes []codeRange

type codeRange struct{ lo, hi int }

// ParseStatusCodes parses entries such as "200-299" and "301".
func ParseStatusCodes(entries []string) (StatusCodes, error) {
	bad := func() (StatusCodes, error) {
		return nil, invalid("accepted_status", `accepted status codes must be codes or ranges such as "200-299", between 100 and 599`)
	}
	if len(entries) == 0 || len(entries) > MaxStatusRanges {
		return bad()
	}
	out := make(StatusCodes, 0, len(entries))
	for _, e := range entries {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(e), "-")
		if !isRange {
			hi = lo
		}
		l, err1 := strconv.Atoi(lo)
		h, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || l < 100 || h > 599 || l > h {
			return bad()
		}
		out = append(out, codeRange{l, h})
	}
	return out, nil
}

// Accepts reports whether code is in any range.
func (s StatusCodes) Accepts(code int) bool {
	for _, r := range s {
		if code >= r.lo && code <= r.hi {
			return true
		}
	}
	return false
}
