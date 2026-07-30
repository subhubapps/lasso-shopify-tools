package reduce

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Required Shopify Partner "Earnings" CSV column names, resolved
// case-insensitively by header name on the first record (order-independent).
// These are the exact columns 02_build_per_charge.rb reads. The Payout Currency
// column is OPTIONAL - when it is absent every row is treated as USD and a
// warning is recorded (per 03, which defaults blank currencies to USD).
//
// Charge Type and Category are intentionally NOT resolved: they are
// diagnostic-only in the source scripts and are never used for classification
// (which keys off the Charge ID GID). Extra columns in the export are ignored.
const (
	colShop              = "Shop"
	colChargeID          = "Charge ID"
	colChargeCreation    = "Charge Creation Time"
	colPartnerShare      = "Partner Share"
	colPartnerSale       = "Partner Sale"
	colPartnerSalePayout = "Partner Sale In Payout Currency"
	colCurrency          = "Payout Currency"
)

const defaultCurrency = "USD"

// columns holds the resolved header indices for one CSV.
type columns struct {
	shop              int
	chargeID          int
	chargeCreation    int
	partnerShare      int
	partnerSale       int
	partnerSalePayout int
	currency          int
	hasCurrency       bool
}

// aggKey identifies one aggregate bucket.
type aggKey struct {
	lead     string
	month    string // YYYY-MM
	currency string
}

// aggEntry accumulates one bucket's running state. amountTT is the exact sum of
// per-charge ten-thousandths (integer 1e-4 units), matching 03's BigDecimal sum
// of the per-charge round(4) amounts.
type aggEntry struct {
	amountTT   int64
	subCount   int
	usageCount int
	otherCount int
	maxSubTime time.Time
	hasSub     bool
	maxAllTime time.Time
	hasAny     bool
}

// LoadStoreMap reads and parses the saved account_shopify_store_map JSON. A
// read or parse failure is a fatal (malformed store map) error.
func LoadStoreMap(path string) (*StoreMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading store map: %w", err)
	}
	var sm StoreMap
	if err := json.Unmarshal(data, &sm); err != nil {
		return nil, fmt.Errorf("parsing store map %s: %w", path, err)
	}
	return &sm, nil
}

// resolvePrefix applies the prefix resolution order: --prefix flag, else the
// store map's importPrefix, else a fatal usage error.
func resolvePrefix(flagPrefix string, sm *StoreMap) (string, error) {
	if p := strings.TrimSpace(flagPrefix); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(sm.ImportPrefix); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no import prefix: pass --prefix or provide importPrefix in the store map")
}

// Run performs the full reduction end to end: load the store map, resolve the
// prefix, stream the CSV, and write the three output files. A guardrail warning
// is printed to stderr when the account is not safe for settled upload, but the
// reduction still completes and Run returns nil (exit 0). Run returns a non-nil
// error - and writes no output files - for usage, IO, malformed-map,
// missing-column, and parse errors.
func Run(cfg Config, stderr io.Writer) error {
	sm, err := LoadStoreMap(cfg.StoreMapPath)
	if err != nil {
		return err
	}
	prefix, err := resolvePrefix(cfg.PrefixFlag, sm)
	if err != nil {
		return err
	}

	f, err := os.Open(cfg.SrcPath)
	if err != nil {
		return fmt.Errorf("opening source CSV: %w", err)
	}
	defer f.Close()

	// reduce reads the header and resolves columns before any data row, so a
	// missing-column error surfaces before OutDir is created.
	res, err := reduce(f, sm, prefix)
	if err != nil {
		return err
	}

	if err := writeOutputs(cfg.OutDir, res); err != nil {
		return err
	}

	if !res.Preview.Guardrail.SettledUploadSafe {
		fmt.Fprintf(stderr, "\nWARNING: %s\n\n", res.Preview.Guardrail.Warning)
	}
	return nil
}

