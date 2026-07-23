package reduce

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// subscriptionKeywords drive the documented default charge-type classification.
// A charge whose (lowercased) type contains any of these is a SUBSCRIPTION;
// everything else is a USAGE record (design D5: "usage record = everything
// else"). "recurring" is included because real Shopify types read
// "Recurring application charge"; verify/extend this set against a real export.
// This is the single place to change the classification rule.
var subscriptionKeywords = []string{"subscription", "recurring"}

// isSubscription reports whether a charge type classifies as a subscription.
func isSubscription(chargeType string) bool {
	t := strings.ToLower(chargeType)
	for _, kw := range subscriptionKeywords {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
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

// parseAmount parses a money field tolerant of thousands separators, common
// currency symbols, and surrounding whitespace. An empty field is 0 (not an
// error); a non-empty non-numeric field is an error (a fatal parse error).
func parseAmount(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	cleaner := strings.NewReplacer(",", "", "$", "", "£", "", "€", "", " ", "")
	c := cleaner.Replace(s)
	if c == "" {
		return 0, nil
	}
	return strconv.ParseFloat(c, 64)
}

// scaleAmount converts Partner Share into the payout currency:
//
//	amount = partnerShare * (partnerSalePayout / partnerSale)
//
// When partnerSale is 0 the ratio is undefined; the unscaled partnerShare is
// used and (ok=false) signals the caller to record a warning. It never divides
// by zero and never silently drops the row.
func scaleAmount(partnerShare, partnerSale, partnerSalePayout float64) (amount float64, ok bool) {
	if partnerSale == 0 {
		return partnerShare, false
	}
	return partnerShare * (partnerSalePayout / partnerSale), true
}

// round2 rounds half away from zero to two decimals. math.Round is defined as
// round-half-away-from-zero, matching the design's stated rule and the server's
// %.2f serialization. Float representation applies (e.g. 2.675 -> 2.67); the
// design accepts float64 for these payout-dollar magnitudes.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// money renders x as a clean two-decimal JSON number (e.g. 72 -> "72.00",
// 1234.56 -> "1234.56"). It pre-rounds with round2 so the tie-break is
// half-away-from-zero, then formats, so no additional rounding rule applies.
func money(x float64) json.Number {
	return json.Number(strconv.FormatFloat(round2(x), 'f', 2, 64))
}

// plural returns the singular form when n == 1, otherwise the plural form.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}

// buildDescription formats "N Subscription(s) + M Usage Record(s)", pluralized
// per count, dropping any zero-count term. With binary classification an
// emitted entry always has sub+usage >= 1, so this is never empty.
func buildDescription(sub, usage int) string {
	var parts []string
	if sub > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", sub, plural(sub, "Subscription", "Subscriptions")))
	}
	if usage > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", usage, plural(usage, "Usage Record", "Usage Records")))
	}
	return strings.Join(parts, " + ")
}

// buildIdentifier builds the exact external identifier the server pins:
// <prefix>-<lead_slug>-<YYYY-MM>-<CCY>.
func buildIdentifier(prefix, leadSlug, month, currency string) string {
	return fmt.Sprintf("%s-%s-%s-%s", prefix, leadSlug, month, currency)
}

// chargeTimeLayouts are tried in order when parsing "Charge Creation Time".
// The exact Shopify layout is an assumption; this list covers the common
// ISO-8601 / space-separated forms. Verify against a real export.
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
// The parsed time is used in its own location for month and revenue-date
// extraction, so results are consistent within a run.
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
