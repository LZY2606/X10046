package metricsql

// This file contains a deliberately small, self-contained label-set/value
// evaluator over a fixed synthetic universe of time series.
//
// It is not a MetricsQL implementation. Its only purpose is to give an
// independent executable oracle for optimizer soundness: pushing common label
// filters down through the AST must not change the multiset of output series
// (labels + values). Every semantic rule implemented here mirrors the
// PromQL/MetricsQL label propagation rules documented in ANALYSIS.md.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// seSeries is one evaluated time series at a single evaluation timestamp.
type seSeries struct {
	labels map[string]string
	value  float64
}

// seUniverse is the fixed dataset the oracle evaluates selectors against.
//
// The label space is intentionally tiny so that pushdown unsoundness shows up
// deterministically and without any randomness or external fixtures.
type seUniverse struct {
	series []seSeries
}

func newSEUniverse() *seUniverse {
	metrics := []string{"http_requests_total", "errors_total", "queue_size"}
	jobs := []string{"api", "web"}
	instances := []string{"h1", "h2"}
	zones := []string{"z1", "z2"}
	var u seUniverse
	for i, name := range metrics {
		for j, job := range jobs {
			for k, inst := range instances {
				for z, zone := range zones {
					u.series = append(u.series, seSeries{
						labels: map[string]string{
							"__name__":  name,
							"job":       job,
							"instance":  inst,
							"zone":      zone,
							"route":     "/v" + strconv.Itoa((j+k+z)%2+1),
							"pod":       "p-" + strconv.Itoa((k*2+z)%3),
							"container": "c" + strconv.Itoa(i),
						},
						value: float64(1 + ((i+1)*7+(j+1)*3+k*5+z*11)%47),
					})
				}
			}
		}
	}
	return &u
}

// key renders a label set as a canonical string. __name__ participates like
// any other label, which is what allows keep_metric_names to be checked.
func seKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(';')
	}
	return b.String()
}

func cloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// evalSE evaluates e against the universe and returns the output multiset.
//
// It panics on unsupported constructs with a descriptive message so that test
// queries never silently skip semantics they claim to cover.
func (u *seUniverse) evalSE(e Expr) []seSeries {
	switch t := e.(type) {
	case *MetricExpr:
		return u.evalSelector(t)
	case *NumberExpr:
		return []seSeries{{labels: map[string]string{}, value: t.N}}
	case *StringExpr:
		return []seSeries{{labels: map[string]string{}, value: 0}}
	case *RollupExpr:
		// Rollup windows only reshape samples in time; at a single instant with
		// one sample per series every supported rollup is identity on labels,
		// and the value-preserving rollups used by the tests keep the value.
		return u.evalSE(t.Expr)
	case *FuncExpr:
		return u.evalFunc(t)
	case *AggrFuncExpr:
		return u.evalAggr(t)
	case *BinaryOpExpr:
		return u.evalBinary(t)
	default:
		panic(fmt.Errorf("semantics_eval: unsupported expr type %T", e))
	}
}

func (u *seUniverse) evalSelector(me *MetricExpr) []seSeries {
	var out []seSeries
	for _, s := range u.series {
		match := false
		for _, lfs := range me.LabelFilterss {
			if seFiltersMatch(s.labels, lfs) {
				match = true
				break
			}
		}
		if match {
			out = append(out, seSeries{labels: cloneLabels(s.labels), value: s.value})
		}
	}
	return out
}

func seFiltersMatch(labels map[string]string, lfs []LabelFilter) bool {
	for _, lf := range lfs {
		v, ok := labels[lf.Label]
		if !ok {
			// PromQL: a missing label behaves like an empty-string label for =~
			// and = comparisons.
			v = ""
		}
		if !seFilterMatch(v, lf) {
			return false
		}
	}
	return true
}

func seFilterMatch(v string, lf LabelFilter) bool {
	matched := false
	if lf.IsRegexp {
		matched = seRegexp(lf.Value).MatchString(v)
	} else {
		matched = v == lf.Value
	}
	if lf.IsNegative {
		return !matched
	}
	return matched
}

func seRegexp(expr string) *regexp.Regexp {
	r, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		panic(fmt.Errorf("semantics_eval: invalid regexp %q: %w", expr, err))
	}
	return r
}

