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

// Column candidate names, resolved case-insensitively by header name on the
// first record. The fixed-name columns each have one canonical spelling; the
// shop and charge-type columns vary between exports, so several candidates are
// tried and the first present wins. The currency column is OPTIONAL - when it
// is absent every row is treated as USD and a warning is recorded.
var (
	colChargeID          = []string{"Charge ID"}
	colChargeCreation    = []string{"Charge Creation Time"}
	colPartnerShare      = []string{"Partner Share"}
	colPartnerSale       = []string{"Partner Sale"}
	colPartnerSalePayout = []string{"Partner Sale In Payout Currency"}
	colShop              = []string{"Shop", "Store", "Myshopify Domain", "Shop Domain", "Store Domain", "Shop Name", "Domain"}
	colChargeType        = []string{"Type", "Charge Type"}
	colCurrency          = []string{"Payout Currency", "Currency", "Charge Currency", "Partner Sale Currency"}
)

const defaultCurrency = "USD"

// columns holds the resolved header indices for one CSV.
type columns struct {
	chargeID          int
	chargeCreation    int
	partnerShare      int
	partnerSale       int
	partnerSalePayout int
	shop              int
	chargeType        int
	currency          int
	hasCurrency       bool
}

// aggKey identifies one aggregate bucket.
type aggKey struct {
	lead     string
	month    string // YYYY-MM
	currency string
}

// aggEntry accumulates one bucket's running state.
type aggEntry struct {
	amount     float64
	subCount   int
	usageCount int
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
	cr.ReuseRecord = true // constant per-record allocation

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

	var (
		rowsRead      int
		skippedNoID   int
		matchedRows   int
		unmatchedRows int
		zeroSaleCount int
	)

	maxIdx := cols.maxIndex()
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse error at CSV line %d: %w", rowsRead+2, err)
		}
		rowsRead++
		if len(rec) <= maxIdx {
			return nil, fmt.Errorf("parse error at CSV line %d: row has %d fields, need at least %d", rowsRead+1, len(rec), maxIdx+1)
		}

		// 1. Skip refunds/downgrades/credits (empty Charge ID).
		if strings.TrimSpace(rec[cols.chargeID]) == "" {
			skippedNoID++
			continue
		}

		// 2. Currency scaling (with the zero Partner Sale guard).
		share, err := parseAmount(rec[cols.partnerShare])
		if err != nil {
			return nil, fmt.Errorf("parse error at CSV line %d: Partner Share: %w", rowsRead+1, err)
		}
		sale, err := parseAmount(rec[cols.partnerSale])
		if err != nil {
			return nil, fmt.Errorf("parse error at CSV line %d: Partner Sale: %w", rowsRead+1, err)
		}
		salePayout, err := parseAmount(rec[cols.partnerSalePayout])
		if err != nil {
			return nil, fmt.Errorf("parse error at CSV line %d: Partner Sale In Payout Currency: %w", rowsRead+1, err)
		}
		amount, ok := scaleAmount(share, sale, salePayout)
		if !ok {
			zeroSaleCount++
		}

		// 3. Date -> month.
		t, err := parseChargeTime(rec[cols.chargeCreation])
		if err != nil {
			return nil, fmt.Errorf("parse error at CSV line %d: %w", rowsRead+1, err)
		}
		month := monthKey(t)

		// 4. Join to a store.
		sub := normalizeSubdomain(rec[cols.shop])
		lead, matched := leadBySubdomain[sub]
		if !matched {
			unmatchedRows++
			u := unmatched[sub]
			if u == nil {
				u = &UnmatchedShop{Subdomain: sub, FirstMonth: month, LastMonth: month}
				unmatched[sub] = u
			}
			if month < u.FirstMonth {
				u.FirstMonth = month
			}
			if month > u.LastMonth {
				u.LastMonth = month
			}
			u.RowCount++
			u.TotalAmount += amount
			continue
		}

		// 5. Accumulate into the aggregate.
		matchedRows++
		currency := defaultCurrency
		if cols.hasCurrency {
			if c := strings.ToUpper(strings.TrimSpace(rec[cols.currency])); c != "" {
				currency = c
			}
		}
		key := aggKey{lead: lead, month: month, currency: currency}
		e := agg[key]
		if e == nil {
			e = &aggEntry{}
			agg[key] = e
		}
		e.amount += amount
		if !e.hasAny || t.After(e.maxAllTime) {
			e.maxAllTime = t
			e.hasAny = true
		}
		if isSubscription(rec[cols.chargeType]) {
			e.subCount++
			if !e.hasSub || t.After(e.maxSubTime) {
				e.maxSubTime = t
			}
			e.hasSub = true
		} else {
			e.usageCount++
		}
	}

	return buildResult(sm, prefix, agg, unmatched, stats{
		rowsRead:      rowsRead,
		skippedNoID:   skippedNoID,
		matchedRows:   matchedRows,
		unmatchedRows: unmatchedRows,
		zeroSaleCount: zeroSaleCount,
		hasCurrency:   cols.hasCurrency,
	}), nil
}

