# reducer

A zero-dependency, constant-memory CLI that turns a multi-GB Shopify Partner
**Earnings** CSV into neutral revenue rows Lasso's
`account_revenue_items_bulk_upsert` MCP tool accepts verbatim.

It streams the CSV record-by-record (`encoding/csv` over `bufio`), so peak
memory is bounded by the size of the **aggregate** (leads x months x
currencies), not by the source row count. The multi-GB CSV never leaves the
machine; only the reduced aggregate (~hundreds of KB at Loop scale) is meant to
travel onward. Stdlib only, builds static with `CGO_ENABLED=0`.

> Part A of LAS-188. The Claude plugin, marketplace packaging, and release
> automation are a separate task; this directory is only the Go reducer.

## Usage

```
reducer --src earnings.csv --store-map map.json [--prefix loop-monthly] --out ./out
reducer --version
```

| flag          | required | meaning                                                        |
| ------------- | -------- | -------------------------------------------------------------- |
| `--src`       | yes      | path to the Shopify Partner earnings CSV                       |
| `--store-map` | yes      | path to the **saved, unmodified** `account_shopify_store_map` JSON |
| `--prefix`    | no       | import prefix override; defaults to the store map's `importPrefix` |
| `--out`       | yes      | output directory for the three artifacts below                 |
| `--version`   | -        | print the build-stamped version and exit 0                     |

**Prefix resolution order:** `--prefix` flag → the store map's `importPrefix` →
fatal usage error if neither is present. The plugin always passes the map's
`importPrefix`; the flag is an ops/testing escape hatch. A wrong override still
fails closed server-side at dry_run (the server pins the prefix).

## Store-map input (camelCase — the LAS-62 tool result, verbatim)

`--store-map` is the JSON result of the `account_shopify_store_map` MCP tool,
saved to disk with **no edits**. The reducer reads exactly these fields:

```json
{
  "stores": [
    { "subdomain": "acme", "leadSlug": "REF-ABC", "aasmState": "closed_won" }
  ],
  "existingExternalIds": ["loop-monthly-REF-ABC-2026-01-USD"],
  "existingExternalIdsTruncated": false,
  "importPrefix": "loop-monthly",
  "revenueModeSignal": {
    "mode": null,
    "apiRevenueSyncDisabled": true,
    "settledUploadSafe": true
  }
}
```

- `stores[].subdomain` / `leadSlug` — the shop→lead join. `aasmState` is carried
  but unused by the reducer.
- `existingExternalIds` — used only for the offline `new_estimate`.
- `existingExternalIdsTruncated` — when `true`, `new_estimate` is reported as
  `null` (an offline estimate would be a lie; rely on the server dry_run).
- `importPrefix` — the default prefix.
- `revenueModeSignal.settledUploadSafe` — **the only** field the guardrail keys
  off. `apiRevenueSyncDisabled` is echoed for information only; `mode` (reserved
  for LAS-187) is ignored. A missing/`null` `settledUploadSafe` is treated as
  **not safe**.

## Neutral-row output (snake_case — the bulk-upsert row input, exactly)

Each aggregate entry is emitted as one line of `aggregate.jsonl`:

```json
{ "lead_slug": "LD-123", "amount": 1234.56, "revenue_date": "2026-03-28",
  "description": "3 Subscriptions + 2 Usage Records + 1 Other",
  "external_identifier": "loop-monthly-LD-123-2026-03-USD" }
```

- `amount` — the bucket sum of the per-charge scaled amounts (each rounded to 4
  decimals in exact rational arithmetic, then summed exactly as integer
  ten-thousandths), rounded half away from zero to 2 decimals at emission and
  written as a clean 2-decimal JSON number.
- `revenue_date` — `YYYY-MM-DD`; the date of the latest **subscription** charge
  in the month, falling back to the latest of **all** charges for months with no
  subscription charge (`anchor_date = last_sub_date || last_date`).
- `description` — `"N Subscription(s) + M Usage Record(s) + K Other"`,
  `Subscription`/`Usage Record` pluralized per count, `Other` never pluralized,
  any zero-count term dropped, terms joined with `" + "` (`1 Subscription`,
  `4 Usage Records`, `2 Subscriptions + 1 Usage Record + 1 Other`).
- `external_identifier` — `<prefix>-<lead_slug>-<YYYY-MM>-<CCY>` (the exact form
  the server pins).

