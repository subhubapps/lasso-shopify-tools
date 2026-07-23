package reduce

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Output file names written under the --out directory.
const (
	AggregateFile = "aggregate.jsonl"
	UnmatchedFile = "unmatched_shops.csv"
	PreviewFile   = "preview.json"
)

// writeOutputs creates the output directory and writes the three artifacts.
func writeOutputs(outDir string, res *Result) error {
	if outDir == "" {
		return fmt.Errorf("no output directory specified")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	if err := writeAggregate(filepath.Join(outDir, AggregateFile), res.Rows); err != nil {
		return err
	}
	if err := writeUnmatched(filepath.Join(outDir, UnmatchedFile), res.Unmatched); err != nil {
		return err
	}
	if err := writePreview(filepath.Join(outDir, PreviewFile), res.Preview); err != nil {
		return err
	}
	return nil
}

// writeAggregate writes one compact JSON neutral row per line (JSONL) in the
// already-deterministic order.
func writeAggregate(path string, rows []NeutralRow) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", AggregateFile, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil { // Encode appends a newline
			return fmt.Errorf("encoding aggregate row: %w", err)
		}
	}
	return w.Flush()
}

// writeUnmatched writes the store-linking worklist. The header row is always
// present, even when every shop matched.
func writeUnmatched(path string, shops []UnmatchedShop) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", UnmatchedFile, err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{"subdomain", "first_month", "last_month", "row_count", "total_amount"}); err != nil {
		return fmt.Errorf("writing %s header: %w", UnmatchedFile, err)
	}
	for _, s := range shops {
		rec := []string{
			s.Subdomain,
			s.FirstMonth,
			s.LastMonth,
			strconv.Itoa(s.RowCount),
			strconv.FormatFloat(round2(s.TotalAmount), 'f', 2, 64),
		}
		if err := w.Write(rec); err != nil {
			return fmt.Errorf("writing %s row: %w", UnmatchedFile, err)
		}
	}
	w.Flush()
	return w.Error()
}

// writePreview writes preview.json, pretty-printed for human/agent review.
func writePreview(path string, p *Preview) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return fmt.Errorf("encoding preview: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", PreviewFile, err)
	}
	return nil
}
