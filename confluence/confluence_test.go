package confluence_test

import (
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metricsql"

	"github.com/VictoriaMetrics/metricsql/confluence"
)

// TestOptimizePreservesSeriesSet compares the series set selected by the
// original query against the series set selected by Optimize(query), using the
// tiny reference label-set model in the confluence package.
func TestOptimizePreservesSeriesSet(t *testing.T) {
	for _, tc := range confluence.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			orig := confluence.MustParse(tc.Query)
			opt := metricsql.Optimize(orig)

			changed := string(opt.AppendString(nil)) != string(orig.AppendString(nil))
			if changed != tc.ExpectChanged {
				t.Fatalf("rewrite expectation mismatch for %q:\nchanged=%v want=%v\noptimized: %s",
					tc.Query, changed, tc.ExpectChanged, string(opt.AppendString(nil)))
			}

			want := confluence.Eval(orig, tc.Universe)
			got := confluence.Eval(opt, tc.Universe)
			if !confluence.EqualSeriesSets(want, got) {
				t.Fatalf("optimizer changed the selected series set for %q\noptimized query: %s\n%s",
					tc.Query, string(opt.AppendString(nil)), confluence.Diff(want, got))
			}
		})
	}
}

// TestPrettifyParseRoundTrip verifies that prettified output reparses to the
// same canonical AST, so no modifier or label-matching detail is lost.
func TestPrettifyParseRoundTrip(t *testing.T) {
	for _, tc := range confluence.RoundTripCases {
		t.Run(tc.Name, func(t *testing.T) {
			e, err := metricsql.Parse(tc.Query)
			if err != nil {
				t.Fatalf("cannot parse original %q: %s", tc.Query, err)
			}
			canonical := string(e.AppendString(nil))

			pretty, err := metricsql.Prettify(tc.Query)
			if err != nil {
				t.Fatalf("Prettify(%q) error: %s", tc.Query, err)
			}
			e2, err := metricsql.Parse(pretty)
			if err != nil {
				t.Fatalf("prettified text does not reparse for %q:\n%s\nparse error: %s",
					tc.Query, pretty, err)
			}
			if got := string(e2.AppendString(nil)); got != canonical {
				t.Fatalf("prettify/parse round trip changed the AST for %q\nbefore: %s\nafter:  %s",
					tc.Query, canonical, got)
			}
		})
	}
}

// TestOptimizeRoundTripComposed chains Optimize and Prettify to make sure the
// rewritten query also survives the prettifier round trip.
func TestOptimizeRoundTripComposed(t *testing.T) {
	for _, tc := range confluence.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			orig := confluence.MustParse(tc.Query)
			opt := metricsql.Optimize(orig)
			pretty, err := metricsql.Prettify(string(opt.AppendString(nil)))
			if err != nil {
				t.Fatalf("Prettify optimized query %q: %s", tc.Query, err)
			}
			reparsed, err := metricsql.Parse(pretty)
			if err != nil {
				t.Fatalf("reparse prettified optimized query %q: %s\n%s", tc.Query, err, pretty)
			}
			if string(reparsed.AppendString(nil)) != string(opt.AppendString(nil)) {
				t.Fatalf("optimized query lost detail through prettify:\nbefore: %s\nafter:  %s",
					string(opt.AppendString(nil)), string(reparsed.AppendString(nil)))
			}
			if !strings.Contains(pretty, ")") && !strings.Contains(pretty, "{") {
				t.Fatalf("unexpected prettifier output:\n%s", pretty)
			}
		})
	}
}