The casing seam (camelCase in, snake_case out) lives once, here, by design.

## Shopify Partner "Earnings" CSV schema

These are the exact columns `02_build_per_charge.rb` reads. Columns are resolved
by header **name** on the first record, case-insensitively and tolerant of column
order. If any required column is missing the reducer exits 1 before reading any
data row, naming every missing column. Extra columns are ignored.

**Required columns**

| header name                       | meaning                                             |
| --------------------------------- | --------------------------------------------------- |
| `Shop`                            | shop domain, joined to the store map                |
| `Charge ID`                       | the charge GID; empty → refund/credit, skipped      |
| `Charge Creation Time`            | ISO-8601 / space-separated timestamp                |
| `Partner Share`                   | partner earnings in the sale currency               |
| `Partner Sale`                    | sale amount in the sale currency                    |
| `Partner Sale In Payout Currency` | sale amount in the payout currency                  |

**Optional column**

| header name       | if absent                                            |
| ----------------- | ---------------------------------------------------- |
| `Payout Currency` | every row is treated as `USD`, with a warning        |

Blank `Payout Currency` **values** (column present) default to `USD` and are
counted in a warning, per `03_aggregate_monthly.rb`.

**Diagnostic-only columns:** `Charge Type` and `Category` are **never required
and never used**. Classification does not read them.

**Classification — by the `Charge ID` GID, not any column.** The `Charge ID` is
a Shopify GID like `gid://shopify/AppSubscription/123`. Matching
`03_aggregate_monthly.rb` exactly:

| `Charge ID` contains | category       |
| -------------------- | -------------- |
| `AppSubscription`    | subscription   |
| `AppUsageRecord`     | usage record   |
| anything else        | other          |

The match is a case-sensitive substring test (Ruby `String#include?`). It is the
single place — `classify` in `reduce/helpers.go` — that drives the `description`
counts, the category counters, and the subscription-anchored `revenue_date`.

**Timestamp parsing:** `Charge Creation Time` is parsed with a small set of
ISO-8601 / space-separated layouts (see `chargeTimeLayouts`) and used in its own
zone to derive both the month bucket and the `revenue_date` day.

**Number parsing:** money fields tolerate thousands separators, `$`/`£`/`€`
symbols, and surrounding whitespace, and are parsed into exact rationals
(`math/big`), never float64. An empty money field is `0`.

## Money rules

