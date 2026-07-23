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
  "description": "3 Subscriptions + 2 Usage Records",
  "external_identifier": "loop-monthly-LD-123-2026-03-USD" }
```

- `amount` — summed, scaled, rounded half-away-from-zero to 2 decimals, emitted
  as a clean 2-decimal JSON number.
- `revenue_date` — `YYYY-MM-DD`; the date of the latest **subscription** charge
  in the month, falling back to the latest of **all** charges for usage-only
  months.
- `description` — `"N Subscription(s) + M Usage Record(s)"`, pluralized per
  count, zero-count terms dropped (`1 Subscription`, `4 Usage Records`,
  `2 Subscriptions + 1 Usage Record`).
- `external_identifier` — `<prefix>-<lead_slug>-<YYYY-MM>-<CCY>` (the exact form
  the server pins).

The casing seam (camelCase in, snake_case out) lives once, here, by design.

## Assumed Shopify Partner "Earnings" CSV schema

The exact export schema is an **assumption to verify against a real export.**
Columns are resolved by header **name** on the first record, case-insensitively
and tolerant of column order. If any required column is missing the reducer
exits 1 before reading any data row, naming every missing column.

**Required columns**

| logical column                    | header name(s) tried (first present wins)                                 |
| --------------------------------- | ------------------------------------------------------------------------- |
| Charge ID                         | `Charge ID`                                                               |
| Charge Creation Time              | `Charge Creation Time`                                                    |
| Partner Share                     | `Partner Share`                                                          |
| Partner Sale                      | `Partner Sale`                                                           |
| Partner Sale In Payout Currency   | `Partner Sale In Payout Currency`                                        |
| shop domain                       | `Shop`, `Store`, `Myshopify Domain`, `Shop Domain`, `Store Domain`, `Shop Name`, `Domain` |
| charge type                       | `Type`, `Charge Type`                                                    |

**Optional column**

| logical column   | header name(s) tried                                            | if absent                          |
| ---------------- | --------------------------------------------------------------- | ---------------------------------- |
| payout currency  | `Payout Currency`, `Currency`, `Charge Currency`, `Partner Sale Currency` | every row is treated as `USD`, with a warning |

**Charge-type classification (documented default):** a charge whose lowercased
type contains `subscription` **or** `recurring` is a **subscription**; every
other type is a **usage record** (design D5: "usage record = everything else").
This is the single knob to adjust — `subscriptionKeywords` in `reduce/helpers.go`
— if a real export uses other spellings. The classification is what drives the
`description` counts and the subscription-aware `revenue_date`.

**Timestamp parsing:** `Charge Creation Time` is parsed with a small set of
ISO-8601 / space-separated layouts (see `chargeTimeLayouts`) and used in its own
zone to derive both the month bucket and the `revenue_date` day.

**Number parsing:** money fields tolerate thousands separators, `$`/`£`/`€`
symbols, and surrounding whitespace. An empty money field is `0`; a non-empty
non-numeric field is a fatal parse error.

## Money rules

Per row: `amount = Partner Share × (Partner Sale In Payout Currency / Partner Sale)`.

- **Zero `Partner Sale` guard:** the ratio is undefined, so the unscaled
  `Partner Share` is used and a preview warning is recorded — never a
  divide-by-zero, never a silently dropped row.
- **Rounding:** half away from zero to 2 decimals at emission (float64, matching
  the server's `%.2f`). Float representation applies (e.g. `2.675` → `2.67`);
  the design accepts float64 for these payout-dollar magnitudes.

Per row the reducer also: **skips** rows with an empty `Charge ID` (refunds,
downgrades, credits — counted as `skipped_no_charge_id`); **normalizes** the
shop subdomain (lowercase + strip a trailing `.myshopify.com` on **both** the
CSV value and the store-map subdomain, plus a defensive scheme/trailing-slash
strip) before joining; and accumulates unmatched shops into a worklist that
**never** contributes to the aggregate.

## Outputs (written under `--out`)

- **`aggregate.jsonl`** — one neutral row per line, ordered deterministically by
  `lead_slug`, then `YYYY-MM`, then currency.
- **`unmatched_shops.csv`** — the store-linking worklist, header always present:
  `subdomain,first_month,last_month,row_count,total_amount`.
- **`preview.json`** — a cross-file-consistent summary:

  ```json
  {
    "rows_read": 14,
    "skipped_no_charge_id": 2,
    "matched_rows": 10,
    "unmatched_rows": 2,
    "unmatched_shops": 1,
    "aggregate_rows": 4,
    "total_amount": 477.00,
    "by_month": [{ "month": "2026-01", "rows": 2, "amount": 357.00 }],
    "by_currency": [{ "currency": "USD", "rows": 4, "amount": 477.00 }],
    "new_estimate": 3,
    "guardrail": { "settled_upload_safe": true, "api_revenue_sync_disabled": true, "warning": "" },
    "prefix": "loop-monthly",
    "warnings": []
  }
  ```

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
- **exit 1** — usage errors (missing/invalid flags, no resolvable prefix), IO
  errors, a malformed store map, a missing required column, or a CSV parse error
  (bad quoting, ragged row, non-numeric money, unparseable timestamp). On a
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

## Open risks / assumptions to confirm against a real export

- **Column names** (`Shop` vs `Store` vs `Myshopify Domain`, `Type` vs
  `Charge Type`) and the **charge-type spellings** that mean "subscription" —
  the current keyword set (`subscription`, `recurring`) is a best guess.
- Whether the export carries a **payout-currency column** (else USD is assumed).
- The **timestamp** layout(s) actually used.
- The **empty-`Charge ID` = refund/credit** rule (from the account-498
  precedent) — confirm no legitimate revenue rows have an empty `Charge ID`.

The module path in `go.mod` is intentionally org-neutral for Part A; the
publish task resolves the GitHub org (design Open Question 1) and rewrites it.
