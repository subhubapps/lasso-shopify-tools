// Command reducer turns a multi-GB Shopify Partner "Earnings" CSV into neutral
// revenue rows for Lasso, in a single streaming pass with memory bounded by the
// aggregate (leads x months x currencies), not the source row count. It is
// stdlib-only and builds static with CGO_ENABLED=0.
//
// Usage:
//
//	reducer --src earnings.csv --store-map map.json [--prefix loop-monthly] --out ./out
//	reducer --version
//
// Shopify Partner "Earnings" CSV schema (resolved case-insensitively by header
// NAME on the first record, order-independent - see reducer/README.md). These
// are the exact columns 02_build_per_charge.rb reads. Required columns:
//
//	Shop                             - shop domain (joined to the store map)
//	Charge ID                        - the charge GID; empty => refund/credit, skipped
//	Charge Creation Time             - ISO-8601 / space-separated timestamp
//	Partner Share                    - partner earnings in the sale currency
//	Partner Sale                     - sale amount in the sale currency
//	Partner Sale In Payout Currency  - sale amount in the payout currency
//
// Optional column (absent => USD assumed for every row, with a warning):
//
//	Payout Currency                  - blank values default to USD, with a warning
//
// Classification is by the Charge ID GID (matching 03_aggregate_monthly.rb): a
// Charge ID containing "AppSubscription" is a SUBSCRIPTION, one containing
// "AppUsageRecord" is a USAGE record, anything else is OTHER. The Charge Type
// and Category columns are diagnostic-only: never required, never used to
// classify. Extra columns in the export are ignored.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"lasso-shopify-tools/reducer/reduce"
)

// version is stamped at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns the process exit code: 0 on
// success (including the unsafe-guardrail case, which warns on stderr but still
// completes), and 1 on usage, IO, malformed-map, missing-column, and parse
// errors.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reducer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: reducer --src <earnings.csv> --store-map <map.json> [--prefix <prefix>] --out <dir>")
		fmt.Fprintln(stderr, "       reducer --version")
		fs.PrintDefaults()
	}

	var (
		src         = fs.String("src", "", "path to the Shopify Partner earnings CSV")
		storeMap    = fs.String("store-map", "", "path to the saved account_shopify_store_map JSON")
		prefix      = fs.String("prefix", "", "import prefix override (defaults to the store map's importPrefix)")
		out         = fs.String("out", "", "output directory for aggregate.jsonl, unmatched_shops.csv, preview.json")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) { // -h/--help printed usage already
			return 0
		}
		return 1
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	var missing []string
	if *src == "" {
		missing = append(missing, "--src")
	}
	if *storeMap == "" {
		missing = append(missing, "--store-map")
	}
	if *out == "" {
		missing = append(missing, "--out")
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "reducer: missing required flag(s): %s\n", strings.Join(missing, ", "))
		fs.Usage()
		return 1
	}

	cfg := reduce.Config{
		SrcPath:      *src,
		StoreMapPath: *storeMap,
		PrefixFlag:   *prefix,
		OutDir:       *out,
	}
	if err := reduce.Run(cfg, stderr); err != nil {
		fmt.Fprintf(stderr, "reducer: %v\n", err)
		return 1
	}
	return 0
}
