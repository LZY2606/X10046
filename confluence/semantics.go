// Package confluence provides a tiny reference model for proving that
// metricsql.Optimize rewrites preserve the selected series set and the
// resulting label space.
//
// It deliberately models only label-set semantics (which series survive and
// which labels they carry); sample values are irrelevant to the optimizer's
// filter pushdown and to label-matching modifiers.
package confluence

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
)

// Series is one time series, identified by its label set (including __name__).
type Series map[string]string

// Universe is the finite set of series the reference model evaluates against.
type Universe []Series

func cloneLabels(in Series) Series {
	out := make(Series, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func key(s Series) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(s[k])
		b.WriteByte(';')
	}
	return b.String()
}

func dedup(in []Series) []Series {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, s := range in {
		k := key(s)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

func matchFilter(s Series, lf metricsql.LabelFilter) bool {
	v, present := s[lf.Label]
	eq := false
	if lf.IsRegexp {
		re, err := regexp.Compile("^(?:" + lf.Value + ")$")
		if err != nil {
			panic(fmt.Sprintf("invalid regexp %q: %s", lf.Value, err))
		}
		eq = present && re.MatchString(v)
	} else {
		eq = present && v == lf.Value
	}
	if lf.IsNegative {
		return !eq
	}
	return eq
}

func matchesAll(s Series, lfs []metricsql.LabelFilter) bool {
	for _, lf := range lfs {
		if !matchFilter(s, lf) {
			return false
		}
	}
	return true
}

func matchingKeys(s, other Series, be *metricsql.BinaryOpExpr) bool {
	switch strings.ToLower(be.GroupModifier.Op) {
	case "on":
		for _, k := range be.GroupModifier.Args {
			if s[k] != other[k] {
				return false
			}
		}
		return true
	case "ignoring":
		skip := make(map[string]struct{}, len(be.GroupModifier.Args))
		for _, k := range be.GroupModifier.Args {
			skip[k] = struct{}{}
		}
		for k, v := range s {
			if _, ok := skip[k]; ok {
				continue
			}
			if _, ok := other[k]; !ok || other[k] != v {
				return false
			}
		}
		for k := range other {
			if _, ok := skip[k]; ok {
				continue
			}
			if _, ok := s[k]; !ok {
				return false
			}
		}
		return true
	default:
		for k, v := range s {
			if _, ok := other[k]; !ok || other[k] != v {
				return false
			}
		}
		for k := range other {
			if _, ok := s[k]; !ok {
				return false
			}
		}
		return true
	}
}

func stringConst(e metricsql.Expr) (string, bool) {
	se, ok := e.(*metricsql.StringExpr)
	if !ok {
		return "", false
	}
	return se.S, true
}

// Eval computes the resulting series set of e against universe u.
func Eval(e metricsql.Expr, u Universe) []Series {
	switch t := e.(type) {
	case *metricsql.MetricExpr:
		var out []Series
		for _, s := range u {
			for _, lfs := range t.LabelFilterss {
				if matchesAll(s, lfs) {
					out = append(out, cloneLabels(s))
					break
				}
			}
		}
		return out

	case *metricsql.NumberExpr:
		return []Series{{}}
	case *metricsql.StringExpr:
		return []Series{{}}

	case *metricsql.RollupExpr:
		// Window/offset/@ do not change which series match.
		return Eval(t.Expr, u)

	case *metricsql.FuncExpr:
		return evalFunc(t, u)

	case *metricsql.AggrFuncExpr:
		return evalAggr(t, u)

	case *metricsql.BinaryOpExpr:
		return evalBinary(t, u)

	default:
		panic(fmt.Sprintf("unsupported expr type %T", e))
	}
}

func evalFunc(fe *metricsql.FuncExpr, u Universe) []Series {
	name := strings.ToLower(fe.Name)
	argSets := make([][]Series, len(fe.Args))
	for i, a := range fe.Args {
		argSets[i] = Eval(a, u)
	}
	passthrough := func(idx int) []Series {
		if idx < 0 || idx >= len(fe.Args) {
			return nil
		}
		return argSets[idx]
	}

	switch name {
	case "label_set":
		out := append([]Series{}, argSets[0]...)
		for i := 1; i+1 < len(fe.Args); i += 2 {
			label, ok1 := stringConst(fe.Args[i])
			value, ok2 := stringConst(fe.Args[i+1])
			if !ok1 || !ok2 {
				continue
			}
			for _, s := range out {
				s[label] = value
			}
		}
		return dedup(out)

	case "label_del":
		labels := constStrings(fe.Args[1:])
		out := append([]Series{}, argSets[0]...)
		for _, s := range out {
			for _, label := range labels {
				delete(s, label)
			}
		}
		return dedup(out)

	case "label_keep":
		labels := make(map[string]struct{})
		for _, l := range constStrings(fe.Args[1:]) {
			labels[l] = struct{}{}
		}
		var out []Series
		for _, s := range argSets[0] {
			ns := Series{}
			for k, v := range s {
				if _, ok := labels[k]; ok {
					ns[k] = v
				}
			}
			out = append(out, ns)
		}
		return dedup(out)

	case "label_replace":
		if len(fe.Args) < 5 {
			return argSets[0]
		}
		dst, _ := stringConst(fe.Args[1])
		repl, _ := stringConst(fe.Args[2])
		src, _ := stringConst(fe.Args[3])
		reText, _ := stringConst(fe.Args[4])
		re, err := regexp.Compile("^(?:" + reText + ")$")
		if err != nil {
			return argSets[0]
		}
		var out []Series
		for _, s := range argSets[0] {
			ns := cloneLabels(s)
			if m := re.FindStringSubmatch(s[src]); m != nil {
				v := repl
				for i := 1; i < len(m); i++ {
					v = strings.ReplaceAll(v, fmt.Sprintf("$%d", i), m[i])
				}
				ns[dst] = v
			}
			out = append(out, ns)
		}
		return dedup(out)

	case "prometheus_buckets":
		// Converts each input vmrange range into an le bucket: the upper bound
		// of "lower...upper" becomes the le value. Input series carry vmrange,
		// so an le=... filter pushed onto the input matches nothing.
		var out []Series
		for _, s := range passthrough(0) {
			ns := cloneLabels(s)
			if r, ok := ns["vmrange"]; ok {
				delete(ns, "vmrange")
				if i := strings.LastIndex(r, "..."); i >= 0 {
					ns["le"] = r[i+3:]
				}
			}
			out = append(out, ns)
		}
		return dedup(out)

	case "absent", "scalar":
		return []Series{{}}

	default:
		// Every other modelled transform/rollup (rate, sum_over_time, abs,
		// histogram_quantile, vector, ...) preserves the series set of its
		// rollup/main argument.
		return passthrough(funcArgIdx(name, len(fe.Args)))
	}
}

func funcArgIdx(name string, nArgs int) int {
	switch name {
	case "quantile_over_time", "aggr_over_time",
		"hoeffding_bound_lower", "hoeffding_bound_upper":
		return 1
	case "quantiles_over_time", "histogram_quantiles":
		return nArgs - 1
	case "limit_offset", "histogram_fraction":
		return 2
	case "buckets_limit", "histogram_quantile", "histogram_share",
		"range_quantile", "range_trim_outliers", "range_trim_spikes",
		"range_trim_zscore":
		return 1
	default:
		return 0
	}
}

func constStrings(args []metricsql.Expr) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if s, ok := stringConst(a); ok {
			out = append(out, s)
		}
	}
	return out
}

