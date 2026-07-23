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
// ASSUMED Shopify Partner "Earnings" CSV schema (resolved case-insensitively by
// header NAME on the first record; verify against a real export - see
// reducer/README.md). Required columns:
//
//	Charge ID                        - empty => refund/downgrade/credit, skipped
//	Charge Creation Time             - ISO-8601 / space-separated timestamp
//	Partner Share                    - partner earnings in the sale currency
//	Partner Sale                     - sale amount in the sale currency
//	Partner Sale In Payout Currency  - sale amount in the payout currency
//	shop domain    - first present of: Shop, Store, Myshopify Domain,
//	                 Shop Domain, Store Domain, Shop Name, Domain
//	charge type    - first present of: Type, Charge Type
//
// Optional column (absent => USD assumed for every row, with a warning):
//
//	payout currency - first present of: Payout Currency, Currency,
//	                  Charge Currency, Partner Sale Currency
//
// Charge-type classification (documented default): a type whose lowercased text
// contains "subscription" or "recurring" is a SUBSCRIPTION; every other type is
// a USAGE record. This is the single knob to adjust if a real export uses other
// spellings.
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
