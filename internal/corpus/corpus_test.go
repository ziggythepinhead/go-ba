package corpus

import "testing"

func mustCompare(t *testing.T, a, b string, opts Options) []Diff {
	t.Helper()
	diffs, err := Compare([]byte(a), []byte(b), opts)
	if err != nil {
		t.Fatal(err)
	}
	return diffs
}

func TestIdenticalDocumentsProduceNoDiffs(t *testing.T) {
	doc := `{"data":{"serviceTypes":[{"id":1,"price":{"gross":12.34}},{"id":2,"price":null}]}}`
	if diffs := mustCompare(t, doc, doc, Options{FloatTolerance: 1e-9}); len(diffs) != 0 {
		t.Fatalf("expected clean, got %v", diffs)
	}
}

func TestNumericToleranceIsRespectedAndBounded(t *testing.T) {
	a := `{"p":12.340000001}`
	b := `{"p":12.340000002}`
	if diffs := mustCompare(t, a, b, Options{FloatTolerance: 1e-6}); len(diffs) != 0 {
		t.Fatalf("within tolerance should pass, got %v", diffs)
	}
	c := `{"p":12.35}`
	if diffs := mustCompare(t, a, c, Options{FloatTolerance: 1e-6}); len(diffs) != 1 {
		t.Fatalf("beyond tolerance should diverge, got %v", diffs)
	}
}

func TestArrayLengthAndOrderMatter(t *testing.T) {
	a := `{"s":[1,2,3]}`
	if diffs := mustCompare(t, a, `{"s":[1,2]}`, Options{}); len(diffs) != 1 || diffs[0].Path != "s" {
		t.Fatalf("length divergence expected at s, got %v", diffs)
	}
	if diffs := mustCompare(t, a, `{"s":[1,3,2]}`, Options{}); len(diffs) != 2 {
		t.Fatalf("order divergence expected at 2 indices, got %v", diffs)
	}
}

func TestAbsentKeysAreReportedBothWays(t *testing.T) {
	diffs := mustCompare(t, `{"a":1,"b":2}`, `{"a":1,"c":3}`, Options{})
	if len(diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %v", diffs)
	}
}

func TestIgnorePathsUseWildcardedIndices(t *testing.T) {
	a := `{"data":{"serviceTypes":[{"id":1,"usedPickupDate":"2026-07-10"}]}}`
	b := `{"data":{"serviceTypes":[{"id":1,"usedPickupDate":"2026-07-11"}]}}`
	opts := Options{IgnorePaths: []string{"data.serviceTypes.*.usedPickupDate"}}
	if diffs := mustCompare(t, a, b, opts); len(diffs) != 0 {
		t.Fatalf("ignored path should not diverge, got %v", diffs)
	}
	if diffs := mustCompare(t, a, b, Options{}); len(diffs) != 1 {
		t.Fatalf("without ignore it must diverge, got %v", diffs)
	}
}

func TestTypeMismatchDiverges(t *testing.T) {
	if diffs := mustCompare(t, `{"a":[1]}`, `{"a":{"0":1}}`, Options{}); len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %v", diffs)
	}
	if diffs := mustCompare(t, `{"a":"1"}`, `{"a":1}`, Options{}); len(diffs) != 1 {
		t.Fatalf("string-vs-number must diverge, got %v", diffs)
	}
}
