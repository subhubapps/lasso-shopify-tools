# lasso-shopify-tools

Reconcile settled **Shopify Partner payout** revenue into Lasso from the Shopify
Partner "Earnings" CSV, without the multi-GB file ever leaving your machine.

This repo ships two things, versioned together by one git tag:

- **`reducer/`** - a zero-dependency, constant-memory Go CLI that streams the
  earnings CSV locally and emits a tiny neutral aggregate.
- **`plugin/`** - a Claude Code plugin (`lasso-shopify-payout`) that wraps the
  whole flow in one command: fetch the store map, guardrail, reduce, preview,
  explicit approval, batched upload, follow-up worklist. It bundles the hosted
  Lasso remote MCP server, so the upload tools arrive with the plugin.

---

## Prerequisite: disable Shopify API revenue sync (do this first)

**Before your first upload, disable the account's Shopify API revenue sync in
Lasso.** Settled payout upload and API revenue sync are two views of the same
money; running both double-counts revenue. This is a one-time, per-account
change.

The safety is enforced in three places:

1. The Lasso server rejects any `commit` while API revenue sync is still active.
2. The `reducer` prints a prominent warning and flags `preview.json` when the
   account is not safe.
3. The plugin **refuses every commit** for the session when the store map
   reports `revenueModeSignal.settledUploadSafe = false` (reduce, preview, and
   `dry_run` still work, so you can rehearse safely).

If a guardrail refuses your commit, come back to this section: it means Shopify
API revenue sync has not been disabled for the account yet.

---

## Install (4 steps, individual Claude Code plan is fine)

Run these inside Claude Code:

1. Add this repo as a plugin marketplace:

   ```
   /plugin marketplace add subhubapps/lasso-shopify-tools
   ```

2. Install the plugin:

   ```
   /plugin install lasso-shopify-payout@lasso-shopify-tools
   ```

3. Authenticate the bundled Lasso MCP server (one-time OAuth in your browser):

   ```
   /mcp
   ```

4. Run the upload against your earnings CSV:

   ```
   /lasso-shopify-payout:lasso-shopify-payout-upload /absolute/path/to/earnings.csv
   ```

> **Command name.** Claude Code namespaces every plugin command as
> `/<plugin>:<command>`, so the working invocation is
> `/lasso-shopify-payout:lasso-shopify-payout-upload`. You can also just ask
> Claude to "upload my Shopify payout CSV at <path>" and it will invoke the same
> `payout-upload` skill.

On first run the plugin fetches the platform-matched `reducer` binary from this
repo's GitHub Release, verifies its SHA256, and caches it under
`~/.cache/lasso-shopify-tools/<version>/`. Later runs reuse the cache with no
network.

---

## What happens in a run

