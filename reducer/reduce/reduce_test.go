package reduce

import (
	"bufio"
	"fmt"
	"io"
	"math/big"
	"runtime"
	"strings"
	"testing"
)

// testHeader is the full real Shopify Partner "Earnings" header used by the
// inline behavior fixtures. It deliberately includes the diagnostic Charge Type
// column (position 3) so the tests prove classification IGNORES it and keys off
// the Charge ID GID (position 1) instead.
const testHeader = "Shop,Charge ID,Charge Creation Time,Charge Type,Partner Share,Partner Sale,Partner Sale In Payout Currency,Payout Currency\n"

func boolPtr(b bool) *bool { return &b }

// rat parses a decimal string into a big.Rat for the helper unit tests.
func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rat: " + s)
	}
	return r
}

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

// TestClassify pins the GID-substring classification to 03_aggregate_monthly.rb:
// AppSubscription -> subscription, AppUsageRecord -> usage, else -> other.
func TestClassify(t *testing.T) {
	cases := map[string]category{
		"gid://shopify/AppSubscription/123":     catSubscription,
		"gid://shopify/AppUsageRecord/456":      catUsage,
		"gid://shopify/AppOneTimeSale/789":      catOther,
		"gid://shopify/AppSubscriptionCredit/1": catSubscription, // substring match, like Ruby include?
		"gid://shopify/AppUsageRecordAdjust/2":  catUsage,
		"":                                      catOther,
		"adjustment-no-gid":                     catOther,
	}
	for in, want := range cases {
		if got := classify(in); got != want {
			t.Errorf("classify(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseDecimal(t *testing.T) {
	ok := map[string]string{
		"":          "0",
		"   ":       "0",
		"12.5":      "12.5",
		"1,234.56":  "1234.56",
		"$50.00":    "50",
		"  80.00  ": "80",
		"-30.00":    "-30",
		"€99.99":    "99.99",
	}
	for in, want := range ok {
		got, err := parseDecimal(in)
		if err != nil {
			t.Errorf("parseDecimal(%q) unexpected error: %v", in, err)
			continue
		}
		if got.Cmp(rat(want)) != 0 {
			t.Errorf("parseDecimal(%q) = %s, want %s", in, got.RatString(), want)
		}
	}
	if _, err := parseDecimal("not-a-number"); err == nil {
		t.Errorf("parseDecimal(\"not-a-number\") expected an error")
	}
}

// TestScaleToTenThousandths pins 02's per-charge scaling + round(4), in exact
// rational arithmetic, returning integer ten-thousandths.
func TestScaleToTenThousandths(t *testing.T) {
	// 80 * 90/100 = 72.0000
	if tt, ok := scaleToTenThousandths(rat("80"), rat("100"), rat("90")); tt != 720000 || !ok {
		t.Errorf("scale(80,100,90) = (%d,%v), want (720000,true)", tt, ok)
	}
	// zero Partner Sale guard -> unscaled Partner Share, ok=false
	if tt, ok := scaleToTenThousandths(rat("25"), rat("0"), rat("0")); tt != 250000 || ok {
		t.Errorf("scale(25,0,0) = (%d,%v), want (250000,false)", tt, ok)
	}
	// 100 * 1/3 = 33.33333... -> round(4) -> 33.3333 -> 333333 ten-thousandths
	if tt, ok := scaleToTenThousandths(rat("100"), rat("3"), rat("1")); tt != 333333 || !ok {
		t.Errorf("scale(100,3,1) = (%d,%v), want (333333,true)", tt, ok)
	}
	// 12.345 * 1/1 -> 12.3450 -> 123450 (exact; no float drift)
	if tt, ok := scaleToTenThousandths(rat("12.345"), rat("1"), rat("1")); tt != 123450 || !ok {
		t.Errorf("scale(12.345,1,1) = (%d,%v), want (123450,true)", tt, ok)
	}
}

func TestRatRoundTenThousandths(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"12.345", 123450},
		{"33.33333333", 333333},
		{"0.12345", 1235}, // half away from zero rounds up
		{"-0.12345", -1235},
		{"0.00005", 1},
		{"-0.00005", -1},
		{"0", 0},
	}
	for _, c := range cases {
		if got := ratRoundTenThousandths(rat(c.in)); got != c.want {
			t.Errorf("ratRoundTenThousandths(%s) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTTToCentsAndMoney(t *testing.T) {
	cents := map[int64]int64{
		123450:  1235,  // 12.3450 -> 12.35 (half away up)
		124900:  1249,  // 12.49 exact
		999999:  10000, // 99.9999 -> 100.00
		-123450: -1235,
		50:      1, // 0.0050 -> 0.01
		49:      0, // 0.0049 -> 0.00
		-50:     -1,
	}
	for in, want := range cents {
		if got := ttToCents(in); got != want {
			t.Errorf("ttToCents(%d) = %d, want %d", in, got, want)
		}
	}
	money := map[int64]string{
		7200:   "72.00",
		1235:   "12.35",
		0:      "0.00",
		123456: "1234.56",
		5:      "0.05",
		-500:   "-5.00",
		10000:  "100.00",
	}
	for in, want := range money {
		if got := moneyFromCents(in).String(); got != want {
			t.Errorf("moneyFromCents(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildDescription(t *testing.T) {
	cases := []struct {
		sub, usage, other int
		want              string
	}{
		{3, 2, 0, "3 Subscriptions + 2 Usage Records"},
		{1, 0, 0, "1 Subscription"},
		{0, 4, 0, "4 Usage Records"},
		{2, 1, 0, "2 Subscriptions + 1 Usage Record"},
		{1, 1, 1, "1 Subscription + 1 Usage Record + 1 Other"},
		{0, 0, 3, "3 Other"}, // "Other" is never pluralized
		{0, 0, 1, "1 Other"},
		{2, 0, 2, "2 Subscriptions + 2 Other"},
	}
	for _, c := range cases {
		if got := buildDescription(c.sub, c.usage, c.other); got != c.want {
			t.Errorf("buildDescription(%d,%d,%d) = %q, want %q", c.sub, c.usage, c.other, got, c.want)
		}
	}
}

func TestBuildIdentifier(t *testing.T) {
	got := buildIdentifier("loop-monthly", "LD-123", "2026-03", "USD")
	if want := "loop-monthly-LD-123-2026-03-USD"; got != want {
		t.Errorf("buildIdentifier = %q, want %q", got, want)
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
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,80.00,100.00,90.00,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Amount.String() != "72.00" {
		t.Errorf("scaled amount = %s, want 72.00", r.Amount.String())
	}
}

func TestReduceZeroPartnerSaleGuard(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-01-20T00:00:00Z,Recurring,25.00,0,0,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Amount.String() != "25.00" {
		t.Errorf("zero Partner Sale amount = %s, want 25.00 (unscaled Partner Share)", r.Amount.String())
	}
	if !hasWarning(res, "zero Partner Sale") {
		t.Errorf("expected a zero Partner Sale warning, got %v", res.Preview.Warnings)
	}
}

// TestReduceDecimalExact proves round(4)-then-sum decimal exactness: a float
// sum-then-round path would drift on repeating decimals and the 12.345 cent tie.
func TestReduceDecimalExact(t *testing.T) {
	// 3 x (100 * 1/3) = 3 x 33.3333 = 99.9999 -> 100.00
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-01-05T00:00:00Z,Recurring,100,3,1,USD\n" +
		"acme,gid://shopify/AppSubscription/2,2026-01-06T00:00:00Z,Recurring,100,3,1,USD\n" +
		"acme,gid://shopify/AppSubscription/3,2026-01-07T00:00:00Z,Recurring,100,3,1,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	if got := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD").Amount.String(); got != "100.00" {
		t.Errorf("repeating-decimal sum = %s, want 100.00", got)
	}

	// 12.345 -> 2dp half away from zero -> 12.35 (a float64 math.Round path gives 12.34).
	csv2 := testHeader +
		"acme,gid://shopify/AppSubscription/9,2026-02-05T00:00:00Z,Recurring,12.345,1,1,USD\n"
	res2 := mustReduce(t, csv2, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	if got := rowByID(t, res2, "loop-monthly-LD-ACME-2026-02-USD").Amount.String(); got != "12.35" {
		t.Errorf("cent tie = %s, want 12.35 (half away from zero, exact decimal)", got)
	}
}

func TestReduceUsageOnlyFallback(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppUsageRecord/1,2026-07-05T00:00:00Z,Usage,5,10,10,USD\n" +
		"acme,gid://shopify/AppUsageRecord/2,2026-07-15T00:00:00Z,Usage,5,10,10,USD\n" +
		"acme,gid://shopify/AppUsageRecord/3,2026-07-09T00:00:00Z,Usage,5,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-07-USD")
	if r.RevenueDate != "2026-07-15" {
		t.Errorf("usage-only revenue_date = %s, want 2026-07-15 (latest of all charges)", r.RevenueDate)
	}
	if r.Description != "3 Usage Records" {
		t.Errorf("description = %q, want %q", r.Description, "3 Usage Records")
	}
}

func TestReduceMultiSubscriptionLatestWins(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-06-03T00:00:00Z,Recurring,10,10,10,USD\n" +
		"acme,gid://shopify/AppSubscription/2,2026-06-20T00:00:00Z,Recurring,10,10,10,USD\n" +
		"acme,gid://shopify/AppUsageRecord/3,2026-06-30T00:00:00Z,Usage,5,10,10,USD\n"
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

// TestReduceOtherCategory covers the third category and proves the
// subscription-anchored revenue_date wins even when a later "other" charge
// exists in the same month.
func TestReduceOtherCategory(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-08-05T00:00:00Z,Recurring,10,10,10,USD\n" +
		"acme,gid://shopify/AppUsageRecord/2,2026-08-10T00:00:00Z,Usage,5,10,10,USD\n" +
		"acme,gid://shopify/AppOneTimeSale/3,2026-08-20T00:00:00Z,OneTime,7,10,10,USD\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-08-USD")
	if r.Description != "1 Subscription + 1 Usage Record + 1 Other" {
		t.Errorf("description = %q, want %q", r.Description, "1 Subscription + 1 Usage Record + 1 Other")
	}
	if r.RevenueDate != "2026-08-05" {
		t.Errorf("revenue_date = %s, want 2026-08-05 (subscription anchor, not the later Other charge)", r.RevenueDate)
	}
	if r.Amount.String() != "22.00" {
		t.Errorf("amount = %s, want 22.00", r.Amount.String())
	}
	if res.Preview.SubscriptionCount != 1 || res.Preview.UsageRecordCount != 1 || res.Preview.OtherCount != 1 {
		t.Errorf("category counts = sub %d usage %d other %d, want 1/1/1",
			res.Preview.SubscriptionCount, res.Preview.UsageRecordCount, res.Preview.OtherCount)
	}
}

// TestReduceClassificationUsesGIDNotChargeType proves the diagnostic Charge Type
// column is ignored: every row here has a misleading Charge Type, yet the GID
// drives classification, description, and the subscription-anchored date.
func TestReduceClassificationUsesGIDNotChargeType(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppUsageRecord/1,2026-01-10T00:00:00Z,Subscription,10,10,10,USD\n" + // type lies "Subscription"
		"acme,gid://shopify/AppSubscription/2,2026-01-15T00:00:00Z,Usage Charge,10,10,10,USD\n" + // type lies "Usage"
		"acme,gid://shopify/AppOneTimeSale/3,2026-01-20T00:00:00Z,Subscription,10,10,10,USD\n" // type lies "Subscription"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	r := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if r.Description != "1 Subscription + 1 Usage Record + 1 Other" {
		t.Errorf("description = %q, want %q (GID classification, not Charge Type)", r.Description, "1 Subscription + 1 Usage Record + 1 Other")
	}
	if r.RevenueDate != "2026-01-15" {
		t.Errorf("revenue_date = %s, want 2026-01-15 (the only AppSubscription GID)", r.RevenueDate)
	}
}

// TestReduceSkipAndCountBadRows proves a single malformed data row is
// skip-and-counted (never fatal): the run succeeds (nil error), the good row is
// the only aggregate row, and each skip reason is counted.
func TestReduceSkipAndCountBadRows(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,USD\n" + // good
		"acme,,2026-01-16T00:00:00Z,Recurring,10,10,10,USD\n" + // empty Charge ID
		"acme,gid://shopify/AppSubscription/3,2026-01-17T00:00:00Z,Recurring,,10,10,USD\n" + // blank Partner Share
		"acme,gid://shopify/AppSubscription/4,2026-01-18T00:00:00Z,Recurring,not-money,10,10,USD\n" + // non-numeric Partner Share
		"acme,gid://shopify/AppUsageRecord/5,not-a-date,Usage,10,10,10,USD\n" // unparseable Charge Creation Time
	res, err := reduce(strings.NewReader(csv), safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	if err != nil {
		t.Fatalf("a malformed data row must not abort the run; got error: %v", err)
	}
	p := res.Preview
	if p.RowsRead != 5 {
		t.Errorf("rows_read = %d, want 5", p.RowsRead)
	}
	if p.SkippedNoChargeID != 1 {
		t.Errorf("skipped_no_charge_id = %d, want 1", p.SkippedNoChargeID)
	}
	if p.SkippedBlankAmount != 2 {
		t.Errorf("skipped_blank_amount = %d, want 2 (blank + non-numeric Partner Share)", p.SkippedBlankAmount)
	}
	if p.SkippedNoRecordAt != 1 {
		t.Errorf("skipped_no_recorded_at = %d, want 1", p.SkippedNoRecordAt)
	}
	if p.MatchedRows != 1 || len(res.Rows) != 1 {
		t.Errorf("matched_rows = %d, aggregate rows = %d, want 1 and 1", p.MatchedRows, len(res.Rows))
	}
	if got := rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD").Amount.String(); got != "10.00" {
		t.Errorf("surviving amount = %s, want 10.00", got)
	}
}

func TestReduceUnmatchedShopWorklistNotAggregate(t *testing.T) {
	csv := testHeader +
		"ghost.myshopify.com,gid://shopify/AppSubscription/1,2026-01-12T00:00:00Z,Recurring,40,100,100,USD\n" +
		"ghost,gid://shopify/AppUsageRecord/2,2026-02-02T00:00:00Z,Usage,15,100,100,USD\n"
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
	if u.TotalTenThousandths != 550000 {
		t.Errorf("unmatched total = %d ten-thousandths, want 550000 (55.0000)", u.TotalTenThousandths)
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
		"Chestnuts,gid://shopify/AppSubscription/1,2026-01-03T00:00:00Z,Recurring,10,10,10,USD\n" +
		"acme.myshopify.com,gid://shopify/AppSubscription/2,2026-01-04T00:00:00Z,Recurring,20,10,10,USD\n"
	res := mustReduce(t, csv, sm, "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-CHEST-2026-01-USD")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if res.Preview.UnmatchedRows != 0 {
		t.Errorf("expected 0 unmatched, got %d", res.Preview.UnmatchedRows)
	}
}

func TestReducePrefixFlagOverride(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,USD\n"
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
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,USD\n" + // existing
		"acme,gid://shopify/AppSubscription/2,2026-02-15T00:00:00Z,Recurring,10,10,10,USD\n" // new
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
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,USD\n"
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
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,USD\n"

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

func TestReduceMultiCurrencySplits(t *testing.T) {
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,USD\n" +
		"acme,gid://shopify/AppSubscription/2,2026-01-16T00:00:00Z,Recurring,10,10,10,EUR\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-EUR")
	if len(res.Preview.ByCurrency) != 2 {
		t.Errorf("by_currency = %+v, want 2 entries", res.Preview.ByCurrency)
	}
}

func TestReduceMissingColumns(t *testing.T) {
	// Missing Shop, Charge Creation Time, and Partner Share. The error must name
	// each missing required column. Charge Type is NOT required (diagnostic-only).
	header := "Charge ID,Partner Sale,Partner Sale In Payout Currency,Payout Currency\n"
	_, err := reduce(strings.NewReader(header), safeMap(), "loop-monthly")
	if err == nil {
		t.Fatal("expected a missing-column error")
	}
	for _, want := range []string{"Shop", "Charge Creation Time", "Partner Share"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "Charge Type") {
		t.Errorf("error %q must NOT require Charge Type (it is diagnostic-only)", err.Error())
	}
}

func TestReduceCurrencyColumnAbsentAssumesUSD(t *testing.T) {
	// No Payout Currency and no Charge Type column (Charge Type is not required).
	header := "Shop,Charge ID,Charge Creation Time,Partner Share,Partner Sale,Partner Sale In Payout Currency\n"
	csv := header +
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,10,10,10\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if !hasWarning(res, "assumed USD") {
		t.Errorf("expected an assumed-USD warning, got %v", res.Preview.Warnings)
	}
}

func TestReduceBlankCurrencyDefaultsUSD(t *testing.T) {
	// Payout Currency column present but a value is blank -> USD + a warning.
	csv := testHeader +
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,Recurring,10,10,10,\n"
	res := mustReduce(t, csv, safeMap(Store{Subdomain: "acme", LeadSlug: "LD-ACME"}), "loop-monthly")
	rowByID(t, res, "loop-monthly-LD-ACME-2026-01-USD")
	if !hasWarning(res, "blank Payout Currency") {
		t.Errorf("expected a blank-currency warning, got %v", res.Preview.Warnings)
	}
}

func TestReduceQuotedEmbeddedNewline(t *testing.T) {
	// A quoted Charge Type field containing an embedded newline and comma must
	// parse as a single record (encoding/csv handles it natively); classification
	// still comes from the GID.
	csv := testHeader +
		"acme,gid://shopify/AppUsageRecord/1,2026-01-15T00:00:00Z,\"Usage,\nCharge\",5,10,10,USD\n"
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
		"acme,gid://shopify/AppSubscription/1,2026-01-15T00:00:00Z,\"Sub\nscription\",80.00,100.00,90.00,USD\n" +
		"acme,gid://shopify/AppUsageRecord/2,2026-01-20T00:00:00Z,Usage Charge,10,10,10,USD\n"
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
			fmt.Fprintf(w, "shop%d,gid://shopify/AppSubscription/%d,2026-05-%02dT00:00:00Z,Recurring,1.00,1.00,1.00,USD\n", i%leads, i, day)
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
	wantPerLead := moneyFromCents(int64(rows/leads) * 100).String()
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
