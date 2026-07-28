---
description: Process your Shopify Partner earnings file on your own computer, preview the partner revenue it found, and upload it to Lasso once you approve.
argument-hint: <path-to-earnings.csv>
---

Run the **payout-upload** skill to process the Shopify Partner "Earnings" CSV at:

`$ARGUMENTS`

Follow the skill's procedure exactly and without shortcuts. In particular:

- The multi-GB CSV is read ONLY by the local `reducer` binary. Never open it,
  read it into context, or send its rows to any tool. Only the reduced
  `aggregate.jsonl` rows travel to Lasso.
- Enforce the guardrail: if `account_shopify_store_map` reports
  `revenueModeSignal.settledUploadSafe = false`, warn the user, cite the
  "Shopify API revenue sync" prerequisite in the README, and REFUSE every `commit`
  call for the rest of the session (reduce, preview, and `dry_run` are still
  allowed).
- Never issue a `commit` without explicit user approval given after the preview.

If no path was provided in `$ARGUMENTS`, ask the user for the absolute path to
their Shopify Partner earnings CSV before doing anything else.
