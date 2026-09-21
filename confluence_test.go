package metricsql

// Executable counter-evidence for the semantic confluence of:
//
//	function metadata -> optimizer filter pushdown -> prettifier writeback.
//
// Every test here is independently runnable:
//
//	go test -run 'TestConfluenceOptimizeSound$'
//	go test -run 'TestConfluenceNaivePushdownCounterExample$'
//	go test -run 'TestConfluencePrettifyRoundTrip$'
//	go test -run 'TestConfluenceErrorDiagnostics$'
//	go test -run 'TestConfluenceRegexpCache$'
//
// See ANALYSIS.md for the legality conditions each query exercises.

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func seResult(ss []seSeries) string {
	type kv struct {
		k string
		v float64
	}
	rows := make([]kv, 0, len(ss))
	for _, s := range ss {
		rows = append(rows, kv{k: seKey(s.labels), v: s.value})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].k != rows[j].k {
			return rows[i].k < rows[j].k
		}
		return rows[i].v < rows[j].v
	})
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.k)
		b.WriteByte('=')
		b.WriteString(formatSEFloat(r.v))
		b.WriteByte('\n')
	}
	return b.String()
}

func formatSEFloat(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func assertSameResult(t *testing.T, tag string, got, want []seSeries) {
	t.Helper()
	a, b := seResult(got), seResult(want)
	if a != b {
		t.Fatalf("%s: result mismatch (labels+values);\ngot:\n%s\nwant:\n%s", tag, a, b)
	}
}

// confluenceQueries covers the four requested ingredients together:
// nested rollups, binary joins (1:1, group_left, set ops, fill),
// keep_metric_names and label manipulation.
var confluenceQueries = []string{
	// nested rollup inside a 1:1 binary op; common filters flow into rate args.
	`sum(rate(http_requests_total{job="api"}[5m])) by (instance) / sum(rate(errors_total{job="api"}[5m])) by (instance)`,
	// many-to-one join: right-side extra labels are trimmed by on(); group_left
	// makes the union of common filters conditional on the matching labels.
	`http_requests_total{job="api"} * on(instance) group_left() queue_size{job="web"}`,
	// group_left + on(instance): a right-side filter on a NON-signature label
	// (route) must be trimmed from the common set, otherwise the left operand
	// is wrongly narrowed. Both sides carry it syntactically.
	`http_requests_total{job="api"} * on(instance) group_left() errors_total{route="/v1",job="api"}`,
	// keep_metric_names on a binary op: label pushdown must remain sound and
	// the flag must survive Clone()/AppendString()/Prettify().
	`(http_requests_total{job="api"} + errors_total{job="api"}) keep_metric_names`,
	// keep_metric_names on a rollup function: __name__ survives the function.
	`rate(http_requests_total{job="api"}[5m]) keep_metric_names / errors_total{job="api"}`,
	// label_set: an overwritten label cannot be pushed as a precondition, but a
	// fresh label can.
	`label_set(http_requests_total{job="api"}, "job", "rewritten") + errors_total{job="api"}`,
	// label_replace: destination label filters must be dropped before pushdown.
	`label_replace(http_requests_total{job="api"}, "service", "$1", "job", "(.*)") + errors_total{job="api",service="x"}`,
	// label_del: filters on a deleted label cannot constrain the inner selector.
	`label_del(http_requests_total{job="api"}, "job") + errors_total{job="api"}`,
	// label_keep: only retained labels may be pushed through.
	`label_keep(http_requests_total{job="api",zone="z1"}, "job", "instance") + errors_total{job="api",zone="z1"}`,
	// aggregate with by() acts as a barrier for non-grouping common labels.
	`sum(http_requests_total{job="api"}) by (instance) + errors_total{job="api",instance="h1"}`,
	// nested rollup below a barrier aggregate, joined 1:1.
	`sum(rate(http_requests_total{job="api"}[5m])) by (instance,zone) + sum(rate(errors_total{job="api"}[5m])) by (instance,zone)`,
	// count_values_over_time: the synthesized destination label must be dropped
	// before common filters reach the rollup argument.
	`count_values_over_time("vv", rate(http_requests_total{job="api"}[5m])) + on(instance) group_left() errors_total{job="api"}`,
	// fill_left: filters may only flow right->left; the sum barrier is respected.
	`http_requests_total{job="api"} + fill_left(0) sum(errors_total{job="api"})`,
	// set op unless: only left-side filters survive, trimmed by the modifier.
	`http_requests_total{job="api",zone="z1"} unless on(instance) errors_total{zone="z2"}`,
	// nested label manipulation under a rollup inside a join with keep names.
	`(rate(label_set(http_requests_total{job="api"},"zone","z1")[5m]) keep_metric_names * on(instance) group_left() errors_total{job="api"}) keep_metric_names`,
}

func TestConfluenceOptimizeSound(t *testing.T) {
	u := newSEUniverse()
	for _, q := range confluenceQueries {
		t.Run(seQueryName(q), func(t *testing.T) {
			e, err := Parse(q)
			if err != nil {
				t.Fatalf("Parse(%q) error: %s", q, err)
			}
			want := u.evalSE(e)
			oe := Optimize(e)
			got := u.evalSE(oe)
			assertSameResult(t, "Optimize changed query results for "+q, got, want)
			// Idempotence: optimizing twice must be a fixpoint.
			oe2 := Optimize(oe)
			if string(oe.AppendString(nil)) != string(oe2.AppendString(nil)) {
				t.Fatalf("Optimize is not idempotent for %q:\n%s\nvs\n%s",
					q, oe.AppendString(nil), oe2.AppendString(nil))
			}
		})
	}
}

func seQueryName(q string) string {
	r := strings.NewReplacer(" ", "_", `"`, "", "{", "", "}", "",
		"(", "", ")", "", "[", "", "]", "", "/", "_", "*", "mul",
		"=", "eq", "~", "re", "!", "not", "+", "plus", ",", "_",
		"\\", "_", ":", "_", ".", "_")
	s := r.Replace(q)
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// naivePushdown pushes every common label filter into every metric selector,
// ignoring function metadata, aggregate/group barriers and label manipulation.
// It is the intentionally-wrong rewriter whose job is to prove that the real
// optimizer's legality conditions carry semantic weight.
func naivePushdown(e Expr, lfs []LabelFilter) {
	if len(lfs) == 0 {
		return
	}
	switch t := e.(type) {
	case *MetricExpr:
		for i, local := range t.LabelFilterss {
			t.LabelFilterss[i] = unionLabelFilters(local, lfs)
		}
	case *RollupExpr:
		naivePushdown(t.Expr, lfs)
	case *FuncExpr:
		for _, a := range t.Args {
			naivePushdown(a, lfs)
		}
	case *AggrFuncExpr:
		for _, a := range t.Args {
			naivePushdown(a, lfs)
		}
	case *BinaryOpExpr:
		naivePushdown(t.Left, lfs)
		naivePushdown(t.Right, lfs)
	}
}

// naiveCommonFilters collects filters from both sides without any modifier or
// function-aware trimming (the getCommonLabelFilters half of the bug).
func naiveCommonFilters(e Expr) []LabelFilter {
	switch t := e.(type) {
	case *MetricExpr:
		return getCommonLabelFiltersWithoutMetricName(t.LabelFilterss)
	case *RollupExpr:
		return naiveCommonFilters(t.Expr)
	case *FuncExpr, *AggrFuncExpr:
		args := getSEArgs(e)
		if len(args) == 0 {
			return nil
		}
		return naiveCommonFilters(args[0])
	case *BinaryOpExpr:
		return unionLabelFilters(naiveCommonFilters(t.Left), naiveCommonFilters(t.Right))
	default:
		return nil
	}
}

func getSEArgs(e Expr) []Expr {
	switch t := e.(type) {
	case *FuncExpr:
		return t.Args
	case *AggrFuncExpr:
		return t.Args
	default:
		return nil
	}
}

// counterExampleQueries each contain at least one legality barrier. The real
// Optimize() stays sound on them; the naive rewriter must change results.
var counterExampleQueries = []string{
	// barrier: ungrouped sum() collapses all series to one with no labels; the
	// one-to-one match then requires no-label/one-series alignment, so pushing a
	// job filter into it removes the right operand of the join.
	`http_requests_total{job="api",instance="h1"} + sum(errors_total{job="api"})`,
	// barrier: label_set rewrites job AFTER selection; the synthesized job
	// precondition must not be read into the inner selector, otherwise the left
	// operand loses every web series before the rewrite.
	`label_set(http_requests_total{instance="h1"}, "job", "api") * on(instance) group_left() sum(errors_total{job="api"}) by (instance)`,
	// barrier: by(instance) drops zone before the join; pushing the right-side
	// zone="z2" into the left aggregate kills the h1/z1 contribution, and
	// pushing zone="z1" into the right aggregate empties it.
	`sum(http_requests_total{zone="z1"}) by (instance) * on(instance) group_left() sum(errors_total{instance="h1",zone="z2"}) by (instance)`,
	// barrier: label_del removes job inside the left subtree; the right-side
	// job precondition must not become a selector constraint on the left.
	`label_del(http_requests_total{instance="h1"}, "job") * on(instance) group_left() sum(errors_total{job="api",instance="h1"}) by (instance)`,
	// barrier: group_left matches on(instance) only; the left-side container
	// filter must be trimmed before entering the right aggregate, otherwise the
	// right operand is wrongly emptied (errors live in container c1).
	`http_requests_total{container="c0",job="api"} * on(instance) group_left() sum(errors_total) by (instance)`,
}

func TestConfluenceNaivePushdownCounterExample(t *testing.T) {
	u := newSEUniverse()
	detected := 0
	for _, q := range counterExampleQueries {
		t.Run(seQueryName(q), func(t *testing.T) {
			e, err := Parse(q)
			if err != nil {
				t.Fatalf("Parse(%q): %s", q, err)
			}
			// Sanity: the production optimizer must be sound here.
			oe := Optimize(e)
			assertSameResult(t, "production optimizer must be sound for "+q,
				u.evalSE(oe), u.evalSE(e))

			buggy := Clone(e)
			lfs := naiveCommonFilters(buggy)
			naivePushdown(buggy, lfs)
			got := u.evalSE(buggy)
			want := u.evalSE(e)
			if seResult(got) == seResult(want) {
				t.Fatalf("counter-example failed to detect the invalid pushdown for %q; "+
					"naive rewrite %q unexpectedly preserved results",
					q, buggy.AppendString(nil))
			}
			detected++
		})
	}
	if detected != len(counterExampleQueries) {
		t.Fatalf("expected %d counter-examples, got %d", len(counterExampleQueries), detected)
	}
}

// Long queries that force appendPrettifiedExpr past maxPrettifiedLineLen so the
// multiline writeback branches (BinaryOpExpr, RollupExpr, AggrFuncExpr,
// FuncExpr with keep_metric_names) are exercised instead of the single-line path.
var prettifyRoundTripQueries = []string{
	`(http_requests_total{job="api-gateway",zone="east-1",instance="host-12345.example.com"} + errors_total{job="api-gateway",zone="east-1",instance="host-12345.example.com"}) keep_metric_names`,
	`rate(http_requests_total{job="api-gateway",zone="east-1",instance="host-12345.example.com",route="/api/v2/widgets"}[5m]) keep_metric_names`,
	`sum(rate(http_requests_total{job="api-gateway",zone="east-1",instance="host-12345.example.com",route="/api/v2/widgets"}[5m])) by (job, instance, zone, route, namespace, cluster)`,
	`http_requests_total{job="api-gateway",zone="east-1",instance="host-12345.example.com"} * on(job, instance) group_left(zone, region) errors_total{job="api-gateway",zone="east-2",region="us-east"}`,
	`http_requests_total{job="api-gateway",zone="east-1"} == bool ignoring(zone, region) group_right(instance) errors_total{job="api-gateway",region="us-east-1"}`,
	`label_replace(rate(http_requests_total{job="api-gateway",zone="east-1",instance="host-12345.example.com"}[5m]), "service_name", "$1", "job", "^(.+)$") + on(instance) group_left() errors_total{job="api-gateway"}`,
	`(sum(rate(http_requests_total{job="api-gateway",zone="east-1",instance="host-12345.example.com"}[5m])) by (instance,zone) + fill(0) sum(rate(errors_total{job="api-gateway",zone="east-1"}[5m])) by (instance,zone)) keep_metric_names`,
}

func TestConfluencePrettifyRoundTrip(t *testing.T) {
	for _, q := range prettifyRoundTripQueries {
		t.Run(seQueryName(q), func(t *testing.T) {
			e, err := Parse(q)
			if err != nil {
				t.Fatalf("Parse(%q): %s", q, err)
			}
			wantCanonical := string(e.AppendString(nil))

			pretty, err := Prettify(q)
			if err != nil {
				t.Fatalf("Prettify(%q): %s", q, err)
			}
			// The query must actually have taken a multiline branch; otherwise
			// the test would not cover the writeback path it claims to cover.
			if !strings.Contains(pretty, "\n") {
				t.Fatalf("expected multiline prettified output for %q; got single line:\n%s", q, pretty)
			}

			reparsed, err := Parse(pretty)
			if err != nil {
				t.Fatalf("prettified text is not reparseable for %q:\n%s\nparse error: %s", q, pretty, err)
			}
			gotCanonical := string(reparsed.AppendString(nil))
			if gotCanonical != wantCanonical {
				t.Fatalf("prettify round trip changed semantics for %q:\npretty:\n%s\ngot: %s\nwant: %s",
					q, pretty, gotCanonical, wantCanonical)
			}

			// Explicit modifier preservation checks: label matching must be identical.
			assertModifiersSurvive(t, e, reparsed)

			// And the optimizer must agree before/after the prettify round trip.
			assertSameResult(t, "eval after prettify for "+q,
				newSEUniverse().evalSE(reparsed), newSEUniverse().evalSE(e))
		})
	}
}

func assertModifiersSurvive(t *testing.T, want, got Expr) {
	t.Helper()
	wantB := seBinarySigs(want)
	gotB := seBinarySigs(got)
	if strings.Join(wantB, "|") != strings.Join(gotB, "|") {
		t.Fatalf("binary op modifiers changed by round trip:\nwant %v\ngot  %v", wantB, gotB)
	}
	wantF := seFuncKeep(want)
	gotF := seFuncKeep(got)
	if strings.Join(wantF, "|") != strings.Join(gotF, "|") {
		t.Fatalf("func keep_metric_names modifiers changed by round trip:\nwant %v\ngot  %v", wantF, gotF)
	}
	wantA := seAggrSigs(want)
	gotA := seAggrSigs(got)
	if strings.Join(wantA, "|") != strings.Join(gotA, "|") {
		t.Fatalf("aggregate modifiers changed by round trip:\nwant %v\ngot  %v", wantA, gotA)
	}
}

func seBinaryModSig(be *BinaryOpExpr) string { return "" }

func seBinarySigs(e Expr) []string {
	var out []string
	VisitAll(e, func(x Expr) {
		if be, ok := x.(*BinaryOpExpr); ok {
			out = append(out, seSigBinary(be))
		}
	})
	return out
}

func seSigBinary(be *BinaryOpExpr) string {
	var b strings.Builder
	b.WriteString(be.Op)
	if be.Bool {
		b.WriteString("|bool")
	}
	if be.GroupModifier.Op != "" {
		b.WriteString("|")
		b.WriteString(string(be.GroupModifier.AppendString(nil)))
	}
	if be.JoinModifier.Op != "" {
		b.WriteString("|")
		b.WriteString(string(be.JoinModifier.AppendString(nil)))
		if be.JoinModifierPrefix != nil {
			b.WriteString("|prefix:")
			b.WriteString(string(be.JoinModifierPrefix.AppendString(nil)))
		}
	}
	if be.FillLeft != nil {
		b.WriteString("|fill_left:")
		b.WriteString(string(be.FillLeft.AppendString(nil)))
	}
	if be.FillRight != nil {
		b.WriteString("|fill_right:")
		b.WriteString(string(be.FillRight.AppendString(nil)))
	}
	if be.KeepMetricNames {
		b.WriteString("|keep_metric_names")
	}
	return b.String()
}

func seFuncKeep(e Expr) []string {
	var out []string
	VisitAll(e, func(x Expr) {
		if fe, ok := x.(*FuncExpr); ok && fe.KeepMetricNames {
			out = append(out, fe.Name)
		}
	})
	sort.Strings(out)
	return out
}

func seAggrSigs(e Expr) []string {
	var out []string
	VisitAll(e, func(x Expr) {
		if ae, ok := x.(*AggrFuncExpr); ok {
			out = append(out, string(ae.appendModifiers(nil)))
		}
	})
	return out
}

func TestConfluenceErrorDiagnostics(t *testing.T) {
	// Errors must preserve parse-context diagnostics (the offending token tail)
	// rather than opaque messages, and they must be returned, not panicked on.
	cases := []struct {
		q    string
		want string
	}{
		{`rate(http_requests_total[5m]`, `unparsed data`},
		{`sum(rate(`, `unexpected token`},
		{`http_requests_total{job=}`, `unexpected token`},
		{`http_requests_total{`, `unexpected token`},
	}
	for _, c := range cases {
		t.Run(seQueryName(c.q), func(t *testing.T) {
			if _, err := Parse(c.q); err == nil {
				t.Fatalf("Parse(%q) unexpectedly succeeded", c.q)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse(%q) error %q does not contain diagnostic %q", c.q, err, c.want)
			}
			if _, err := Prettify(c.q); err == nil {
				t.Fatalf("Prettify(%q) unexpectedly succeeded", c.q)
			}
		})
	}

	// A modifier keyword cannot start an expression; error must carry context.
	if _, err := Parse(`on(instance) foo`); err == nil {
		t.Fatalf("leading group modifier must be a parse error")
	}
	if _, err := Parse(`sum(x) by (a) keep_metric_names`); err == nil {
		t.Fatalf("keep_metric_names after an aggregate modifier must be a parse error")
	}
}

func TestConfluenceRegexpCache(t *testing.T) {
	// CompileRegexp caches both success and failure; a bad regexp returns the
	// same diagnostic on repeat without panicking, and a good one compiles.
	r1, err1 := CompileRegexp(`a+`)
	r2, err2 := CompileRegexp(`a+`)
	if err1 != nil || err2 != nil || r1 == nil || r1 != r2 {
		t.Fatalf("successful regexp must be cached and error-free; got %v %v %v %v", r1, err1, r2, err2)
	}
	bad := `[`
	_, e1 := CompileRegexp(bad)
	_, e2 := CompileRegexp(bad)
	if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
		t.Fatalf("invalid regexp error must be cached consistently; got %v %v", e1, e2)
	}
	// Anchored compilation used by selector matching.
	if _, err := CompileRegexpAnchored(`api.*`); err != nil {
		t.Fatalf("CompileRegexpAnchored: %s", err)
	}
}

// TestConfluencePushdownModifierTrim pins the pushdown-side barrier
// independently from the collection side: filters handed explicitly to
// PushdownBinaryOpFilters must still be trimmed by the binary op's group
// modifier before entering either operand. This protects the second layer of
// the defense-in-depth trimming (pushdownBinaryOpFiltersInplace on
// *BinaryOpExpr) even if the collection-side trim regressed.
func TestConfluencePushdownModifierTrim(t *testing.T) {
	u := newSEUniverse()
	q := `http_requests_total{job="api"} * on(instance) group_left() errors_total{job="api"}`
	e, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}
	// Pretend a non-signature label filter was handed to the pushdown API.
	filter := []LabelFilter{{Label: "route", Value: "/v1"}}
	out := PushdownBinaryOpFilters(e, filter)
	got := u.evalSE(out)
	want := u.evalSE(e)
	assertSameResult(t, "pushdown must trim route filter by on(instance)", got, want)
	text := string(out.AppendString(nil))
	if strings.Contains(text, `route="/v1"`) {
		t.Fatalf("non-signature filter leaked through on(instance): %s", text)
	}

	// The same filter on the signature label instance must be pushed and stay sound.
	filterOK := []LabelFilter{{Label: "instance", Value: "h1"}}
	outOK := PushdownBinaryOpFilters(e, filterOK)
	assertSameResult(t, "signature filter pushdown", u.evalSE(outOK), u.evalSE(outOK))
	if !strings.Contains(string(outOK.AppendString(nil)), `instance="h1"`) {
		t.Fatalf("signature filter was not pushed: %s", outOK.AppendString(nil))
	}
}
