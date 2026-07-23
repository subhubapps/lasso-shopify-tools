package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lasso-shopify-tools/reducer/reduce"
)

// update regenerates the golden files instead of comparing against them:
//
//	go test -run Golden -update
var update = flag.Bool("update", false, "regenerate golden files")

var goldenFiles = []string{"aggregate.jsonl", "unmatched_shops.csv", "preview.json"}

// runFixture reduces testdata/<fixture> into a temp dir and byte-for-byte
// compares (or, with -update, rewrites) the three output files against
// testdata/<fixture>/golden. It returns whatever the reducer wrote to stderr.
func runFixture(t *testing.T, fixture string) string {
	t.Helper()
	dir := filepath.Join("testdata", fixture)
	outDir := t.TempDir()
	var stderr bytes.Buffer
	cfg := reduce.Config{
		SrcPath:      filepath.Join(dir, "earnings.csv"),
		StoreMapPath: filepath.Join(dir, "map.json"),
		OutDir:       outDir,
	}
	if err := reduce.Run(cfg, &stderr); err != nil {
		t.Fatalf("Run(%s): %v", fixture, err)
	}

	// Cross-file consistency is checked against the ACTUAL output (not the
	// frozen goldens), so a bad -update can never silently encode an
	// inconsistency.
	checkConsistency(t, outDir)

	for _, fn := range goldenFiles {
		got, err := os.ReadFile(filepath.Join(outDir, fn))
		if err != nil {
			t.Fatalf("reading output %s: %v", fn, err)
		}
		goldenPath := filepath.Join(dir, "golden", fn)
		if *update {
			if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
				t.Fatalf("updating golden %s: %v", goldenPath, err)
			}
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("reading golden %s: %v", goldenPath, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s/%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", fixture, fn, got, want)
		}
	}
	return stderr.String()
}

// checkConsistency parses the three output files and asserts the preview's
// counts and total agree with aggregate.jsonl and unmatched_shops.csv.
func checkConsistency(t *testing.T, outDir string) {
	t.Helper()

	pb, err := os.ReadFile(filepath.Join(outDir, "preview.json"))
	if err != nil {
		t.Fatalf("reading preview.json: %v", err)
	}
	var p reduce.Preview
	if err := json.Unmarshal(pb, &p); err != nil {
		t.Fatalf("parsing preview.json: %v", err)
	}

	ab, err := os.ReadFile(filepath.Join(outDir, "aggregate.jsonl"))
	if err != nil {
		t.Fatalf("reading aggregate.jsonl: %v", err)
	}
	lines, sum := 0, 0.0
	for _, ln := range strings.Split(strings.TrimRight(string(ab), "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lines++
		var r reduce.NeutralRow
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("parsing aggregate row %q: %v", ln, err)
		}
		f, err := r.Amount.Float64()
		if err != nil {
			t.Fatalf("amount %q not numeric: %v", r.Amount, err)
		}
		sum += f
	}
	if p.AggregateRows != lines {
		t.Errorf("aggregate_rows %d != aggregate.jsonl line count %d", p.AggregateRows, lines)
	}

	ub, err := os.ReadFile(filepath.Join(outDir, "unmatched_shops.csv"))
	if err != nil {
		t.Fatalf("reading unmatched_shops.csv: %v", err)
	}
	urows := 0
	for i, ln := range strings.Split(strings.TrimRight(string(ub), "\n"), "\n") {
		if i == 0 || strings.TrimSpace(ln) == "" { // row 0 is the header
			continue
		}
		urows++
	}
	if p.UnmatchedShops != urows {
		t.Errorf("unmatched_shops %d != unmatched_shops.csv data rows %d", p.UnmatchedShops, urows)
	}

	gotTotal := strconv.FormatFloat(math.Round(sum*100)/100, 'f', 2, 64)
	if p.TotalAmount.String() != gotTotal {
		t.Errorf("total_amount %s != sum of emitted amounts %s", p.TotalAmount.String(), gotTotal)
	}
}

func TestGoldenComprehensive(t *testing.T) {
	stderr := runFixture(t, "comprehensive")
	if stderr != "" {
		t.Errorf("safe fixture should emit nothing on stderr, got:\n%s", stderr)
	}
}

func TestGoldenUnsafeTruncated(t *testing.T) {
	stderr := runFixture(t, "unsafe_truncated")
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, "NOT safe") {
		t.Errorf("unsafe fixture should emit a prominent stderr WARNING, got:\n%s", stderr)
	}
}

// TestNoPrefixFatalWritesNoFiles verifies the prefix-missing usage error exits
// non-zero and writes no output (the output dir is never created).
func TestNoPrefixFatalWritesNoFiles(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "should-not-exist")
	cfg := reduce.Config{
		SrcPath:      filepath.Join("testdata", "comprehensive", "earnings.csv"),
		StoreMapPath: filepath.Join("testdata", "no_prefix", "map.json"),
		OutDir:       outDir,
	}
	var stderr bytes.Buffer
	err := reduce.Run(cfg, &stderr)
	if err == nil {
		t.Fatal("expected a fatal prefix error")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Errorf("error %q should name the missing prefix", err.Error())
	}
	if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
		t.Errorf("output dir should not be created on a prefix error (stat err = %v)", statErr)
	}
}
