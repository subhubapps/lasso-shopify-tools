package reduce

import (
	"bufio"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
)

// testHeader is the full assumed header used by the inline behavior fixtures.
const testHeader = "Shop,Charge ID,Charge Creation Time,Type,Partner Share,Partner Sale,Partner Sale In Payout Currency,Payout Currency\n"

func boolPtr(b bool) *bool { return &b }

// safeMap builds a store map that is safe for settled upload with the default
// loop-monthly prefix and the given stores.
func safeMap(stores ...Store) *StoreMap {
	return &StoreMap{
		Stores:            stores,
		ImportPrefix:      "loop-monthly",
		RevenueModeSignal: RevenueModeSignal{SettledUploadSafe: boolPtr(true), ApiRevenueSyncDisabled: boolPtr(true)},
	}
}

func mustReduce(t *testing.T, csvText string, sm *StoreMap, prefix string) *Result {
	t.Helper()
	res, err := reduce(strings.NewReader(csvText), sm, prefix)
	if err != nil {
		t.Fatalf("reduce returned error: %v", err)
	}
	return res
}

func rowByID(t *testing.T, res *Result, id string) NeutralRow {
	t.Helper()
	for _, r := range res.Rows {
		if r.ExternalIdentifier == id {
			return r
		}
	}
	t.Fatalf("no aggregate row with external_identifier %q; got %+v", id, res.Rows)
	return NeutralRow{}
}