// reduce streams the CSV and produces the Result. It is pure with respect to
// output files (it only reads src) so it is directly testable, including the
// constant-memory proof (feed a synthetic io.Reader).
func reduce(src io.Reader, sm *StoreMap, prefix string) (*Result, error) {
	// Normalized-subdomain -> lead slug. First mapping wins on duplicates.
	leadBySubdomain := make(map[string]string, len(sm.Stores))
	for _, s := range sm.Stores {
		key := normalizeSubdomain(s.Subdomain)
		if key == "" {
			continue
		}
		if _, exists := leadBySubdomain[key]; !exists {
			leadBySubdomain[key] = s.LeadSlug
		}
	}

	cr := csv.NewReader(src)
	cr.ReuseRecord = true   // constant per-record allocation
	cr.FieldsPerRecord = -1 // tolerate ragged rows: a single short row is skipped, never fatal

	header, err := cr.Read()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("source CSV is empty (no header row)")
		}
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}
	cols, err := resolveColumns(header)
	if err != nil {
		return nil, err
	}

	agg := make(map[aggKey]*aggEntry)
	unmatched := make(map[string]*UnmatchedShop)

	var st stats
	st.hasCurrency = cols.hasCurrency

	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A genuine CSV parse error (bad quoting) desyncs record boundaries;
			// treat it as a structural/malformed-file error. Ragged rows never
			// reach here (FieldsPerRecord = -1).
			return nil, fmt.Errorf("parse error at CSV line %d: %w", st.rowsRead+2, err)
		}
		st.rowsRead++

		// 1. Join to a store FIRST (02's order: shop -> slug, before any
		//    data-quality check). A blank shop is silently skipped (02: `next if
		//    sub blank`). An unmatched shop is counted into the worklist and
		//    never contributes to the aggregate.
		sub := normalizeSubdomain(field(rec, cols.shop))
		if sub == "" {
			continue
		}
		lead, matched := leadBySubdomain[sub]
		if !matched {
			st.unmatchedRows++
			accumulateUnmatched(unmatched, sub, rec, cols)
			continue
		}

		// 2. Skip-and-count bad DATA rows for matched shops (02's order and
		//    counters). A single malformed data row is skipped, never fatal.
		if strings.TrimSpace(field(rec, cols.chargeID)) == "" {
			st.skippedNoID++ // empty Charge ID: refund/downgrade/credit
			continue
		}
		shareRaw := field(rec, cols.partnerShare)
		if strings.TrimSpace(shareRaw) == "" {
			st.skippedBlankAmount++ // blank Partner Share
			continue
		}
		t, err := parseChargeTime(field(rec, cols.chargeCreation))
		if err != nil {
			st.skippedNoRecordedAt++ // unparseable Charge Creation Time
			continue
		}

		// 3. Decimal-exact currency scaling (with the zero Partner Sale guard).
		//    A non-numeric money field can't yield an amount; skip it as a bad
		//    amount rather than aborting the run.
		share, err := parseDecimal(shareRaw)
		if err != nil {
			st.skippedBlankAmount++
			continue
		}
		sale, err := parseDecimal(field(rec, cols.partnerSale))
		if err != nil {
			st.skippedBlankAmount++
			continue
		}
		salePayout, err := parseDecimal(field(rec, cols.partnerSalePayout))
		if err != nil {
			st.skippedBlankAmount++
			continue
		}
		amountTT, scaled := scaleToTenThousandths(share, sale, salePayout)
		if !scaled {
			st.zeroSaleCount++
		}

		// 4. Currency (default USD; count blank values, per 03).
		currency := defaultCurrency
		if cols.hasCurrency {
			if c := strings.ToUpper(strings.TrimSpace(field(rec, cols.currency))); c != "" {
				currency = c
			} else {
				st.blankCurrencyRows++
			}
		}

		// 5. Classify by the Charge ID GID (03 lines 62-68) and accumulate.
		st.matchedRows++
		cat := classify(field(rec, cols.chargeID))
		switch cat {
		case catSubscription:
			st.subCount++
		case catUsage:
			st.usageCount++
		default:
			st.otherCount++
		}

		key := aggKey{lead: lead, month: monthKey(t), currency: currency}
		e := agg[key]
		if e == nil {
			e = &aggEntry{}
			agg[key] = e
		}
		e.amountTT += amountTT
		if !e.hasAny || t.After(e.maxAllTime) {
			e.maxAllTime = t
			e.hasAny = true
		}
		switch cat {
		case catSubscription:
			e.subCount++
			if !e.hasSub || t.After(e.maxSubTime) {
				e.maxSubTime = t
			}
			e.hasSub = true
		case catUsage:
			e.usageCount++
		default:
			e.otherCount++
		}
	}

	return buildResult(sm, prefix, agg, unmatched, st), nil
}

