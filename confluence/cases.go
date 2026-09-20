package confluence

import "github.com/VictoriaMetrics/metricsql"

// Case is one optimizer soundness scenario evaluated with the reference model.
type Case struct {
	// Name identifies the case in test output. It is semantic, never a
	// reference to an external fixture file.
	Name string
	// Query is parsed, optimized and compared against itself unoptimized.
	Query string
	// Universe is the finite series set the query is evaluated over.
	Universe Universe
	// ExpectChanged reports whether the optimizer is supposed to rewrite the
	// textual query. Sound cases may change the text; the dangerous cases
	// must stay untouched.
	ExpectChanged bool
}

// Cases covers nested rollups, binary joins, keep_metric_names and label
// manipulation. The first two cases are the most dangerous counterexamples:
// before the fix the optimizer rewrote the query and changed its result.
var Cases = []Case{
	{
		Name:          "prometheus_buckets-vmrange-to-le-join",
		Query:         `prometheus_buckets(foo_bucket) / on(le) bar_bucket{le="0.5"}`,
		ExpectChanged: false,
		Universe: Universe{
			Series{"__name__": "foo_bucket", "vmrange": "0.25...0.5"},
			Series{"__name__": "bar_bucket", "le": "0.5"},
		},
	},
	{
		Name:          "default-coalesce-keeps-right-only-series",
		Query:         `foo{x="1"} default bar{x="2"}`,
		ExpectChanged: false,
		Universe: Universe{
			Series{"__name__": "foo", "x": "1"},
			Series{"__name__": "bar", "x": "2"},
		},
	},
	{
		Name:          "default-common-filter-still-pushed",
		Query:         `foo{x="1",z="w"} default bar{x="2",z="w"}`,
		ExpectChanged: false,
		Universe: Universe{
			Series{"__name__": "foo", "x": "1", "z": "w"},
			Series{"__name__": "bar", "x": "2", "z": "w"},
		},
	},
	{
		Name:          "nested-rollup-aggr-pushdown-sound",
		Query:         `sum(rate(foo{job="a"}[5m])) by (instance,job) / on(instance,job) group_left() bar{job="b"}`,
		ExpectChanged: true,
		Universe: Universe{
			Series{"__name__": "foo", "job": "a", "instance": "i1"},
			Series{"__name__": "bar", "job": "b", "instance": "i1", "x": "y"},
		},
	},
	{
		Name:          "label_replace-dst-filter-not-pushed",
		Query:         `label_replace(foo, "dst", "$1", "src", "(.*)") + bar{dst="v"}`,
		ExpectChanged: false,
		Universe: Universe{
			Series{"__name__": "foo", "src": "v"},
			Series{"__name__": "bar", "dst": "v"},
		},
	},
	{
		Name:          "label_set-overwrite-passthrough",
		Query:         `label_set(foo, "job", "renamed") + bar{job="renamed"}`,
		ExpectChanged: false,
		Universe: Universe{
			Series{"__name__": "foo", "job": "other"},
			Series{"__name__": "bar", "job": "renamed"},
		},
	},
	{
		Name:          "label_del-source-filter-pushed",
		Query:         `label_del(foo, "tmp") + bar{x="y"}`,
		ExpectChanged: true,
		Universe: Universe{
			Series{"__name__": "foo", "x": "y", "tmp": "t"},
			Series{"__name__": "bar", "x": "y"},
		},
	},
	{
		Name:          "arithmetic-join-common-pushdown-sound",
		Query:         `foo{a="b"} + on(a) group_left(c) bar{c="d"}`,
		ExpectChanged: true,
		Universe: Universe{
			Series{"__name__": "foo", "a": "b"},
			Series{"__name__": "bar", "a": "b", "c": "d"},
		},
	},
}

// RoundTripCase verifies that Prettify output reparses to the same AST,
// including modifiers, keep_metric_names and label manipulation.
type RoundTripCase struct {
	Name  string
	Query string
}

// RoundTripCases contains queries exercising every modifier kind the
// prettifier can emit on long lines.
var RoundTripCases = []RoundTripCase{
	{
		Name:  "join-modifier-prefix-long",
		Query: `very_long_metric_name_aaaaaaaaaa{x="y"} default on(instance,job) group_left(pod) prefix "p_" other_very_long_metric_name_zzzzzzzzzz{zzzz="wwww"}`,
	},
	{
		Name:  "keep-metric-names-nested-long",
		Query: `(rate(very_long_metric_aaaaaaaaaa[5m]) keep_metric_names + label_del(other_metric_bbbbbbbbbb, "tmp")) keep_metric_names`,
	},
	{
		Name:  "group-join-fill-long",
		Query: `sum by(job)(rate(very_long_fooooooooooo[5m])) / on(job) group_left() fill_left(0) very_long_bbbbbbbbbbbbbb{xxxxx="yyyyy"}`,
	},
	{
		Name:  "escaped-metric-name-only-selector",
		Query: `{"3foobar_baz_namespace_pod_name_container_name_container_cpu_usage_seconds_total_sum_rate"}`,
	},
	{
		Name:  "embedded-quote-in-label-name",
		Query: `{foo="bar","la\"bel"="val"}`,
	},
	{
		Name:  "group-right-prefix-roundtrip",
		Query: `sum(rate(process_cpu_seconds_total{instance="foo",job="bar"}[5m] offset 1h @ start())) by (x) / on(x) group_right(y) prefix "x" sum(rate(node_cpu_seconds_total{mode!="idle"}[5m]) keep_metric_names)`,
	},
}

// MustParse parses q or panics with diagnostic context.
func MustParse(q string) metricsql.Expr {
	e, err := metricsql.Parse(q)
	if err != nil {
		panic("parse " + q + ": " + err.Error())
	}
	return e
}