func groupingLabels(s Series, args []string, op string) Series {
	out := Series{}
	switch strings.ToLower(op) {
	case "by":
		for _, k := range args {
			if v, ok := s[k]; ok {
				out[k] = v
			}
		}
	case "without":
		drop := make(map[string]struct{}, len(args))
		for _, k := range args {
			drop[k] = struct{}{}
		}
		for k, v := range s {
			if k == "__name__" {
				continue
			}
			if _, ok := drop[k]; !ok {
				out[k] = v
			}
		}
	default:
		// No modifier aggregates everything into one unnamed series.
		return Series{}
	}
	return out
}

func evalAggr(ae *metricsql.AggrFuncExpr, u Universe) []Series {
	name := strings.ToLower(ae.Name)
	var sets [][]Series
	dataIdx := 0
	switch name {
	case "count_values":
		dataIdx = 1
	case "bottomk", "topk", "limitk", "quantile", "outliersk", "outliers_mad",
		"bottomk_avg", "bottomk_max", "bottomk_median", "bottomk_last", "bottomk_min",
		"topk_avg", "topk_max", "topk_median", "topk_last", "topk_min":
		dataIdx = 1
	case "quantiles":
		dataIdx = len(ae.Args) - 1
	}
	for i, a := range ae.Args {
		s := Eval(a, u)
		// topk/bottomk/quantile style funcs keep the per-series label set of
		// their data argument; multi-arg aggregations merge arguments.
		if i == dataIdx && name != "count_values" && isSelectorAggr(name) {
			return s
		}
		sets = append(sets, s)
	}
	if name == "count_values" {
		labelName, _ := stringConst(ae.Args[0])
		var out []Series
		for _, s := range sets[dataIdx] {
			ns := groupingLabels(s, ae.Modifier.Args, ae.Modifier.Op)
			ns[labelName] = "value"
			out = append(out, ns)
		}
		return dedup(out)
	}
	seen := make(map[string]struct{})
	var out []Series
	for _, set := range sets {
		for _, s := range set {
			ns := groupingLabels(s, ae.Modifier.Args, ae.Modifier.Op)
			k := key(ns)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, ns)
		}
	}
	return out
}