// field returns rec[idx] or "" when idx is out of range, so a ragged (too-short)
// row is handled gracefully instead of panicking - a missing required field
// then falls into the appropriate skip counter or the blank-shop skip.
func field(rec []string, idx int) string {
	if idx < 0 || idx >= len(rec) {
		return ""
	}
	return rec[idx]
}

// accumulateUnmatched records one unmatched-shop row into the store-linking
// worklist. Amount and month are best-effort (parse failures contribute 0 / no
// month) so a malformed unmatched row never aborts the run; these rows never
// touch the aggregate.
func accumulateUnmatched(unmatched map[string]*UnmatchedShop, sub string, rec []string, cols columns) {
	u := unmatched[sub]
	if u == nil {
		u = &UnmatchedShop{Subdomain: sub}
		unmatched[sub] = u
	}
	u.RowCount++

	share, e1 := parseDecimal(field(rec, cols.partnerShare))
	sale, e2 := parseDecimal(field(rec, cols.partnerSale))
	salePayout, e3 := parseDecimal(field(rec, cols.partnerSalePayout))
	if e1 == nil && e2 == nil && e3 == nil {
		tt, _ := scaleToTenThousandths(share, sale, salePayout)
		u.TotalTenThousandths += tt
	}

	if t, err := parseChargeTime(field(rec, cols.chargeCreation)); err == nil {
		m := monthKey(t)
		if u.FirstMonth == "" || m < u.FirstMonth {
			u.FirstMonth = m
		}
		if u.LastMonth == "" || m > u.LastMonth {
			u.LastMonth = m
		}
	}
}

// stats carries the streaming counters into result assembly.
type stats struct {
	rowsRead            int
	skippedNoID         int
	skippedBlankAmount  int
	skippedNoRecordedAt int
	matchedRows         int
	unmatchedRows       int
	zeroSaleCount       int
	blankCurrencyRows   int
	subCount            int
	usageCount          int
	otherCount          int
	hasCurrency         bool
}