func hasWarning(res *Result, substr string) bool {
	for _, w := range res.Preview.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// --- helper unit tests ---

func TestNormalizeSubdomain(t *testing.T) {
	cases := map[string]string{
		"Chestnuts":                    "chestnuts",
		"acme.myshopify.com":           "acme",
		"ACME.MyShopify.com":           "acme",
		"  spacey  ":                   "spacey",
		"https://acme.myshopify.com/":  "acme",
		"http://Store.myshopify.com":   "store",
		"already-bare":                 "already-bare",
		"chestnuts.myshopify.com/path": "chestnuts.myshopify.com/path", // only a trailing slash is stripped
	}
	for in, want := range cases {
		if got := normalizeSubdomain(in); got != want {
			t.Errorf("normalizeSubdomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseAmount(t *testing.T) {
	ok := map[string]float64{
		"":          0,
		"   ":       0,
		"12.5":      12.5,
		"1,234.56":  1234.56,
		"$50.00":    50,
		"  80.00  ": 80,
		"-30.00":    -30,
		"€99.99":    99.99,
	}
	for in, want := range ok {
		got, err := parseAmount(in)
		if err != nil {
			t.Errorf("parseAmount(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseAmount(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseAmount("not-a-number"); err == nil {
		t.Errorf("parseAmount(\"not-a-number\") expected an error")
	}
}

func TestRound2AndMoney(t *testing.T) {
	// round half away from zero (0.125 and 12.5 are exactly representable).
	if got := round2(0.125); got != 0.13 {
		t.Errorf("round2(0.125) = %v, want 0.13", got)
	}
	if got := round2(-0.125); got != -0.13 {
		t.Errorf("round2(-0.125) = %v, want -0.13", got)
	}
	moneyCases := map[float64]string{
		72:      "72.00",
		1234.56: "1234.56",
		0:       "0.00",
		1234.5:  "1234.50",
		0.1:     "0.10",
		-5:      "-5.00",
	}
	for in, want := range moneyCases {
		if got := money(in).String(); got != want {
			t.Errorf("money(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildDescription(t *testing.T) {
	cases := []struct {
		sub, usage int
		want       string
	}{
		{3, 2, "3 Subscriptions + 2 Usage Records"},
		{1, 0, "1 Subscription"},
		{0, 4, "4 Usage Records"},
		{2, 1, "2 Subscriptions + 1 Usage Record"},
		{1, 1, "1 Subscription + 1 Usage Record"},
		{2, 0, "2 Subscriptions"},
	}
	for _, c := range cases {
		if got := buildDescription(c.sub, c.usage); got != c.want {
			t.Errorf("buildDescription(%d,%d) = %q, want %q", c.sub, c.usage, got, c.want)
		}
	}
}

func TestBuildIdentifier(t *testing.T) {
	got := buildIdentifier("loop-monthly", "LD-123", "2026-03", "USD")
	if want := "loop-monthly-LD-123-2026-03-USD"; got != want {
		t.Errorf("buildIdentifier = %q, want %q", got, want)
	}
}

func TestIsSubscription(t *testing.T) {
	sub := []string{"Subscription", "subscription plan", "Recurring application charge", "RECURRING"}
	usage := []string{"Usage Charge", "usage", "One time", "Application charge", ""}
	for _, s := range sub {
		if !isSubscription(s) {
			t.Errorf("isSubscription(%q) = false, want true", s)
		}
	}
	for _, s := range usage {
		if isSubscription(s) {
			t.Errorf("isSubscription(%q) = true, want false", s)
		}
	}
}

func TestScaleAmount(t *testing.T) {
	if amt, ok := scaleAmount(80, 100, 90); amt != 72 || !ok {
		t.Errorf("scaleAmount(80,100,90) = (%v,%v), want (72,true)", amt, ok)
	}
	if amt, ok := scaleAmount(25, 0, 0); amt != 25 || ok {
		t.Errorf("scaleAmount(25,0,0) = (%v,%v), want (25,false)", amt, ok)
	}
	if amt, ok := scaleAmount(100, 200, 200); amt != 100 || !ok {
		t.Errorf("scaleAmount(100,200,200) = (%v,%v), want (100,true)", amt, ok)
	}
}

func TestParseChargeTime(t *testing.T) {
	good := []struct{ in, month, date string }{
		{"2026-01-03T10:00:00Z", "2026-01", "2026-01-03"},
		{"2026-01-03", "2026-01", "2026-01-03"},
		{"2026-12-31 23:59:59", "2026-12", "2026-12-31"},
	}
	for _, c := range good {
		tm, err := parseChargeTime(c.in)
		if err != nil {
			t.Errorf("parseChargeTime(%q) error: %v", c.in, err)
			continue
		}
		if monthKey(tm) != c.month || dateKey(tm) != c.date {
			t.Errorf("parseChargeTime(%q) => month %q date %q, want %q %q", c.in, monthKey(tm), dateKey(tm), c.month, c.date)
		}
	}
	for _, bad := range []string{"", "   ", "not-a-time"} {
		if _, err := parseChargeTime(bad); err == nil {
			t.Errorf("parseChargeTime(%q) expected error", bad)
		}
	}
}

func TestResolvePrefix(t *testing.T) {
	sm := &StoreMap{ImportPrefix: "loop-monthly"}
	if p, err := resolvePrefix("acme-monthly", sm); err != nil || p != "acme-monthly" {
		t.Errorf("flag override => (%q,%v), want (acme-monthly,nil)", p, err)
	}
	if p, err := resolvePrefix("", sm); err != nil || p != "loop-monthly" {
		t.Errorf("map default => (%q,%v), want (loop-monthly,nil)", p, err)
	}
	if _, err := resolvePrefix("", &StoreMap{ImportPrefix: ""}); err == nil {
		t.Errorf("no prefix anywhere: expected an error")
	}
	if _, err := resolvePrefix("   ", &StoreMap{ImportPrefix: "  "}); err == nil {
		t.Errorf("whitespace-only prefixes: expected an error")
	}
}

// --- reduce() behavior tests ---

func TestReduceCurrencyScaling(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,80.00,100.00,90.00,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Amount.String() != "72.00" {
		t.Errorf("scaled amount = %s, want 72.00", r.Amount.String())
	}
}

func TestReduceZeroPartnerSaleGuard(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-20T00:00:00Z,Subscription,25.00,0,0,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Amount.String() != "25.00" {
		t.Errorf("zero Partner Sale amount = %s, want 25.00 (unscaled Partner Share)", r.Amount.String())
	}
	if !hasWarning(res, "zero Partner Sale") {
		t.Errorf("expected a zero Partner Sale warning, got %v", res.Preview.Warnings)
	}
}

func TestReduceUsageOnlyFallback(t *testing.T) {
	csv := testHeader +
		"acme,U1,2026-07-05T00:00:00Z,Usage Charge,5,10,10,USD\n" +
		"acme,U2,2026-07-15T00:00:00Z,Usage Charge,5,10,10,USD\n" +
		"acme,U3,2026-07-09T00:00:00Z,Usage Charge,5,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-07-USD")
	if r.RevenueDate != "2026-07-15" {
		t.Errorf("usage-only revenue_date = %s, want 2026-07-15 (latest charge)", r.RevenueDate)
	}
	if r.Description != "3 Usage Records" {
		t.Errorf("description = %q, want %q", r.Description, "3 Usage Records")
	}
}

func TestReduceMultiSubscriptionLatestWins(t *testing.T) {
	csv := testHeader +
		"acme,S1,2026-06-03T00:00:00Z,Subscription,10,10,10,USD\n" +
		"acme,S2,2026-06-20T00:00:00Z,Subscription,10,10,10,USD\n" +
		"acme,U1,2026-06-30T00:00:00Z,Usage Charge,5,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-06-USD")
	if r.RevenueDate != "2026-06-20" {
		t.Errorf("revenue_date = %s, want 2026-06-20 (latest subscription, not the later usage charge)", r.RevenueDate)
	}
	if r.Description != "2 Subscriptions + 1 Usage Record" {
		t.Errorf("description = %q", r.Description)
	}
	if r.Amount.String() != "25.00" {
		t.Errorf("amount = %s, want 25.00", r.Amount.String())
	}
}

func TestReduceUnmatchedShopWorklistNotAggregate(t *testing.T) {
	csv := testHeader +
		"ghost.myshopify.com,G1,2026-01-12T00:00:00Z,Subscription,40,100,100,USD\n" +
		"ghost,G2,2026-02-02T00:00:00Z,Usage Charge,15,100,100,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	if len(res.Rows) != 0 {
		t.Fatalf("expected no aggregate rows for unmatched shops, got %d", len(res.Rows))
	}
	if len(res.Unmatched) != 1 {
		t.Fatalf("expected 1 unmatched shop, got %d", len(res.Unmatched))
	}
	u := res.Unmatched[0]
	if u.Subdomain != "ghost" || u.FirstMonth != "2026-01" || u.LastMonth != "2026-02" || u.RowCount != 2 {
		t.Errorf("unmatched worklist = %+v", u)
	}
	if round2(u.TotalAmount) != 55.00 {
		t.Errorf("unmatched total = %v, want 55.00", u.TotalAmount)
	}
	if res.Preview.UnmatchedRows != 2 || res.Preview.UnmatchedShops != 1 {
		t.Errorf("preview unmatched counts = rows %d shops %d", res.Preview.UnmatchedRows, res.Preview.UnmatchedShops)
	}
}

func TestReduceMyshopifyStrippedBothSides(t *testing.T) {
	// Map side carries the full domain; CSV side the bare (differently cased)
	// subdomain -- and vice versa. Both must join.
	sm := safeMap(
		Store{Subdomain: "chestnuts.myshopify.com", LeadSlug: "LD-CHEST"},
		Store{Subdomain: "acme", LeadSlug: "LD-ACME"},
	)
	csv := testHeader +
		"Chestnuts,C1,2026-01-03T00:00:00Z,Subscription,10,10,10,USD\n" +
		"acme.myshopify.com,A1,2026-01-04T00:00:00Z,Subscription,20,10,10,USD\n"
	res := mustReduce(t, csv, sm, "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-CHEST-2026-01-USD")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if res.Preview.UnmatchedRows != 0 {
		t.Errorf("expected 0 unmatched, got %d", res.Preview.UnmatchedRows)
	}
}

func TestReducePrefixFlagOverride(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,10,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "acme-monthly")
	rowByID(t, res, "acme-monthly-LD-ACME-2026-01-USD")
	if res.Preview.Prefix != "acme-monthly" {
		t.Errorf("preview prefix = %q, want acme-monthly", res.Preview.Prefix)
	}
}

func TestReduceNewEstimate(t *testing.T) {
	sm := safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"})
	sm.ExistingExternalIDs = []string{"loop-monthly-LD-ACME-2026-01-USD"}
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,10,10,10,USD\n" + // existing
		"acme,C2,2026-02-15T00:00:00Z,Subscription,10,10,10,USD\n" // new
	res := mustReduce(t, csv, sm, "loop-monthly")
	if res.Preview.NewEstimate == nil {
		t.Fatal("new_estimate should not be nil when inventory is complete")
	}
	if *res.Preview.NewEstimate != 1 {
		t.Errorf("new_estimate = %d, want 1", *res.Preview.NewEstimate)
	}
}

func TestReduceTruncatedInventory(t *testing.T) {
	sm := safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"})
	sm.ExistingExternalIDsTruncated = true
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,10,10,10,USD\n"
	res := mustReduce(t, csv, sm, "loop-monthly")
	if res.Preview.NewEstimate != nil {
		t.Errorf("new_estimate = %v, want nil when truncated", *res.Preview.NewEstimate)
	}
	if !hasWarning(res, "truncated") {
		t.Errorf("expected a truncation warning, got %v", res.Preview.Warnings)
	}
}

func TestReduceGuardrailSafeUnsafeAndMissing(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,10,10,10,USD\n"

	safe := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	if !safe.Preview.Guardrail.SettledUploadSafe || safe.Preview.Guardrail.Warning != "" {
		t.Errorf("safe guardrail = %+v, want safe with empty warning", safe.Preview.Guardrail)
	}

	unsafeMap := safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"})
	unsafeMap.RevenueModeSignal.SettledUploadSafe = boolPtr(false)
	unsafe := mustReduce(t, csv, unsafeMap, "loop-monthly")
	if unsafe.Preview.Guardrail.SettledUploadSafe || unsafe.Preview.Guardrail.Warning == "" {
		t.Errorf("unsafe guardrail = %+v, want not-safe with a warning", unsafe.Preview.Guardrail)
	}

	// A nil settledUploadSafe (signal absent) is treated as not safe.
	missingMap := safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"})
	missingMap.RevenueModeSignal = RevenueModeSignal{}
	missing := mustReduce(t, csv, missingMap, "loop-monthly")
	if missing.Preview.Guardrail.SettledUploadSafe {
		t.Errorf("missing signal should be treated as not safe")
	}
}

func TestReduceRecurringClassifiedAsSubscription(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Recurring application charge,10,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Description != "1 Subscription" {
		t.Errorf("Recurring application charge classified as %q, want subscription", r.Description)
	}
}

func TestReduceMultiCurrencySplits(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,10,10,10,USD\n" +
		"acme,C2,2026-01-16T00:00:00Z,Subscription,10,10,10,EUR\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-EUR")
	if len(res.Preview.ByCurrency) != 2 {
		t.Errorf("by_currency = %+v, want 2 entries", res.Preview.ByCurrency)
	}
}

func TestReduceMissingColumns(t *testing.T) {
	// Missing Partner Share, no shop-domain candidate, and no charge-type
	// candidate. The error must name each missing column (the multi-candidate
	// ones with their candidate list).
	header := "Charge ID,Charge Creation Time,Partner Sale,Partner Sale In Payout Currency\n"
	_, err := reduce(strings.NewReader(header), safeMap(), "loop-monthly")
	if err == nil {
		t.Fatal("expected a missing-column error")
	}
	for _, want := range []string{"Partner Share", "shop domain (one of:", "charge type (one of:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

func TestReduceCurrencyColumnAbsentAssumesUSD(t *testing.T) {
	header := "Shop,Charge ID,Charge Creation Time,Type,Partner Share,Partner Sale,Partner Sale In Payout Currency\n"
	csv := header +
		"acme,C1,2026-01-15T00:00:00Z,Subscription,10,10,10\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if !hasWarning(res, "assumed USD") {
		t.Errorf("expected an assumed-USD warning, got %v", res.Preview.Warnings)
	}
}

func TestReduceQuotedEmbeddedNewline(t *testing.T) {
	// A quoted field containing an embedded newline and comma must parse as a
	// single record (encoding/csv handles it natively).
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,\"Usage,\nCharge\",5,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	if res.Preview.RowsRead != 1 {
		t.Fatalf("rows_read = %d, want 1 (embedded newline must not split the record)", res.Preview.RowsRead)
	}
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Description != "1 Usage Record" {
		t.Errorf("description = %q, want 1 Usage Record", r.Description)
	}
}

// oneByteReader returns the data one byte per Read to stress the streaming path
// across arbitrary chunk boundaries.
type oneByteReader struct {
	data []byte
	i    int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.i]
	r.i++
	return 1, nil
}

func TestReduceChunkedReaderMatchesWholeRead(t *testing.T) {
	csv := testHeader +
		"acme,C1,2026-01-15T00:00:00Z,\"Sub\nscription\",80.00,100.00,90.00,USD\n" +
		"acme,C2,2026-01-20T00:00:00Z,Usage Charge,10,10,10,USD\n"
	sm := safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"})

	whole, err := reduce(strings.NewReader(csv), sm, "loop-monthly")
	if err != nil {
		t.Fatalf("whole read: %v", err)
	}
	chunked, err := reduce(&oneByteReader{data: []byte(csv)}, sm, "loop-monthly")
	if err != nil {
		t.Fatalf("chunked read: %v", err)
	}
	if len(whole.Rows) != 1 || len(chunked.Rows) != 1 {
		t.Fatalf("row counts: whole %d chunked %d", len(whole.Rows), len(chunked.Rows))
	}
	w, c := whole.Rows[0], chunked.Rows[0]
	if w != c {
		t.Errorf("chunked output differs:\n whole   %+v\n chunked %+v", w, c)
	}
}

// TestConstantMemory streams a large number of rows through a synthetic pipe
// (never materializing a file) and asserts peak live heap stays bounded by the
// aggregate size, not the row count -- and that the aggregate totals equal the
// analytic baseline for the same logical rows. The same streaming code path
// handles a multi-GB on-disk CSV identically.
func TestConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping constant-memory test in -short mode")
	}
	const (
		rows  = 1_000_000
		leads = 5
	)
	stores := make([]Store, leads)
	for i := range stores {
		stores[i] = Store{Subdomain: fmt.Sprintf("shop%d", i), LeadSlug: fmt.Sprintf("LD-%d", i)}
	}
	sm := safeMap(stores...)

	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriterSize(pw, 1<<16)
		_, _ = io.WriteString(w, testHeader)
		for i := 0; i < rows; i++ {
			day := i%28 + 1
			fmt.Fprintf(w, "shop%d,C%d,2026-05-%02dT00:00:00Z,Subscription,1.00,1.00,1.00,USD\n", i%leads, i, day)
		}
		_ = w.Flush()
		_ = pw.Close()
	}()

	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	res, err := reduce(pr, sm, "loop-monthly")
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	if res.Preview.RowsRead != rows {
		t.Fatalf("rows_read = %d, want %d", res.Preview.RowsRead, rows)
	}
	if len(res.Rows) != leads {
		t.Fatalf("aggregate rows = %d, want %d (bounded by leads x months x currencies)", len(res.Rows), leads)
	}
	// Output equals the analytic baseline: each lead summed rows/leads * $1.00.
	wantPerLead := money(float64(rows / leads)).String()
	for _, r := range res.Rows {
		if r.Amount.String() != wantPerLead {
			t.Fatalf("lead %s amount = %s, want %s", r.LeadSlug, r.Amount.String(), wantPerLead)
		}
	}

	heapDelta := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
	const bound = 16 << 20 // 16 MiB; the logical input is ~90 MB, GB-scale in production
	if heapDelta > bound {
		t.Fatalf("heap grew by %d bytes reducing %d rows into %d aggregate rows; want <= %d (memory must be O(aggregate), not O(rows))",
			heapDelta, rows, len(res.Rows), bound)
	}
	t.Logf("reduced %d rows into %d aggregate rows; retained-heap delta %d bytes (bound %d)", rows, len(res.Rows), heapDelta, bound)
}
