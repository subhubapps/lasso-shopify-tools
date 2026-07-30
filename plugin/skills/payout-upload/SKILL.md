---
name: payout-upload
description: >-
  Upload settled Shopify Partner payout revenue to Lasso from the Shopify
  Partner "Earnings" file (a CSV export). Use when a user wants to reconcile,
  import, or upload a Shopify Partner earnings or payout CSV into Lasso, or runs
  /lasso-shopify-payout-upload. Processes the earnings file on the user's own
  computer, shows a preview of the amounts, and uploads only the relevant rows
  after the user approves. The earnings file itself never leaves the computer.
---

# Shopify payout upload

You are driving a money-affecting reconciliation. Be precise, surface numbers,
and never commit without explicit approval. Follow the steps in order.

## What this does

The Shopify Partner "Earnings" CSV is huge (2-10 GB) and must never be read into
context or sent anywhere. A bundled, checksum-verified `reducer` binary streams
it locally and emits a tiny neutral aggregate (~hundreds of KB). Only those
aggregate rows travel to Lasso, through the `account_revenue_items_bulk_upsert`
MCP tool.

## Prerequisite (state this up front)

The account's **Shopify API revenue sync must be disabled** before the first
upload, or Lasso would double-count settled revenue against the API-synced
revenue. The server enforces this (it rejects `commit` while sync is active) and
so does this skill. See the README section "Before your first upload: turn off automatic Shopify revenue sync"
(the Lasso setting is named "Shopify API revenue sync"). The store map's `revenueModeSignal.settledUploadSafe` is the
signal for whether this has been done.

## MCP tools (bundled Lasso remote MCP; OAuth via `/mcp`)

Three account-scoped tools are provided by this plugin's bundled Lasso MCP
server. Their fully-scoped callable names are
`mcp__plugin_lasso-shopify-payout_lasso__<tool>`; referenced below by their
logical names:

- `account_shopify_store_map` -> `{ stores:[{subdomain, leadSlug, aasmState}],
  existingExternalIds:[String], existingExternalIdsTruncated:Bool,
  importPrefix:String, revenueModeSignal:{ mode, apiRevenueSyncDisabled,
  settledUploadSafe } }`. (Results are camelCase.)
- `account_revenue_items_bulk_upsert(rows, mode:"dry_run"|"commit",
  payout_currency)` where each row is `{ lead_slug, amount, revenue_date,
  description, external_identifier }` (snake_case). `dry_run` ->
  `{ new, updated, unchanged, delta_amount, per_row }`; `commit` ->
  `{ created, updated, skipped, errored, per_row }`. Server guards (fail closed):
  <= 1000 rows/call, identifiers pinned to `<prefix>-<lead_slug>-<YYYY-MM>-<CCY>`,
  `payout_currency` must equal the account currency, no concurrent batches,
  `commit` rejected while API revenue sync is active.
- `account_leads_missing_revenue_link(scope:"closed_won"|"active")` ->
  `[{ lead_slug, partner, company, missing:[] }]`.

**If the Lasso MCP server is not authenticated** (tools missing, or a call
returns 401/403/unauthorized): stop and tell the user to run `/mcp`, complete
the Lasso OAuth sign-in in the browser, and then re-run the command. Resume from
where you left off once authenticated. Do not try to work around it.

## Procedure

### 1. Ensure the reducer binary (fetch-on-first-run, checksum-verified)

Run the bundled fetch helper, which platform-detects, downloads the binary +
`SHA256SUMS` from the GitHub Release whose tag equals this plugin's version,
verifies SHA256 **before** first execution, caches it under
`~/.cache/lasso-shopify-tools/<version>/`, and reuses it offline thereafter. It
prints the verified binary path on stdout.

- macOS / Linux (and Windows with Git Bash):

  ```sh
  REDUCER="$(sh "${CLAUDE_PLUGIN_ROOT}/scripts/fetch-reducer.sh")"
  ```

- Native Windows PowerShell (no bash):

  ```powershell
  $REDUCER = & pwsh -File "${CLAUDE_PLUGIN_ROOT}/scripts/fetch-reducer.ps1"
  ```