// buildResult turns the accumulated maps into the deterministically ordered
// rows, worklist, and preview.
func buildResult(sm *StoreMap, prefix string, agg map[aggKey]*aggEntry, unmatched map[string]*UnmatchedShop, st stats) *Result {
	keys := make([]aggKey, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].lead != keys[j].lead {
			return keys[i].lead < keys[j].lead
		}
		if keys[i].month != keys[j].month {
			return keys[i].month < keys[j].month
		}
		return keys[i].currency < keys[j].currency
	})

	existing := make(map[string]struct{}, len(sm.ExistingExternalIDs))
	for _, id := range sm.ExistingExternalIDs {
		existing[id] = struct{}{}
	}

	rows := make([]NeutralRow, 0, len(keys))
	monthCents := map[string]int64{}
	monthRows := map[string]int{}
	monthOrder := []string{}
	currencyCents := map[string]int64{}
	currencyRows := map[string]int{}
	currencyOrder := []string{}
	var totalCents int64
	newCount := 0

	for _, k := range keys {
		e := agg[k]
		// Emit the neutral-row amount rounded to 2 decimals (half away from
		// zero) from the exact ten-thousandths bucket sum. The by_month / total
		// rollups sum these emitted cents so preview totals equal the sum of the
		// emitted row amounts exactly.
		cents := ttToCents(e.amountTT)
		revTime := e.maxAllTime
		if e.hasSub {
			revTime = e.maxSubTime // 03: anchor_date = last_sub_date || last_date
		}
		id := buildIdentifier(prefix, k.lead, k.month, k.currency)
		rows = append(rows, NeutralRow{
			LeadSlug:           k.lead,
			Amount:             moneyFromCents(cents),
			RevenueDate:        dateKey(revTime),
			Description:        buildDescription(e.subCount, e.usageCount, e.otherCount),
			ExternalIdentifier: id,
		})
		totalCents += cents
		if _, seen := monthCents[k.month]; !seen {
			monthOrder = append(monthOrder, k.month)
		}
		monthCents[k.month] += cents
		monthRows[k.month]++
		if _, seen := currencyCents[k.currency]; !seen {
			currencyOrder = append(currencyOrder, k.currency)
		}
		currencyCents[k.currency] += cents
		currencyRows[k.currency]++
		if _, found := existing[id]; !found {
			newCount++
		}
	}

	sort.Strings(monthOrder)
	byMonth := make([]MonthBucket, 0, len(monthOrder))
	for _, m := range monthOrder {
		byMonth = append(byMonth, MonthBucket{Month: m, Rows: monthRows[m], Amount: moneyFromCents(monthCents[m])})
	}
	sort.Strings(currencyOrder)
	byCurrency := make([]CurrencyBucket, 0, len(currencyOrder))
	for _, c := range currencyOrder {
		byCurrency = append(byCurrency, CurrencyBucket{Currency: c, Rows: currencyRows[c], Amount: moneyFromCents(currencyCents[c])})
	}

	worklist := make([]UnmatchedShop, 0, len(unmatched))
	for _, u := range unmatched {
		worklist = append(worklist, *u)
	}
	sort.Slice(worklist, func(i, j int) bool { return worklist[i].Subdomain < worklist[j].Subdomain })

	warnings := []string{}
	if st.zeroSaleCount > 0 {
		warnings = append(warnings, fmt.Sprintf("%d row(s) had a zero Partner Sale; used the unscaled Partner Share for those rows", st.zeroSaleCount))
	}
	if !st.hasCurrency {
		warnings = append(warnings, "no Payout Currency column found in the CSV header; assumed USD for all rows (verify against the real export)")
	} else if st.blankCurrencyRows > 0 {
		warnings = append(warnings, fmt.Sprintf("%d row(s) had a blank Payout Currency; defaulted them to USD", st.blankCurrencyRows))
	}

	var newEstimate *int
	if sm.ExistingExternalIDsTruncated {
		warnings = append(warnings, "existingExternalIds was truncated by the server; new_estimate is unavailable offline - rely on the server dry_run preview")
	} else {
		n := newCount
		newEstimate = &n
	}

	safe := sm.RevenueModeSignal.SettledUploadSafe != nil && *sm.RevenueModeSignal.SettledUploadSafe
	guardrail := Guardrail{
		SettledUploadSafe:      safe,
		ApiRevenueSyncDisabled: sm.RevenueModeSignal.ApiRevenueSyncDisabled,
	}
	if !safe {
		guardrail.Warning = "account is NOT safe for settled upload: Shopify API revenue sync is still active, so a commit will be rejected server-side. Reduction is read-only and completed; do not commit until the account is marked safe (see the README prerequisite)."
	}

	preview := &Preview{
		RowsRead:           st.rowsRead,
		SkippedNoChargeID:  st.skippedNoID,
		SkippedBlankAmount: st.skippedBlankAmount,
		SkippedNoRecordAt:  st.skippedNoRecordedAt,
		MatchedRows:        st.matchedRows,
		UnmatchedRows:      st.unmatchedRows,
		UnmatchedShops:     len(worklist),
		AggregateRows:      len(rows),
		SubscriptionCount:  st.subCount,
		UsageRecordCount:   st.usageCount,
		OtherCount:         st.otherCount,
		TotalAmount:        moneyFromCents(totalCents),
		ByMonth:            byMonth,
		ByCurrency:         byCurrency,
		NewEstimate:        newEstimate,
		Guardrail:          guardrail,
		Prefix:             prefix,
		Warnings:           warnings,
	}

	return &Result{Rows: rows, Unmatched: worklist, Preview: preview}
}

// resolveColumns maps the required logical columns to header indices (by exact
// name, case-insensitively, order-independent), failing with a message that
// names every missing column. Data rows are never read when a column is missing,
// so a bad header is caught before OutDir is created. The Payout Currency column
// is optional (absent => USD assumed for every row, with a warning).
func resolveColumns(header []string) (columns, error) {
	index := make(map[string]int, len(header))
	for i, h := range header {
		index[strings.ToLower(strings.TrimSpace(h))] = i
	}

	var c columns
	var missing []string
	req := func(name string, dst *int) {
		if idx, ok := index[strings.ToLower(name)]; ok {
			*dst = idx
			return
		}
		missing = append(missing, name)
	}

	req(colShop, &c.shop)
	req(colChargeID, &c.chargeID)
	req(colChargeCreation, &c.chargeCreation)
	req(colPartnerShare, &c.partnerShare)
	req(colPartnerSale, &c.partnerSale)
	req(colPartnerSalePayout, &c.partnerSalePayout)

	if idx, ok := index[strings.ToLower(colCurrency)]; ok {
		c.currency = idx
		c.hasCurrency = true
	}

	if len(missing) > 0 {
		return columns{}, fmt.Errorf("missing required column(s): %s", strings.Join(missing, ", "))
	}
	return c, nil
}