Per charge, in exact rational arithmetic (matching 02's BigDecimal):

`amount = round(Partner Share × Partner Sale In Payout Currency / Partner Sale, 4)`

The per-charge result is rounded to **4 decimals** and accumulated as exact
integer ten-thousandths, so a bucket sum is bit-for-bit the sum of the per-charge
`round(4)` amounts — no float64 drift. The bucket sum is then rounded **half away
from zero to 2 decimals** for the server-ready neutral-row `amount`.

- **Zero `Partner Sale` guard:** the ratio is undefined, so the unscaled
  `Partner Share` is used and a preview warning is recorded — never a
  divide-by-zero, never a silently dropped row.

**Skip-and-count (never fatal on one row).** A single malformed data row is
skipped and counted, never aborting a multi-GB run. Matching `02`, for rows whose
shop joins to a lead:

- empty `Charge ID` (refunds/downgrades/credits) → `skipped_no_charge_id`
- blank or non-numeric `Partner Share` → `skipped_blank_amount`
- unparseable `Charge Creation Time` → `skipped_no_recorded_at`

A run is fatal (exit 1) **only** for structural errors: a missing required
column, an unreadable source file or store map, malformed store-map JSON, or a
genuine CSV quoting error that desyncs record boundaries. Ragged (short) rows are
tolerated (`FieldsPerRecord = -1`) and fall into the skip counters above.

The reducer also **normalizes** the shop subdomain (lowercase + strip a trailing
`.myshopify.com` on **both** the CSV value and the store-map subdomain, plus a
defensive scheme/trailing-slash strip) before joining, and accumulates unmatched
shops into a worklist that **never** contributes to the aggregate.

## Outputs (written under `--out`)

- **`aggregate.jsonl`** — one neutral row per line, ordered deterministically by
  `lead_slug`, then `YYYY-MM`, then currency.
- **`unmatched_shops.csv`** — the store-linking worklist, header always present:
  `subdomain,first_month,last_month,row_count,total_amount`.
- **`preview.json`** — a cross-file-consistent summary:

  ```json
  {
    "rows_read": 20,
    "skipped_no_charge_id": 1,
    "skipped_blank_amount": 1,
    "skipped_no_recorded_at": 1,
    "matched_rows": 15,
    "unmatched_rows": 2,
    "unmatched_shops": 1,
    "aggregate_rows": 6,
    "subscription_count": 9,
    "usage_record_count": 5,
    "other_count": 1,
    "total_amount": 594.35,
    "by_month": [{ "month": "2026-01", "rows": 2, "amount": 362.00 }],
    "by_currency": [{ "currency": "USD", "rows": 6, "amount": 594.35 }],
    "new_estimate": 5,
    "guardrail": { "settled_upload_safe": true, "api_revenue_sync_disabled": true, "warning": "" },
    "prefix": "loop-monthly",
    "warnings": ["1 row(s) had a zero Partner Sale; used the unscaled Partner Share for those rows"]
  }
  ```

  The three `skipped_*` counters are matched-shop rows dropped for bad data (an
  empty `Charge ID`, a blank/non-numeric `Partner Share`, or an unparseable
  `Charge Creation Time`). The `subscription_count` / `usage_record_count` /
  `other_count` are run totals of the GID classification across accumulated rows.
  `aggregate_rows` equals the `aggregate.jsonl` line count; the unmatched counts
  equal `unmatched_shops.csv`; `total_amount` equals the sum of emitted amounts.
  `new_estimate` is the count of emitted identifiers **not** in
  `existingExternalIds` (or `null` + a warning when the inventory was truncated).

## Guardrail and exit codes

When `revenueModeSignal.settledUploadSafe` is `false` (or absent), the reducer
prints a prominent `WARNING` to **stderr**, sets `guardrail.settled_upload_safe:
false` with the message in `preview.json` — and still completes the reduction
and **exits 0** (reducing is read-only; refusing a commit is the plugin's/server's
concern). The guardrail keys off `settledUploadSafe` only, never
`apiRevenueSyncDisabled`.

- **exit 0** — success, including the unsafe-guardrail case and `--version`.
  Malformed **data** rows (empty `Charge ID`, blank/non-numeric `Partner Share`,
  unparseable `Charge Creation Time`, ragged rows) are skip-and-counted, not
  fatal — a single bad row never aborts a multi-GB run.
- **exit 1** — **structural** errors only: usage errors (missing/invalid flags,
  no resolvable prefix), IO errors, a malformed store map, a missing required
  column, or a genuine CSV quoting error that desyncs record boundaries. On a
  usage/prefix/missing-column error no output files are written.

## Build, test, validate

```sh
gofmt -l .                                   # prints nothing when formatted
CGO_ENABLED=0 go build ./...                 # static, no cgo
go vet ./...
go test ./...                                # add -short to skip the 1M-row memory test
go test ./... -run Golden -update            # regenerate golden files after an intentional change

# version stamping (as the release build does):
CGO_ENABLED=0 go build -ldflags "-X main.version=v0.1.0" -o /tmp/reducer .
/tmp/reducer --version                       # -> v0.1.0
```

Golden fixtures and the constant-memory proof live in `testdata/` and
`reduce/reduce_test.go`.

## Provenance

The schema, classification, currency scaling, monthly aggregation, description,
identifier, and skip rules all mirror the proven ops scripts
`02_build_per_charge.rb` (per-charge conversion) and `03_aggregate_monthly.rb`
(monthly aggregation) — the source of truth for idempotency with Loop's existing
revenue items. The reducer streams both steps in one pass. Two intentional
differences from the scripts:

- The neutral-row `amount` is rounded to **2 decimals** here (server-ready),
  whereas 03's CSV emits the un-rounded BigDecimal sum for the admin bulk-update
  job to round. The 4-decimal per-charge rounding and exact summation are
  identical.
- A single malformed data row is skip-and-counted rather than aborting the run
  (02 would raise on a non-numeric `Partner Share`), so a multi-GB run survives
  one bad row. Blank `Partner Share`, empty `Charge ID`, and unparseable
  `Charge Creation Time` match 02's counters exactly.

The module path in `go.mod` is intentionally org-neutral for Part A; the
publish task resolves the GitHub org (design Open Question 1) and rewrites it.