1. **Store map.** Calls `account_shopify_store_map` and saves the raw JSON to
   `map.json` (the reducer's input).
2. **Guardrail.** Keys off `revenueModeSignal.settledUploadSafe`; refuses commit
   for the session if unsafe.
3. **Reduce.** Runs `reducer --src <csv> --store-map map.json --out ./out`. The
   prefix comes from the store map's `importPrefix` automatically.
4. **Preview.** Shows totals, a by-month table, the offline `new_estimate`
   (Strategy A), and the unmatched-shop worklist size.
5. **Dry run.** Batches the aggregate **by lead** into `<= 1000`-row calls (a
   lead never straddles a batch) and runs each as
   `account_revenue_items_bulk_upsert(mode: "dry_run")` - the server-authoritative
   preview (Strategy B).
6. **Explicit approval.** Nothing is committed without your clear "yes" after the
   preview.
7. **Commit.** Replays the identical batches with `mode: "commit"`, sequentially,
   and reports `created / updated / skipped / errored` per batch.
8. **Follow-up.** Surfaces `unmatched_shops.csv` and offers
   `account_leads_missing_revenue_link(scope: "closed_won")`. Re-runs are
   idempotent (already-uploaded months come back `unchanged`).

---

## Your data stays on your machine

The Shopify Partner earnings CSV is often several GB. It is read **only** by the
local `reducer` binary, in a single streaming pass with memory bounded by the
aggregate (leads x months x currencies), not the row count. It is never opened
in chat, never sent to Lasso, and never uploaded anywhere.

What travels over the wire is only the reduced aggregate: neutral rows of the
form

```json
{ "lead_slug": "LD-123", "amount": 1234.56, "revenue_date": "2026-03-28",
  "description": "3 Subscriptions + 2 Usage Records",
  "external_identifier": "loop-monthly-LD-123-2026-03-USD" }
```

- roughly hundreds of KB even at Loop scale.

---

## Hosted MCP endpoint (prod vs pre-prod)

The plugin ships pointed at **production**:

```
https://api.lassotech.com/api/v1/mcp
```

The pre-production / QA endpoint (used while LAS-62 is not yet in prod) is:

```
https://qa-lasso-core.onrender.com/api/v1/mcp
```

To validate against QA, edit `url` in `plugin/.mcp.json` locally (or override it
in your own MCP config) and complete `/mcp` OAuth against that host. OAuth is
always completed by you via `/mcp`; no tokens are stored in the repo.

---

## Distribution, releases, and the binary trust boundary

- **One tag versions everything.** The reducer binary, `plugin/.claude-plugin/
  plugin.json`'s `version`, and the marketplace entry move together. The release
  workflow fails if the pushed tag does not equal `v<plugin.json version>`, and
  the fetch script resolves the release by the plugin's own version - so an
  installed plugin can never run a mismatched reducer.

- **Release assets.** Pushing a `v*` tag builds `CGO_ENABLED=0` static reducers
  for 5 targets via goreleaser and publishes exactly these assets:

  | GOOS/GOARCH     | asset name                  |
  | --------------- | --------------------------- |
  | darwin/arm64    | `reducer_darwin_arm64`      |
  | darwin/amd64    | `reducer_darwin_amd64`      |
  | linux/amd64     | `reducer_linux_amd64`       |
  | linux/arm64     | `reducer_linux_arm64`       |
  | windows/amd64   | `reducer_windows_amd64.exe` |
  | (checksums)     | `SHA256SUMS`                |

  The asset-name scheme is `reducer_<goos>_<goarch>` (goreleaser appends `.exe`
  for windows). `plugin/scripts/fetch-reducer.{sh,ps1}` construct exactly these
  names from `uname` / `$PROCESSOR_ARCHITECTURE`, so the download URL always
  matches a real asset.

- **Verify-before-exec.** The fetch script downloads the binary and `SHA256SUMS`,
  computes the binary's SHA256, and refuses to execute (deletes the download) on
  any mismatch. Trust boundary, stated honestly: the binary and its checksums
  come from the same GitHub Release, so the checksum guarantees download
  integrity (corrupt/partial/wrong-platform), not independent attestation. The
  trust root is this GitHub repo - the same root the `/plugin marketplace add`
  install already trusts.

- **Offline reuse.** A verified binary for the plugin's version is cached and
  reused with no network; the script re-verifies it against a stored checksum on
  each cache hit and refetches only when the version changes.

---

## Repository layout

```
lasso-shopify-tools/
  .claude-plugin/marketplace.json      # marketplace: lasso-shopify-tools -> ./plugin
  reducer/                             # Go module (Part A): the CSV reducer
  plugin/
    .claude-plugin/plugin.json         # name: lasso-shopify-payout, version (lockstep)
    .mcp.json                          # hosted Lasso remote MCP (http + OAuth)
    commands/lasso-shopify-payout-upload.md
    skills/payout-upload/SKILL.md
    scripts/fetch-reducer.sh           # fetch-on-first-run (POSIX / Git Bash)
    scripts/fetch-reducer.ps1          # fetch-on-first-run (native Windows)
  .github/workflows/ci.yml             # gofmt + vet + test + manifest validation
  .github/workflows/release.yml        # tag v* -> tests -> lockstep -> goreleaser
  .goreleaser.yaml
```

## Development

```sh
cd reducer
gofmt -l .        # prints nothing when formatted
go vet ./...
go test ./...

# validate the manifests parse
jq empty plugin/.claude-plugin/plugin.json plugin/.mcp.json .claude-plugin/marketplace.json
```

Cut a release by bumping `plugin/.claude-plugin/plugin.json` `version`, then
pushing the matching tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```
