# CLAUDE.md

## Project Overview

axm2snipe syncs Apple Business Manager (ABM) / Apple School Manager (ASM) devices into Snipe-IT asset management. The "X" in axm2snipe represents both ABM and ASM. Written in Go.

## Build & Run

```bash
go build -o axm2snipe .
./axm2snipe sync --dry-run -v     # safe test
./axm2snipe sync -v               # real sync
./axm2snipe sync --serial SERIAL  # single device
./axm2snipe download -v           # cache ABM data locally
./axm2snipe test                  # test API connections
./axm2snipe setup -v              # create custom fields in Snipe-IT
```

## Project Structure

- `main.go` — Entry point, sets version, calls `cmd.Execute()`
- `cmd/` — Cobra subcommands (sync, download, setup, test, access-token, request)
- `config/config.go` — YAML config loading, validation, env var overrides
- `abmclient/client.go` — Apple Business Manager API client (thin wrapper around upstream `abm` library)
- `snipe/client.go` — Snipe-IT API client (thin wrapper around upstream `go-snipeit` library with dry-run enforcement)
- `sync/sync.go` — Core sync engine (model/supplier resolution, field mapping, create/update logic)
- `notify/` — Slack webhook notifications
- `settings.example.yaml` — Example config with all options documented

## Key Design Decisions

- **No hardcoded custom fields**: All Snipe-IT custom field mappings are in `settings.yaml` under `sync.field_mapping`. The left side is the Snipe-IT DB column name (`_snipeit_*_N`), the right side is the ABM source value name.
- **Dry-run is enforced at wrapper level**: When `DryRun` is true on the `snipe.Client` wrapper, write methods return `ErrDryRun` before calling the upstream library.
- **Update-only mode**: When `update_only: true`, assets not found in Snipe-IT are skipped. No new assets, models, or suppliers are created.
- **Colors and statuses are title-cased**: ABM returns uppercase values (SILVER, ACTIVE, Paid_up_front). `titleCase()` converts underscores to spaces and title-cases (Silver, Active, Paid Up Front).
- **CDW order numbers are cleaned**: "CDW/1CJ6QLW/002" → "1CJ6QLW" via `cleanOrderNumber()`.
- **MAC addresses are auto-formatted**: Raw hex from ABM (e.g. "2CCA164BD29D") is converted to colon-separated format ("2C:CA:16:4B:D2:9D") via `formatMAC()`.
- **Model matching priority**: `ensureModel()` matches against Snipe-IT models in order: ProductType (e.g. "Mac16,10") → DeviceModel (e.g. "Mac mini (2024)") → PartNumber (e.g. "MW0Y3LL/A"). ProductType is checked first because existing Snipe-IT models populated by MDM tools like Jamf use hardware identifiers as model numbers.
- **Models indexed by name AND number**: `loadModels()` indexes Snipe-IT models by both `Name` and `ModelNumber` for flexible matching.
- **Suppliers auto-created**: ABM's `PurchaseSourceType` is matched against existing Snipe-IT suppliers (case-insensitive).
- **Snipe-IT validation errors detected**: Snipe-IT returns HTTP 200 with `{"status":"error"}` for validation failures. The upstream go-snipeit library and our wrapper both check for this.
- **warranty_months calculated from purchase date**: `warranty_months = purchase_date → applecare_end` so Snipe-IT's auto-calculated "Warranty Expires" matches the actual coverage end date.
- **setup scaffolds supplier_mapping**: The `setup` command connects to ABM, fetches MDM server names (used as listbox options for the Assigned MDM Server field) and all purchase sources. It writes a `supplier_mapping` scaffold to the config with TODO entries for each purchase source, so you can fill in Snipe-IT supplier IDs.
- **Per-family category IDs**: `snipe_it.computer_category_id` is used for Mac models, `snipe_it.mobile_category_id` for iPhone/iPad/Watch/Vision. Falls back to `snipe_it.category_id`.
- **Model images from appledb.dev**: When `sync.model_images: true`, `ensureModel()` calls `fetchModelImage(ctx, productType)` using the hardware identifier (e.g. "Mac16,10") to fetch a PNG from appledb.dev and attach it as a base64 data URI. Failures are silently skipped. Only applies to newly created models.
- **Serial is always the asset tag**: `createAsset()` forces `asset_tag = serial` after `applyFieldMapping()`, so a `field_mapping` entry of `asset_tag: imei` cannot override it.
- **Exact serial matching**: `GetAssetBySerial` post-filters Snipe-IT's `/byserial` results (which do substring matching) to exact case-insensitive matches, preventing wrong assets from being updated.
- **Invalid field retry**: `PatchAsset` retries once when Snipe-IT rejects custom fields, with per-field remediation. Fields rejected with "not available on this Asset Model's fieldset" are **stripped** from the retry payload. Fields rejected with "is invalid." (value not in the allowed options) are sent with an **empty value** on retry to clear the stored bad value — Snipe-IT re-validates stored custom-field values on every PATCH against the current field definition, so stripping alone would leave the stored bad value intact and the next PATCH would fail with the same error.

## Testing

Unit tests live in `abmclient/`, `config/`, `snipe/`, and `sync/`. Run with `go test ./...`.
Use `sync --dry-run -v` to verify end-to-end behavior without making changes.

## Documentation Rules

- **Always update README.md** when adding new config options, CLI flags, or user-facing features:
  - New `sync.*` or `snipe_it.*` config keys → add a row to the **Config Options** table
  - New `sync` CLI flags → add a row to the **Sync Flags** table
  - New features → add a bullet to the **Features** list
  - New field mapping source values → add a row to the appropriate field mapping table
  - Changed field values (e.g. radio button options) → update the **Recommended Field Types** table
- **Always update settings.example.yaml** when adding new config options:
  - Add a commented-out entry under the appropriate section with a clear description
  - Keep field type hints and valid values up to date (e.g. radio button options in the `field_mapping` comments)

## Gotchas

- ABM `deviceModel` returns marketing names like "Mac mini (2024)", NOT hardware identifiers like "Mac16,10" that Jamf uses. However, ABM `productType` does return the hardware identifier, and `ensureModel()` checks it first.
- Snipe-IT returns HTTP 200 with `{"status":"error","messages":{...}}` for validation failures — not HTTP 4xx.
- Snipe-IT radio/listbox fields reject values not in their predefined options — the entire update fails silently.
- Snipe-IT MAC format fields require colon-separated MACs (e.g. "2C:CA:16:4B:D2:9D").
- Snipe-IT custom field DB column names include an auto-incremented ID suffix (e.g. `_snipeit_color_7`). These are instance-specific.
- ABM `fields[orgDevices]` is a JSON:API sparse fieldset — it filters attributes to only the listed fields, not adds to them.
- The `warranty_months` field is auto-calculated and is the only non-configurable field mapping.