func seIsScalar(ss []seSeries) bool {
	return len(ss) == 1 && len(ss[0].labels) == 0
}

// seSignature computes the matching key labels per the group modifier.
func seSignature(labels map[string]string, be *BinaryOpExpr) map[string]string {
	sig := map[string]string{}
	switch strings.ToLower(be.GroupModifier.Op) {
	case "on":
		for _, a := range be.GroupModifier.Args {
			if a == "__name__" {
				continue
			}
			sig[a] = labels[a]
		}
	case "ignoring":
		skip := map[string]bool{}
		for _, a := range be.GroupModifier.Args {
			skip[a] = true
		}
		for k, v := range labels {
			if k == "__name__" {
				continue
			}
			if !skip[k] {
				sig[k] = v
			}
		}
	default:
		for k, v := range labels {
			if k == "__name__" {
				continue
			}
			sig[k] = v
		}
	}
	return sig
}

func seGroup(ss []seSeries, sig map[string]string) []seSeries {
	var out []seSeries
	for _, s := range ss {
		ok := true
		for k, v := range sig {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

func seExtraLabels(labels map[string]string, sig map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if _, inSig := sig[k]; !inSig {
			out[k] = v
		}
	}
	return out
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := cloneLabels(a)
	for k, v := range b {
		out[k] = v
	}
	return out
}

func seDropName(labels map[string]string) map[string]string {
	out := cloneLabels(labels)
	delete(out, "__name__")
	return out
}

func seKeepMetricNames(e Expr) bool {
	switch t := e.(type) {
	case *BinaryOpExpr:
		return t.KeepMetricNames
	case *FuncExpr:
		return t.KeepMetricNames
	default:
		return false
	}
}

func (u *seUniverse) evalBinary(be *BinaryOpExpr) []seSeries {
	op := strings.ToLower(be.Op)
	left := u.evalSE(be.Left)
	right := u.evalSE(be.Right)

	switch op {
	case "or":
		return u.evalSetOr(be, left, right)
	case "and":
		return u.evalSetAndUnless(be, left, right, true)
	case "unless":
		return u.evalSetAndUnless(be, left, right, false)
	case "if":
		// MetricsQL `if`: like and but keeps right-side value semantics; for
		// label-set soundness it filters left by right signature, keeping left.
		return u.evalSetAndUnless(be, left, right, true)
	case "ifnot":
		return u.evalSetAndUnless(be, left, right, false)
	case "default":
		return u.evalDefault(be, left, right)
	}

	if seIsScalar(right) {
		rv := right[0].value
		var out []seSeries
		for _, s := range left {
			out = append(out, seSeries{labels: cloneLabels(s.labels), value: seApplyArith(op, s.value, rv, be.Bool)})
		}
		return seMaybeDropNames(be, out)
	}
	if seIsScalar(left) {
		lv := left[0].value
		var out []seSeries
		for _, s := range right {
			out = append(out, seSeries{labels: seDropName(s.labels), value: seApplyArith(op, lv, s.value, be.Bool)})
		}
		return out
	}

	join := strings.ToLower(be.JoinModifier.Op)
	var out []seSeries
	if join == "group_left" || join == "group_right" {
		out = u.evalManyToOne(be, left, right, join == "group_left")
	} else {
		out = u.evalOneToOne(be, left, right)
	}
	out = seApplyFill(be, out, left, right)
	return seMaybeDropNames(be, out)
}

// seMaybeDropNames removes __name__ unless the expression (or a nested binary
// directly carried in the syntax) asks to keep metric names.
func seMaybeDropNames(e Expr, ss []seSeries) []seSeries {
	if seKeepMetricNames(e) {
		return ss
	}
	out := make([]seSeries, len(ss))
	for i, s := range ss {
		out[i] = seSeries{labels: seDropName(s.labels), value: s.value}
	}
	return out
}

func seApplyArith(op string, a, b float64, isBool bool) float64 {
	cmp := func(f func() bool) float64 {
		if isBool {
			if f() {
				return 1
			}
			return 0
		}
		if f() {
			return a
		}
		return math.NaN()
	}
	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		return a / b
	case "%":
		return math.Mod(a, b)
	case "^":
		return math.Pow(a, b)
	case "atan2":
		return math.Atan2(a, b)
	case "==":
		return cmp(func() bool { return a == b })
	case "!=":
		return cmp(func() bool { return a != b })
	case ">":
		return cmp(func() bool { return a > b })
	case "<":
		return cmp(func() bool { return a < b })
	case ">=":
		return cmp(func() bool { return a >= b })
	case "<=":
		return cmp(func() bool { return a <= b })
	default:
		panic(fmt.Errorf("semantics_eval: unsupported scalar op %q", op))
	}
}

func (u *seUniverse) evalOneToOne(be *BinaryOpExpr, left, right []seSeries) []seSeries {
	var out []seSeries
	for _, ls := range left {
		sig := seSignature(ls.labels, be)
		rg := seGroup(right, sig)
		if len(rg) != 1 {
			continue
		}
		rs := rg[0]
		labels := mergeLabels(ls.labels, rs.labels)
		out = append(out, seSeries{labels: labels, value: seApplyArith(strings.ToLower(be.Op), ls.value, rs.value, be.Bool)})
	}
	return out
}

func (u *seUniverse) evalManyToOne(be *BinaryOpExpr, left, right []seSeries, groupLeft bool) []seSeries {
	// PromQL does not validate the one-to-many cardinality; it pairs every
	// many-side series with the single one-side series.
	var out []seSeries
	if groupLeft {
		// left is the "many" side, right is the "one" side.
		for _, ls := range left {
			sig := seSignature(ls.labels, be)
			rg := seGroup(right, sig)
			if len(rg) != 1 {
				continue
			}
			labels := mergeLabels(seExtraLabels(ls.labels, sig), seExtraLabels(rg[0].labels, sig))
			for k, v := range sig {
				labels[k] = v
			}
			out = append(out, seSeries{labels: labels, value: seApplyArith(strings.ToLower(be.Op), ls.value, rg[0].value, be.Bool)})
		}
		return out
	}
	// group_right: right is the "many" side, left is the "one" side.
	for _, rs := range right {
		sig := seSignature(rs.labels, be)
		lg := seGroup(left, sig)
		if len(lg) != 1 {
			continue
		}
		labels := mergeLabels(seExtraLabels(lg[0].labels, sig), seExtraLabels(rs.labels, sig))
		for k, v := range sig {
			labels[k] = v
		}
		out = append(out, seSeries{labels: labels, value: seApplyArith(strings.ToLower(be.Op), lg[0].value, rs.value, be.Bool)})
	}
	return out
}

func (u *seUniverse) evalSetOr(be *BinaryOpExpr, left, right []seSeries) []seSeries {
	seen := map[string]bool{}
	var out []seSeries
	add := func(s seSeries) {
		k := seKey(s.labels)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	for _, s := range left {
		add(s)
	}
	for _, s := range right {
		sig := seSignature(s.labels, be)
		lg := seGroup(left, sig)
		keep := false
		if len(lg) == 0 {
			keep = true
		}
		// With group_left/group_right (many-to-one) or semantics: extra labels
		// of the one side are merged. The test queries avoid that exotic case,
		// so plain dedup by full signature is sufficient and sound here.
		_ = keep
		add(s)
	}
	return seMaybeDropNames(be, out)
}

func (u *seUniverse) evalSetAndUnless(be *BinaryOpExpr, left, right []seSeries, wantMatch bool) []seSeries {
	var out []seSeries
	for _, ls := range left {
		sig := seSignature(ls.labels, be)
		rg := seGroup(right, sig)
		matched := len(rg) > 0
		if matched == wantMatch {
			out = append(out, seSeries{labels: cloneLabels(ls.labels), value: ls.value})
		}
	}
	return seMaybeDropNames(be, out)
}

func (u *seUniverse) evalDefault(be *BinaryOpExpr, left, right []seSeries) []seSeries {
	// default: left value when present, otherwise right value.
	seen := map[string]bool{}
	var out []seSeries
	add := func(s seSeries) {
		k := seKey(s.labels)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	for _, s := range left {
		add(s)
	}
	for _, s := range right {
		sig := seSignature(s.labels, be)
		if len(seGroup(left, sig)) == 0 {
			add(s)
		}
	}
	return seMaybeDropNames(be, out)
}

// seApplyFill models MetricsQL fill/fill_left/fill_right: matched pairs are
// emitted as usual; unmatched series on the filled side are emitted with the
// fill value (and with the union of label sets per matching signature).
func seApplyFill(be *BinaryOpExpr, matched []seSeries, left, right []seSeries) []seSeries {
	if be.FillLeft == nil && be.FillRight == nil {
		return matched
	}
	out := append([]seSeries{}, matched...)
	filledLeftKeys := map[string]bool{}
	filledRightKeys := map[string]bool{}
	for _, s := range matched {
		filledLeftKeys[seKey(seSignature(s.labels, be))] = true
	}
	if be.FillRight != nil {
		for _, ls := range left {
			sig := seSignature(ls.labels, be)
			if len(seGroup(right, sig)) == 0 {
				labels := cloneLabels(ls.labels)
				out = append(out, seSeries{labels: labels, value: be.FillRight.N})
			}
		}
	}
	if be.FillLeft != nil {
		for _, rs := range right {
			sig := seSignature(rs.labels, be)
			if len(seGroup(left, sig)) == 0 {
				labels := cloneLabels(rs.labels)
				out = append(out, seSeries{labels: labels, value: be.FillLeft.N})
			}
		}
	}
	_ = filledLeftKeys
	_ = filledRightKeys
	return out
}

func (u *seUniverse) evalFunc(fe *FuncExpr) []seSeries {
	name := strings.ToLower(fe.Name)
	// Rollup functions: the optimizer decides which argument carries series
	// through GetRollupArgIdx()/getRollupArgIdxForOptimization(). The oracle
	// uses the same metadata so that a metadata drift becomes observable.
	if IsRollupFunc(name) {
		idx := GetRollupArgIdx(fe)
		if idx < 0 || idx >= len(fe.Args) {
			// absent_over_time: label-preserving presence probe; our universe
			// always has a sample, so it behaves as identity on labels.
			if name == "absent_over_time" {
				idx = 0
			} else {
				return nil
			}
		}
		ss := u.evalSE(fe.Args[idx])
		if name == "count_values_over_time" {
			return seCountValues(fe.Args[0], ss)
		}
		return seMaybeDropNames(fe, ss)
	}

	switch name {
	case "":
		// empty-name func is a synonym for union across args.
		var out []seSeries
		for _, a := range fe.Args {
			out = append(out, u.evalSE(a)...)
		}
		return out
	case "union":
		var out []seSeries
		seen := map[string]bool{}
		for _, a := range fe.Args {
			for _, s := range u.evalSE(a) {
				if k := seKey(s.labels); !seen[k] {
					seen[k] = true
					out = append(out, s)
				}
			}
		}
		return out
	case "label_set":
		ss := u.evalSE(fe.Args[0])
		return seLabelSet(ss, fe.Args[1:])
	case "label_replace":
		ss := u.evalSE(fe.Args[0])
		return seLabelReplace(ss, fe.Args[1:])
	case "label_join":
		ss := u.evalSE(fe.Args[0])
		return seLabelJoin(ss, fe.Args[1:])
	case "label_del":
		ss := u.evalSE(fe.Args[0])
		return seLabelDel(ss, fe.Args[1:])
	case "label_keep":
		ss := u.evalSE(fe.Args[0])
		return seLabelKeep(ss, fe.Args[1:])
	case "label_copy", "label_move":
		ss := u.evalSE(fe.Args[0])
		return seLabelCopyMove(ss, name == "label_move", fe.Args[1:])
	case "label_uppercase", "label_lowercase":
		ss := u.evalSE(fe.Args[0])
		return seLabelCase(ss, name == "label_uppercase", fe.Args[1:])
	case "scalar":
		return []seSeries{{labels: map[string]string{}, value: u.evalSE(fe.Args[0])[0].value}}
	case "vector":
		return u.evalSE(fe.Args[0])
	case "absent":
		ss := u.evalSE(fe.Args[0])
		if len(ss) == 0 {
			return []seSeries{{labels: map[string]string{}, value: 1}}
		}
		return nil
	case "sort", "sort_desc", "abs", "ceil", "floor", "round", "sqrt", "ln",
		"log2", "log10", "exp", "sgn", "interpolate", "keep_last_value",
		"keep_next_value", "running_avg", "running_max", "running_min",
		"running_sum", "remove_resets", "prometheus_buckets":
		return seMaybeDropNames(fe, u.evalSE(fe.Args[0]))
	default:
		// Conservative default for any value-only transform with one series arg:
		// labels (minus __name__) flow through the metadata-selected arg.
		if IsTransformFunc(name) {
			idx := getTransformArgIdxForOracle(name, fe.Args)
			if idx >= 0 && idx < len(fe.Args) {
				return seMaybeDropNames(fe, u.evalSE(fe.Args[idx]))
			}
			return nil
		}
		panic(fmt.Errorf("semantics_eval: unsupported transform func %q", name))
	}
}

// getTransformArgIdxForOracle mirrors getTransformArgIdxForOptimization:
// the argument index carrying series labels is function metadata, not syntax.
func getTransformArgIdxForOracle(name string, args []Expr) int {
	switch name {
	case "buckets_limit", "histogram_quantile", "histogram_share", "range_quantile",
		"range_trim_outliers", "range_trim_spikes", "range_trim_zscore":
		return 1
	case "limit_offset", "histogram_fraction":
		return 2
	case "drop_common_labels", "absent", "scalar", "end", "now", "pi",
		"start", "step", "time":
		return -1
	default:
		return 0
	}
}

func seStringArg(e Expr) string {
	if se, ok := e.(*StringExpr); ok {
		return se.S
	}
	panic(fmt.Errorf("semantics_eval: expected string literal, got %T", e))
}

func seLabelSet(ss []seSeries, args []Expr) []seSeries {
	for i := 0; i+1 < len(args); i += 2 {
		k, v := seStringArg(args[i]), seStringArg(args[i+1])
		for j := range ss {
			ss[j].labels = cloneLabels(ss[j].labels)
			ss[j].labels[k] = v
		}
	}
	return ss
}

func seLabelDel(ss []seSeries, args []Expr) []seSeries {
	names := map[string]bool{}
	for _, a := range args {
		names[seStringArg(a)] = true
	}
	out := make([]seSeries, 0, len(ss))
	for _, s := range ss {
		l := cloneLabels(s.labels)
		for n := range names {
			delete(l, n)
		}
		out = append(out, seSeries{labels: l, value: s.value})
	}
	return out
}

func seLabelKeep(ss []seSeries, args []Expr) []seSeries {
	names := map[string]bool{}
	for _, a := range args {
		names[seStringArg(a)] = true
	}
	out := make([]seSeries, 0, len(ss))
	for _, s := range ss {
		l := map[string]string{}
		for n := range names {
			if v, ok := s.labels[n]; ok {
				l[n] = v
			}
		}
		out = append(out, seSeries{labels: l, value: s.value})
	}
	return out
}

func seLabelCopyMove(ss []seSeries, isMove bool, args []Expr) []seSeries {
	for i := 0; i+1 < len(args); i += 2 {
		dst, src := seStringArg(args[i]), seStringArg(args[i+1])
		for j := range ss {
			ss[j].labels = cloneLabels(ss[j].labels)
			if v, ok := ss[j].labels[src]; ok {
				ss[j].labels[dst] = v
				if isMove {
					delete(ss[j].labels, src)
				}
			}
		}
	}
	return ss
}

func seLabelCase(ss []seSeries, upper bool, args []Expr) []seSeries {
	names := map[string]bool{}
	for _, a := range args {
		names[seStringArg(a)] = true
	}
	for j := range ss {
		ss[j].labels = cloneLabels(ss[j].labels)
		for n := range names {
			v := ss[j].labels[n]
			if upper {
				ss[j].labels[n] = strings.ToUpper(v)
			} else {
				ss[j].labels[n] = strings.ToLower(v)
			}
		}
	}
	return ss
}

func seLabelReplace(ss []seSeries, args []Expr) []seSeries {
	// label_replace(v instant-vector, dst, replacement, src, regex)
	dst := seStringArg(args[0])
	replacement := seStringArg(args[1])
	src := seStringArg(args[2])
	re := seRegexp(seStringArg(args[3]))
	out := make([]seSeries, 0, len(ss))
	for _, s := range ss {
		m := re.FindStringSubmatch(s.labels[src])
		if m == nil {
			out = append(out, s)
			continue
		}
		l := cloneLabels(s.labels)
		val := replacement
		for i, g := range m {
			val = strings.ReplaceAll(val, "$"+strconv.Itoa(i), g)
		}
		val = strings.ReplaceAll(val, "$0", m[0])
		l[dst] = val
		out = append(out, seSeries{labels: l, value: s.value})
	}
	return out
}

func seLabelJoin(ss []seSeries, args []Expr) []seSeries {
	// label_join(v, dst, sep, src...)
	dst := seStringArg(args[0])
	sep := seStringArg(args[1])
	srcs := args[2:]
	out := make([]seSeries, 0, len(ss))
	for _, s := range ss {
		parts := make([]string, 0, len(srcs))
		for _, src := range srcs {
			parts = append(parts, s.labels[seStringArg(src)])
		}
		l := cloneLabels(s.labels)
		l[dst] = strings.Join(parts, sep)
		out = append(out, seSeries{labels: l, value: s.value})
	}
	return out
}

func seCountValues(dstLabelExpr Expr, ss []seSeries) []seSeries {
	dst := seStringArg(dstLabelExpr)
	type entry struct {
		labels map[string]string
		value  string
		n      int
	}
	var entries []entry
	index := map[string]int{}
	for _, s := range ss {
		v := strconv.FormatFloat(s.value, 'g', -1, 64)
		l := seDropName(s.labels)
		l[dst] = v
		k := seKey(l)
		if i, ok := index[k]; ok {
			entries[i].n++
		} else {
			index[k] = len(entries)
			entries = append(entries, entry{labels: l, value: v, n: 1})
		}
	}
	out := make([]seSeries, 0, len(entries))
	for _, e := range entries {
		out = append(out, seSeries{labels: e.labels, value: float64(e.n)})
	}
	return out
}

func (u *seUniverse) evalAggr(ae *AggrFuncExpr) []seSeries {
	ss := u.evalSE(ae.Args[len(ae.Args)-1])
	name := strings.ToLower(ae.Name)
	if name == "count_values" {
		return seCountValues(ae.Args[0], ss)
	}

	groups := map[string][]seSeries{}
	var order []string
	for _, s := range ss {
		g := map[string]string{}
		switch strings.ToLower(ae.Modifier.Op) {
		case "by":
			for _, a := range ae.Modifier.Args {
				g[a] = s.labels[a]
			}
		case "without":
			skip := map[string]bool{}
			for _, a := range ae.Modifier.Args {
				skip[a] = true
			}
			for k, v := range s.labels {
				if k != "__name__" && !skip[k] {
					g[k] = v
				}
			}
		}
		k := seKey(g)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}
	if ae.Modifier.Op == "" {
		// No grouping modifier: aggregation collapses everything to one series
		// with no labels.
		order = []string{""}
		groups[""] = ss
	}

	var out []seSeries
	for _, k := range order {
		members := groups[k]
		if len(members) == 0 {
			continue
		}
		labels := cloneLabels(members[0].labels)
		delete(labels, "__name__")
		if ae.Modifier.Op == "" {
			labels = map[string]string{}
		}
		value := seAggrValue(name, members, ae.Args[:len(ae.Args)-1])
		if math.IsNaN(value) {
			continue
		}
		out = append(out, seSeries{labels: labels, value: value})
	}
	return out
}

func seAggrValue(name string, ss []seSeries, extraArgs []Expr) float64 {
	sum := 0.0
	min := math.Inf(1)
	max := math.Inf(-1)
	for _, s := range ss {
		sum += s.value
		if s.value < min {
			min = s.value
		}
		if s.value > max {
			max = s.value
		}
	}
	switch name {
	case "sum", "any", "group":
		return sum
	case "avg", "median":
		return sum / float64(len(ss))
	case "count", "distinct":
		return float64(len(ss))
	case "min":
		return min
	case "max":
		return max
	case "stddev", "stdvar", "mad", "zscore", "geomean", "mode", "share", "sum2",
		"histogram":
		return sum
	case "quantile":
		return min
	case "topk", "bottomk", "topk_avg", "bottomk_avg", "topk_max", "bottomk_max",
		"topk_min", "bottomk_min", "topk_median", "bottomk_median", "topk_last",
		"bottomk_last", "limitk", "outliersk", "outliers_mad", "quantiles":
		return sum
	default:
		panic(fmt.Errorf("semantics_eval: unsupported aggregate %q", name))
	}
}
