// Command metricsql-confluence is the executable counter-evidence for the
// MetricsQL "function metadata / optimizer pushdown / prettifier write-back"
// semantic confluence work.
//
// It exits with status 0 when every scenario is sound and with status 1 when
// a rewrite changes the selected series set or a prettify/parse round trip
// loses information. It uses no network, no sleeps and no host paths.
package main

import (
	"fmt"
	"os"

	"github.com/VictoriaMetrics/metricsql"

	"github.com/VictoriaMetrics/metricsql/confluence"
)

func main() {
	failures := 0

	fmt.Println("== optimizer soundness (reference series-set semantics) ==")
	for _, tc := range confluence.Cases {
		orig := confluence.MustParse(tc.Query)
		opt := metricsql.Optimize(orig)

		want := confluence.Eval(orig, tc.Universe)
		got := confluence.Eval(opt, tc.Universe)
		status := "sound"
		if !confluence.EqualSeriesSets(want, got) {
			status = "UNSOUND: " + confluence.Diff(want, got)
			failures++
		}
		fmt.Printf("- %-45s %s\n    -> %s\n", tc.Name, status, string(opt.AppendString(nil)))
	}

	fmt.Println("\n== prettify -> parse round trips (modifier preservation) ==")
	for _, tc := range confluence.RoundTripCases {
		e, err := metricsql.Parse(tc.Query)
		if err != nil {
			fmt.Printf("- %-45s PARSE ERROR: %s\n", tc.Name, err)
			failures++
			continue
		}
		canonical := string(e.AppendString(nil))
		pretty, err := metricsql.Prettify(tc.Query)
		if err != nil {
			fmt.Printf("- %-45s PRETTIFY ERROR: %s\n", tc.Name, err)
			failures++
			continue
		}
		e2, err := metricsql.Parse(pretty)
		if err != nil {
			fmt.Printf("- %-45s REPARSE ERROR: %s\n", tc.Name, err)
			failures++
			continue
		}
		status := "preserved"
		if string(e2.AppendString(nil)) != canonical {
			status = "CHANGED"
			failures++
		}
		fmt.Printf("- %-45s %s\n", tc.Name, status)
	}

	if failures > 0 {
		fmt.Printf("\n%d confluence violation(s)\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall confluence scenarios hold")
}
