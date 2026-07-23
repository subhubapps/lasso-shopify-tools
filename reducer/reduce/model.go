// Package reduce turns a Shopify Partner "Earnings" CSV into neutral revenue
// rows that the Lasso account_revenue_items_bulk_upsert MCP tool accepts
// verbatim. It streams the CSV record-by-record so memory stays bounded by the
// size of the aggregate (leads x months x currencies), never by the source row
// count. See the package README for the assumed CSV schema and money rules.
package reduce

import "encoding/json"

// Config is the resolved CLI input for a single reduction.
type Config struct {
	SrcPath      string // path to the Shopify Partner earnings CSV
	StoreMapPath string // path to the saved account_shopify_store_map JSON
	PrefixFlag   string // --prefix override; empty means "use the store map's importPrefix"
	OutDir       string // directory the three output files are written to
}

// StoreMap models the UNMODIFIED JSON result of the account_shopify_store_map
// MCP tool (LAS-62). Field casing is camelCase because it is a GraphQL-shaped
// tool result; the reducer's row OUTPUT is snake_case (see NeutralRow). That
// casing seam lives here, by design.
type StoreMap struct {
	Stores                       []Store           `json:"stores"`
	ExistingExternalIDs          []string          `json:"existingExternalIds"`
	ExistingExternalIDsTruncated bool              `json:"existingExternalIdsTruncated"`
	ImportPrefix                 string            `json:"importPrefix"`
	RevenueModeSignal            RevenueModeSignal `json:"revenueModeSignal"`
}

// Store is one Shopify-store-to-lead mapping. Subdomain is compared to the
// CSV's shop domain after normalization (lowercase + strip .myshopify.com on
// BOTH sides). AasmState is carried for fidelity but not used by the reducer.
type Store struct {
	Subdomain string `json:"subdomain"`
	LeadSlug  string `json:"leadSlug"`
	AasmState string `json:"aasmState"`
}

// RevenueModeSignal is LAS-62's forward-compatible guardrail signal. The
// reducer keys its guardrail EXCLUSIVELY off SettledUploadSafe; it never
// couples to ApiRevenueSyncDisabled (the raw interim boolean) and it ignores
// Mode (reserved for LAS-187). A nil SettledUploadSafe is treated as "not safe".
type RevenueModeSignal struct {
	// Mode is intentionally not modeled - it is reserved for LAS-187 and unused.
	ApiRevenueSyncDisabled *bool `json:"apiRevenueSyncDisabled"`
	SettledUploadSafe      *bool `json:"settledUploadSafe"`
}

// NeutralRow is one aggregate row, emitted with EXACTLY the snake_case fields
// account_revenue_items_bulk_upsert consumes - no extra or renamed fields.
// Amount is a json.Number so it serializes as a clean 2-decimal JSON number.
type NeutralRow struct {
	LeadSlug           string      `json:"lead_slug"`
	Amount             json.Number `json:"amount"`
	RevenueDate        string      `json:"revenue_date"`
	Description        string      `json:"description"`
	ExternalIdentifier string      `json:"external_identifier"`
}

// UnmatchedShop is one row of the store-linking worklist: a CSV shop domain
// that matched no store-map entry. Its rows NEVER contribute to the aggregate.
type UnmatchedShop struct {
	Subdomain   string
	FirstMonth  string // earliest YYYY-MM seen
	LastMonth   string // latest YYYY-MM seen
	RowCount    int
	TotalAmount float64 // summed scaled amount
}

// Preview is the human/agent-facing summary written to preview.json. Its counts
// are cross-file consistent: AggregateRows equals the aggregate.jsonl line
// count, the unmatched counts equal unmatched_shops.csv, and TotalAmount equals
// the sum of emitted row amounts.
type Preview struct {
	RowsRead          int              `json:"rows_read"`
	SkippedNoChargeID int              `json:"skipped_no_charge_id"`
	MatchedRows       int              `json:"matched_rows"`
	UnmatchedRows     int              `json:"unmatched_rows"`
	UnmatchedShops    int              `json:"unmatched_shops"`
	AggregateRows     int              `json:"aggregate_rows"`
	TotalAmount       json.Number      `json:"total_amount"`
	ByMonth           []MonthBucket    `json:"by_month"`
	ByCurrency        []CurrencyBucket `json:"by_currency"`
	NewEstimate       *int             `json:"new_estimate"`
	Guardrail         Guardrail        `json:"guardrail"`
	Prefix            string           `json:"prefix"`
	Warnings          []string         `json:"warnings"`
}

// MonthBucket is a per-month rollup: Rows is the number of aggregate rows in
// that month, Amount is their summed emitted amount.
type MonthBucket struct {
	Month  string      `json:"month"`
	Rows   int         `json:"rows"`
	Amount json.Number `json:"amount"`
}

// CurrencyBucket is a per-currency rollup with the same Rows/Amount semantics.
type CurrencyBucket struct {
	Currency string      `json:"currency"`
	Rows     int         `json:"rows"`
	Amount   json.Number `json:"amount"`
}

// Guardrail echoes the account-safety signal into the preview. SettledUploadSafe
// is the ONLY field the guardrail decision keys off; ApiRevenueSyncDisabled is
// carried for information only. Warning is set (and printed to stderr) when the
// account is not safe.
type Guardrail struct {
	SettledUploadSafe      bool   `json:"settled_upload_safe"`
	ApiRevenueSyncDisabled *bool  `json:"api_revenue_sync_disabled"`
	Warning                string `json:"warning"`
}

// Result bundles everything a reduction produces: the deterministically ordered
// neutral rows, the unmatched-shop worklist, and the preview.
type Result struct {
	Rows      []NeutralRow
	Unmatched []UnmatchedShop
	Preview   *Preview
}
