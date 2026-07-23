package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Errorf("--version printed %q, want %q", got, version)
	}
}

func TestRunMissingFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--src", "x.csv"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"--store-map", "--out"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q should name missing flag %q", stderr.String(), want)
		}
	}
}

func TestRunEndToEnd(t *testing.T) {
	outDir := t.TempDir()
	args := []string{
		"--src", filepath.Join("testdata", "comprehensive", "earnings.csv"),
		"--store-map", filepath.Join("testdata", "comprehensive", "map.json"),
		"--out", outDir,
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	for _, fn := range []string{"aggregate.jsonl", "unmatched_shops.csv", "preview.json"} {
		if _, err := os.Stat(filepath.Join(outDir, fn)); err != nil {
			t.Errorf("expected output file %s: %v", fn, err)
		}
	}
}

func TestRunFatalMissingStoreMap(t *testing.T) {
	args := []string{
		"--src", filepath.Join("testdata", "comprehensive", "earnings.csv"),
		"--store-map", filepath.Join("testdata", "does-not-exist.json"),
		"--out", t.TempDir(),
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "reducer:") {
		t.Errorf("expected a reducer: error on stderr, got %q", stderr.String())
	}
}