If the helper exits non-zero it prints nothing usable on stdout - report its
stderr and stop. A checksum mismatch is fatal by design: never run an unverified
binary. (`"$REDUCER" --version` should print this plugin's version.)

### 2. Fetch the store map and save it verbatim

Call `account_shopify_store_map`. Save the raw JSON result, unmodified, to
`map.json` in the working directory. This file is the reducer's `--store-map`
input; do not transform or hand-edit it.

### 3. GUARDRAIL - check `settledUploadSafe`

Read `revenueModeSignal.settledUploadSafe` from the store map.

- If it is `false` (or missing/null): the account is **not safe** - Shopify API
  revenue sync is still active. Warn the user clearly, cite the prerequisite
  ("Shopify API revenue sync", see the README prerequisite section), and mark this session
  **commit-refused**. You MAY still reduce, preview, and run `dry_run` (which
  writes nothing), but you MUST refuse every `commit` for the rest of the
  session, exactly as the server would.
- If it is `true`: proceed normally.

### 4. Run the reducer

Create the output directory and run the reducer against the user's CSV. Do
**not** pass `--prefix`: the reducer defaults to the store map's `importPrefix`,
which is the server-pinned prefix.

```sh
mkdir -p ./out
"$REDUCER" --src "<path-to-earnings.csv>" --store-map map.json --out ./out
```

Only honor an explicit user-supplied prefix override by adding
`--prefix <value>`, and when you do, warn that the server pins identifiers to
the account's registered prefix and will reject a mismatched override at
`dry_run`.

Surface any reducer stderr warnings and the `preview.json.warnings` array (for
example, zero-`Partner Sale` rows that were counted unscaled).

The reducer writes three files under `./out`:
- `aggregate.jsonl` - one neutral row per line, ordered by lead_slug, month,
  currency (a lead's rows are always contiguous).
- `unmatched_shops.csv` - shops with no lead link (the follow-up worklist).
- `preview.json` - the summary you present next.

### 5. Present the preview (Strategy A - offline)

Read `preview.json` and show the user:
- `rows_read`, `skipped_no_charge_id`, `matched_rows`, `unmatched_rows`,
  `unmatched_shops`, `aggregate_rows`, `total_amount`.
- The `by_month` table and `by_currency` breakdown.
- `prefix` and the guardrail block.
- **Strategy A estimate:** `new_estimate` = how many emitted identifiers are NOT
  already in the account (offline diff against `existingExternalIds`).
  - If `new_estimate` is `null` because `existingExternalIdsTruncated` was true,
    say the offline estimate is unavailable and that the server `dry_run`
    (Strategy B) will be the only reliable preview.

### 6. Determine `payout_currency` and batch by lead (<= 1000 rows)

- `payout_currency`: use the currency from `by_currency`. In the normal case
  there is exactly one. If several appear, upload only the **account-currency**
  group this run (the server rejects any other), filter `aggregate.jsonl` to
  rows whose `external_identifier` ends in `-<CCY>` for that currency, and report
  the other currency groups as not uploaded.
- Batch the rows into `account_revenue_items_bulk_upsert` calls of **at most
  1000 rows**, never splitting one lead across batches. Because rows are sorted
  by lead_slug, group contiguous rows by lead_slug and close the current batch
  before adding a lead group that would push it over 1000:

  ```
  batches = []; current = []
  for each contiguous group G of rows with the same lead_slug:
      if len(current) + len(G) > 1000: batches.append(current); current = []
      current += G
  if current: batches.append(current)
  ```

  A single lead's group is months x currencies (tens at most), so it can never
  exceed a batch on its own.

### 7. Dry-run every batch (Strategy B - server-authoritative)

For each batch in order, call
`account_revenue_items_bulk_upsert(rows=<batch>, mode="dry_run",
payout_currency=<currency>)`. Never run batches concurrently. Sum the per-batch
`{ new, updated, unchanged, delta_amount }` into combined totals and present them
as the authoritative preview.

If Strategy A was available and its `new_estimate` diverges materially from
Strategy B's combined `new`, surface the divergence to the user before asking
for approval.

### 8. EXPLICIT approval gate

Show the combined `dry_run` totals (new / updated / unchanged / delta_amount,
batch count, row count, `payout_currency`) and ask the user to approve the
commit. **No commit without a clear "yes" in this session.** If the session is
commit-refused (step 3) or the user does not clearly approve, stop here and
summarize what would have been uploaded - dry_run wrote nothing.

### 9. Commit (replay the identical batches, sequentially)

Only after approval, and only if not commit-refused: re-send each batch, in the
same order, with `mode="commit"` and the same `payout_currency`. One at a time -
never concurrently. For each batch report `{ created, updated, skipped,
errored }`, and print every `errored` row and its server error **verbatim**.
Aggregate a final summary across all batches.

### 10. Follow-up worklist

- If `unmatched_shops.csv` has rows, present it as the store-linking worklist
  (subdomain, first/last month, row_count, total_amount).
- Offer to call `account_leads_missing_revenue_link(scope="closed_won")` to list
  leads lacking revenue linkage.
- Note that re-running the upload after shops are linked / leads added is safe:
  the server upsert is idempotent - already-uploaded months come back as
  `unchanged`/`skipped` and a re-commit creates 0 new rows. Recommend a re-run
  after partial failures or new links.

## Notes

- Casing seam: store map results are camelCase; the reducer emits snake_case row
  fields that `account_revenue_items_bulk_upsert` consumes verbatim. Do not
  rename fields in either direction.
- `map.json`, `./out`, and the cached reducer binary are the only artifacts;
  `map.json` and `./out` live in the working directory and are safe to delete
  after the run.
