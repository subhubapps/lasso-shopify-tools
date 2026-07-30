# lasso-shopify-tools

Turn your Shopify Partner earnings file into partner revenue in Lasso.

You download the "Earnings" file from your Shopify Partner account, point this
tool at it, review a preview of what would be uploaded, and approve it. The
earnings file is processed on your own computer and is never uploaded anywhere.
Only the partner revenue rows that Lasso needs are sent.

You get two pieces, released together so they always match:

- **A small program that runs on your computer.** It reads the earnings file,
  works out how much revenue belongs to each partner for each month, and writes
  a short summary. The earnings file itself stays on your machine.
- **A Claude Code plugin** (`lasso-shopify-payout`) that walks you through the
  whole thing in one command: it looks up your partner and store list, confirms
  the account is ready, builds the summary, shows you the totals, waits for your
  approval, uploads, and then hands you a short list of anything it could not
  match to a partner. The connection to Lasso comes with the plugin, so there is
  nothing extra to set up.

---

## Before your first upload: turn off automatic Shopify revenue sync

**If Lasso is already pulling Shopify revenue for this account automatically,
turn that off before you upload for the first time.** The automatic sync and
this upload describe the same money, so leaving both on would count your revenue
twice. It is a one-time change per account.

In Lasso this setting is called **Shopify API revenue sync**, under Integrations
then Shopify then Configure Sync Settings. That is the name to look for.

Three separate checks protect you here:

1. Lasso refuses the upload while automatic Shopify revenue sync is still on.
2. The preview says plainly, and prominently, when the account is not ready.
3. The plugin blocks every upload for the rest of the session when the account
   is not ready. You can still build the summary and look at the preview, so you
   can rehearse the run safely.

If your upload gets refused, come back to this section: it means automatic
Shopify revenue sync has not been turned off for the account yet.

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

3. Connect to Lasso. This signs you in once, in your browser:

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

The first run downloads a small helper program and checks it is genuine before
using it. Later runs reuse it and work offline.

## Your earnings file stays on your computer

The earnings file can be several gigabytes, and it never leaves your machine.
The helper program reads it locally and works out, for each partner and each
month, how much revenue belongs to them. Only that short summary is sent to
Lasso, and only after you have seen the totals and approved them.

---

## Technical reference (for developers)

Everything below this line is implementation detail. You do not need it to run
an upload.

### What happens in a run

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

### Your data stays on your machine

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

### Hosted MCP endpoint (prod vs pre-prod)

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

### Distribution, releases, and the binary trust boundary

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

### Repository layout

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

### Development

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
