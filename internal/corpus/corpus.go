// Package corpus holds the recorded quote corpus and the semantic comparator used by both
// cmd/corpus-record and cmd/corpus-gate. Phase 0 of go-ba: the corpus IS the spec — nothing ships
// that doesn't reproduce the php backend's /api/v2/quote responses over this corpus.
package corpus

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Case is one recorded (request, response) pair against the oracle backend.
type Case struct {
	Name     string          `json:"name"`
	Request  json.RawMessage `json:"request"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response"`
}

// Corpus is the full recorded artifact. Responses are date-dependent (minPickupDate, excluded
// dates walk forward from "today"), so a corpus is only authoritative for the day it was recorded;
// re-record rather than reuse a stale one.
type Corpus struct {
	RecordedAt string `json:"recordedAt"`
	Oracle     string `json:"oracle"`
	Cases      []Case `json:"cases"`
}

// Diff is one semantic divergence between oracle and candidate at a JSON path.
type Diff struct {
	Path     string
	Oracle   string
	Candidate string
}

// Options controls the comparator.
type Options struct {
	// FloatTolerance is the absolute tolerance for numeric leaf comparison. Prices are money —
	// keep this at exact-or-nearly-exact (1e-9) unless a documented rounding asymmetry appears.
	FloatTolerance float64
	// IgnorePaths are path prefixes (after wildcarding array indices to *) excluded from
	// comparison, for fields established as volatile between identical requests.
	IgnorePaths []string
}

// Compare semantically compares two JSON documents and returns all divergences.
// Objects compare by key union, arrays by index (order is part of the /api/v2/quote contract),
// numbers within tolerance, everything else exactly.
func Compare(oracle, candidate []byte, opts Options) ([]Diff, error) {
	var o, c any
	if err := json.Unmarshal(oracle, &o); err != nil {
		return nil, fmt.Errorf("oracle json: %w", err)
	}
	if err := json.Unmarshal(candidate, &c); err != nil {
		return nil, fmt.Errorf("candidate json: %w", err)
	}
	var diffs []Diff
	walk(o, c, "", opts, &diffs)
	return diffs, nil
}

func walk(o, c any, path string, opts Options, diffs *[]Diff) {
	for _, ig := range opts.IgnorePaths {
		if strings.HasPrefix(wildcardIndices(path), ig) {
			return
		}
	}
	switch ov := o.(type) {
	case map[string]any:
		cv, ok := c.(map[string]any)
		if !ok {
			record(diffs, path, describe(o), describe(c))
			return
		}
		keys := map[string]struct{}{}
		for k := range ov {
			keys[k] = struct{}{}
		}
		for k := range cv {
			keys[k] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			oc, oOK := ov[k]
			cc, cOK := cv[k]
			childPath := path + "." + k
			if path == "" {
				childPath = k
			}
			switch {
			case !oOK:
				record(diffs, childPath, "<absent>", describe(cc))
			case !cOK:
				record(diffs, childPath, describe(oc), "<absent>")
			default:
				walk(oc, cc, childPath, opts, diffs)
			}
		}
	case []any:
		cv, ok := c.([]any)
		if !ok {
			record(diffs, path, describe(o), describe(c))
			return
		}
		if len(ov) != len(cv) {
			record(diffs, path, fmt.Sprintf("len=%d", len(ov)), fmt.Sprintf("len=%d", len(cv)))
			return
		}
		for i := range ov {
			walk(ov[i], cv[i], fmt.Sprintf("%s.%d", path, i), opts, diffs)
		}
	case float64:
		cf, ok := c.(float64)
		if !ok || math.Abs(ov-cf) > opts.FloatTolerance {
			record(diffs, path, describe(o), describe(c))
		}
	default:
		if describe(o) != describe(c) {
			record(diffs, path, describe(o), describe(c))
		}
	}
}

func record(diffs *[]Diff, path, o, c string) {
	*diffs = append(*diffs, Diff{Path: wildcardIndices(path), Oracle: o, Candidate: c})
}

func describe(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// wildcardIndices turns data.options.serviceTypes.3.price into data.options.serviceTypes.*.price
// so ignore rules and diff grouping are index-independent.
func wildcardIndices(path string) string {
	parts := strings.Split(path, ".")
	for i, p := range parts {
		if p != "" && strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
			parts[i] = "*"
		}
	}
	return strings.Join(parts, ".")
}