// stats carries the streaming counters into result assembly.
type stats struct {
	rowsRead      int
	skippedNoID   int
	matchedRows   int
	unmatchedRows int
	zeroSaleCount int
	hasCurrency   bool
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
	monthAmount := map[string]float64{}
	monthRows := map[string]int{}
	monthOrder := []string{}
	currencyAmount := map[string]float64{}
	currencyRows := map[string]int{}
	currencyOrder := []string{}
	var total float64
	newCount := 0

	for _, k := range keys {
		e := agg[k]
		r := round2(e.amount)
		revTime := e.maxAllTime
		if e.hasSub {
			revTime = e.maxSubTime
		}
		id := buildIdentifier(prefix, k.lead, k.month, k.currency)
		rows = append(rows, NeutralRow{
			LeadSlug:           k.lead,
			Amount:             money(r),
			RevenueDate:        dateKey(revTime),
			Description:        buildDescription(e.subCount, e.usageCount),
			ExternalIdentifier: id,
		})
		total += r
		if _, seen := monthAmount[k.month]; !seen {
			monthOrder = append(monthOrder, k.month)
		}
		monthAmount[k.month] += r
		monthRows[k.month]++
		if _, seen := currencyAmount[k.currency]; !seen {
			currencyOrder = append(currencyOrder, k.currency)
		}
		currencyAmount[k.currency] += r
		currencyRows[k.currency]++
		if _, found := existing[id]; !found {
			newCount++
		}
	}

	sort.Strings(monthOrder)
	byMonth := make([]MonthBucket, 0, len(monthOrder))
	for _, m := range monthOrder {
		byMonth = append(byMonth, MonthBucket{Month: m, Rows: monthRows[m], Amount: money(monthAmount[m])})
	}
	sort.Strings(currencyOrder)
	byCurrency := make([]CurrencyBucket, 0, len(currencyOrder))
	for _, c := range currencyOrder {
		byCurrency = append(byCurrency, CurrencyBucket{Currency: c, Rows: currencyRows[c], Amount: money(currencyAmount[c])})
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
		warnings = append(warnings, "no payout-currency column found in the CSV header; assumed USD for all rows (verify against the real export)")
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
		RowsRead:          st.rowsRead,
		SkippedNoChargeID: st.skippedNoID,
		MatchedRows:       st.matchedRows,
		UnmatchedRows:     st.unmatchedRows,
		UnmatchedShops:    len(worklist),
		AggregateRows:     len(rows),
		TotalAmount:       money(total),
		ByMonth:           byMonth,
		ByCurrency:        byCurrency,
		NewEstimate:       newEstimate,
		Guardrail:         guardrail,
		Prefix:            prefix,
		Warnings:          warnings,
	}

	return &Result{Rows: rows, Unmatched: worklist, Preview: preview}
}

// resolveColumns maps the required logical columns to header indices, failing
// with a message that names every missing column (data rows are never read
// when a column is missing).
func resolveColumns(header []string) (columns, error) {
	index := make(map[string]int, len(header))
	for i, h := range header {
		index[strings.ToLower(strings.TrimSpace(h))] = i
	}
	find := func(candidates []string) (int, bool) {
		for _, c := range candidates {
			if idx, ok := index[strings.ToLower(c)]; ok {
				return idx, true
			}
		}
		return -1, false
	}

	var c columns
	var missing []string
	req := func(candidates []string, label string, dst *int) {
		if idx, ok := find(candidates); ok {
			*dst = idx
			return
		}
		if len(candidates) == 1 {
			missing = append(missing, candidates[0])
		} else {
			missing = append(missing, fmt.Sprintf("%s (one of: %s)", label, strings.Join(candidates, ", ")))
		}
	}

	req(colChargeID, "charge id", &c.chargeID)
	req(colChargeCreation, "charge creation time", &c.chargeCreation)
	req(colPartnerShare, "partner share", &c.partnerShare)
	req(colPartnerSale, "partner sale", &c.partnerSale)
	req(colPartnerSalePayout, "partner sale in payout currency", &c.partnerSalePayout)
	req(colShop, "shop domain", &c.shop)
	req(colChargeType, "charge type", &c.chargeType)

	if idx, ok := find(colCurrency); ok {
		c.currency = idx
		c.hasCurrency = true
	}

	if len(missing) > 0 {
		return columns{}, fmt.Errorf("missing required column(s): %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// maxIndex is the highest header index the reducer reads, used to reject rows
// with too few fields.
func (c columns) maxIndex() int {
	m := c.chargeID
	for _, i := range []int{c.chargeCreation, c.partnerShare, c.partnerSale, c.partnerSalePayout, c.shop, c.chargeType} {
		if i > m {
			m = i
		}
	}
	if c.hasCurrency && c.currency > m {
		m = c.currency
	}
	return m
}