func isSelectorAggr(name string) bool {
	switch name {
	case "topk", "bottomk", "limitk", "quantile", "outliersk", "outliers_mad",
		"bottomk_avg", "bottomk_max", "bottomk_median", "bottomk_last", "bottomk_min",
		"topk_avg", "topk_max", "topk_median", "topk_last", "topk_min":
		return true
	default:
		return false
	}
}

func isSetOp(op string) bool {
	switch strings.ToLower(op) {
	case "or", "and", "unless", "if", "ifnot", "default":
		return true
	default:
		return false
	}
}

func joinResultLabels(l, r Series, be *metricsql.BinaryOpExpr) Series {
	out := cloneLabels(l)
	switch strings.ToLower(be.JoinModifier.Op) {
	case "group_left":
		for _, k := range be.JoinModifier.Args {
			if k == "*" {
				for kk, vv := range r {
					if kk == "__name__" {
						continue
					}
					if _, exists := out[kk]; !exists {
						out[kk] = vv
					}
				}
				continue
			}
			if v, ok := r[k]; ok {
				out[k] = v
			}
		}
	case "group_right":
		out = cloneLabels(r)
		for _, k := range be.JoinModifier.Args {
			if k == "*" {
				for kk, vv := range l {
					if kk == "__name__" {
						continue
					}
					if _, exists := out[kk]; !exists {
						out[kk] = vv
					}
				}
				continue
			}
			if v, ok := l[k]; ok {
				out[k] = v
			}
		}
	}
	return out
}

func evalBinary(be *metricsql.BinaryOpExpr, u Universe) []Series {
	left := Eval(be.Left, u)
	right := Eval(be.Right, u)
	op := strings.ToLower(be.Op)

	switch op {
	case "or":
		return dedup(append(append([]Series{}, left...), right...))
	case "default":
		// Coalesce: for every matching signature, the left series wins; series
		// existing only on the right are taken from there.
		seen := make(map[string]struct{}, len(left))
		out := append([]Series{}, left...)
		for _, s := range left {
			seen[key(s)] = struct{}{}
		}
		for _, s := range right {
			if _, ok := seen[key(s)]; !ok {
				out = append(out, s)
			}
		}
		return dedup(out)
	case "and":
		var out []Series
		for _, l := range left {
			for _, r := range right {
				if matchingKeys(l, r, be) {
					out = append(out, cloneLabels(l))
					break
				}
			}
		}
		return dedup(out)
	case "if":
		var out []Series
		for _, l := range left {
			for _, r := range right {
				if matchingKeys(l, r, be) {
					out = append(out, cloneLabels(l))
					break
				}
			}
		}
		return dedup(out)
	case "unless", "ifnot":
		var out []Series
		for _, l := range left {
			matched := false
			for _, r := range right {
				if matchingKeys(l, r, be) {
					matched = true
					break
				}
			}
			if !matched {
				out = append(out, cloneLabels(l))
			}
		}
		return dedup(out)
	}

	// Arithmetic / comparison vector matching.
	var out []Series
	for _, l := range left {
		for _, r := range right {
			if !matchingKeys(l, r, be) {
				continue
			}
			out = append(out, joinResultLabels(l, r, be))
		}
	}
	return dedup(out)
}

// EqualSeriesSets reports whether two series sets are identical.
func EqualSeriesSets(a, b []Series) bool {
	a = dedup(append([]Series{}, a...))
	b = dedup(append([]Series{}, b...))
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[key(s)]++
	}
	for _, s := range b {
		k := key(s)
		if seen[k] == 0 {
			return false
		}
		seen[k]--
	}
	return true
}

// Diff returns a human-readable description of the first difference between
// the two series sets, or "" when they are equal.
func Diff(want, got []Series) string {
	wantK := map[string]struct{}{}
	gotK := map[string]struct{}{}
	for _, s := range want {
		wantK[key(s)] = struct{}{}
	}
	for _, s := range got {
		gotK[key(s)] = struct{}{}
	}
	var missing, extra []string
	for k := range wantK {
		if _, ok := gotK[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range gotK {
		if _, ok := wantK[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	var b strings.Builder
	if len(missing) > 0 {
		b.WriteString("missing after rewrite: " + strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString("introduced by rewrite: " + strings.Join(extra, ", "))
	}
	return b.String()
}
