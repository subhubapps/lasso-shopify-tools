package reduce

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// category is the per-charge classification. It is derived EXCLUSIVELY from the
// Shopify Charge ID GID (see classify), never from the diagnostic Charge Type
// or Category columns. This mirrors 03_aggregate_monthly.rb lines 62-68.
type category int

const (
	catSubscription category = iota
	catUsage
	catOther
)

// classify buckets a charge by its Charge ID GID substring, matching
// 03_aggregate_monthly.rb:
//
//	cid.include?('AppSubscription') -> :subscription
//	cid.include?('AppUsageRecord')  -> :usage
//	else                            -> :other
//
// The Charge ID is a GID like "gid://shopify/AppSubscription/123". The match is
// case-sensitive to mirror Ruby's String#include?.
func classify(chargeID string) category {
	switch {
	case strings.Contains(chargeID, "AppSubscription"):
		return catSubscription
	case strings.Contains(chargeID, "AppUsageRecord"):
		return catUsage
	default:
		return catOther
	}
}

// normalizeSubdomain lowercases and strips a trailing ".myshopify.com" so a
// map carrying full domains and a CSV carrying bare subdomains (or vice versa)
// join correctly. It also defensively strips a scheme and trailing slash so a
// value like "https://Acme.myshopify.com/" normalizes to "acme".
func normalizeSubdomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".myshopify.com")
	return s
}

// moneyCleaner strips thousands separators, common currency symbols, and spaces
// before an exact decimal parse.
var moneyCleaner = strings.NewReplacer(",", "", "$", "", "£", "", "€", "", " ", "")

// parseDecimal parses a money field into an EXACT big.Rat, tolerant of thousands
// separators, currency symbols, and surrounding whitespace. An empty field is 0
// (not an error), matching 02's `BigDecimal((row['...'] || '0').to_s)` where an
// absent CSV cell reads as nil and becomes "0". A non-empty non-numeric field is
// an error the caller turns into a skip (never a fatal abort of the whole run).
func parseDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Rat), nil
	}
	c := moneyCleaner.Replace(s)
	if c == "" {
		return new(big.Rat), nil
	}
	r, ok := new(big.Rat).SetString(c)
	if !ok {
		return nil, fmt.Errorf("invalid number %q", s)
	}
	return r, nil
}

// tenThousand scales a rational to ten-thousandths before rounding.
var tenThousand = big.NewInt(10000)

// scaleToTenThousandths converts Partner Share into the payout currency EXACTLY
// and rounds the per-charge result to 4 decimals, returning integer
// ten-thousandths. This mirrors 02_build_per_charge.rb:
//
//	partner_share_in_payout = partner_sale.zero? ? partner_share
//	                          : (partner_share * sale_in_payout / partner_sale).round(4)
//
// The whole computation is done in exact rational arithmetic (big.Rat), so there
// is no float64 drift, and the 4-decimal rounding is half away from zero to match
// BigDecimal#round's default. When Partner Sale is 0 the ratio is undefined; the
// unscaled Partner Share is used and ok=false signals the zero-sale warning.
func scaleToTenThousandths(share, sale, salePayout *big.Rat) (tt int64, ok bool) {
	var scaled *big.Rat
	if sale.Sign() == 0 {
		scaled = new(big.Rat).Set(share)
		ok = false
	} else {
		scaled = new(big.Rat).Mul(share, salePayout)
		scaled.Quo(scaled, sale)
		ok = true
	}
	return ratRoundTenThousandths(scaled), ok
}

// ratRoundTenThousandths rounds an exact rational to the nearest ten-thousandth,
// half away from zero, returning integer ten-thousandths (1e-4 units). This is
// the per-charge round(4) step; the caller accumulates these int64 values
// exactly (integer addition), so a bucket sum is bit-for-bit the BigDecimal sum
// of the per-charge round(4) amounts (03 sums the already-rounded per-charge
// values).
func ratRoundTenThousandths(r *big.Rat) int64 {
	num := new(big.Int).Mul(r.Num(), tenThousand)
	den := r.Denom() // always > 0 for a big.Rat
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(num, den, rem) // truncated division: q toward zero, rem has num's sign
	twoRem := new(big.Int).Abs(rem)
	twoRem.Lsh(twoRem, 1) // 2*|rem|
	if twoRem.Cmp(den) >= 0 {
		if num.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// ttToCents rounds integer ten-thousandths to integer cents (2 decimals), half
// away from zero. This is the neutral-row emission step: the bucket sum of
// per-charge ten-thousandths is rounded to the server-ready 2-decimal amount.
func ttToCents(tt int64) int64 {
	q := tt / 100
	r := tt % 100
	switch {
	case r >= 50:
		q++
	case r <= -50:
		q--
	}
	return q
}

// moneyFromCents renders integer cents as a clean two-decimal JSON number
// (e.g. 7200 -> "72.00", 1235 -> "12.35", -500 -> "-5.00").
func moneyFromCents(cents int64) json.Number {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if neg {
		s = "-" + s
	}
	return json.Number(s)
}

// moneyFromTenThousandths rounds ten-thousandths to cents (half away from zero)
// and renders the 2-decimal JSON number.
func moneyFromTenThousandths(tt int64) json.Number {
	return moneyFromCents(ttToCents(tt))
}

// plural returns the singular form when n == 1, otherwise the plural form.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}

// buildDescription formats "N Subscription(s) + M Usage Record(s) + K Other",
// exactly like 03_aggregate_monthly.rb's `describe` (lines 96-102): Subscription
// and Usage Record pluralize per count, "Other" is never pluralized, any
// zero-count term is dropped, and the terms join with " + ". Every emitted
// aggregate row has sub+usage+other >= 1, so this is never empty.
func buildDescription(sub, usage, other int) string {
	var parts []string
	if sub > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", sub, plural(sub, "Subscription", "Subscriptions")))
	}
	if usage > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", usage, plural(usage, "Usage Record", "Usage Records")))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("%d Other", other))
	}
	return strings.Join(parts, " + ")
}

// buildIdentifier builds the exact external identifier the server pins:
// <prefix>-<lead_slug>-<YYYY-MM>-<CCY>.
func buildIdentifier(prefix, leadSlug, month, currency string) string {
	return fmt.Sprintf("%s-%s-%s-%s", prefix, leadSlug, month, currency)
}

// chargeTimeLayouts are tried in order when parsing "Charge Creation Time".
// The list covers the common ISO-8601 / space-separated forms Shopify emits.
var chargeTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseChargeTime parses a Charge Creation Time value using chargeTimeLayouts.
// An empty or unrecognized value is an error the caller turns into a
// skipped_no_recorded_at row (per 02), never a fatal abort. The parsed time is
// used in its own location for month and revenue-date extraction, so results are
// consistent within a run.
func parseChargeTime(s string) (time.Time, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty Charge Creation Time")
	}
	for _, layout := range chargeTimeLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized Charge Creation Time %q", s)
}

// monthKey formats a time as YYYY-MM.
func monthKey(t time.Time) string { return t.Format("2006-01") }

// dateKey formats a time as YYYY-MM-DD.
func dateKey(t time.Time) string { return t.Format("2006-01-02") }
