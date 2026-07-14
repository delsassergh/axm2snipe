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
./axm2snipe import-released --csv All_ABM_Assets.csv -v  # historical release bootstrap
./axm2snipe test                  # test API connections
./axm2snipe setup -v              # create custom fields in Snipe-IT
./axm2snipe backfill-images -v    # attach appledb.dev images to existing models missing one
```

## Project Structure

- `main.go` — Entry point, sets version, calls `cmd.Execute()`
- `cmd/` — Cobra subcommands (sync, download, import-released, setup, test, backfill-images, access-token, request)
- `config/config.go` — YAML config loading, validation, env var overrides
- `abmclient/client.go` — Apple Business Manager API client (thin wrapper around upstream `abm` library)
- `abmclient/paged.go` — paced, resumable `orgDevices` pagination (`FetchDevicesPaged`), bypassing the upstream library's fixed internal rate limiter so callers can control pacing (see Gotchas below)
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
- **Archived status driven by ABM release date**: when `attrs.ReleasedFromOrgDateTime` is set and `snipe_it.archived_status_id` is configured, `createAsset`/`updateAsset` move the asset to that status label. On update, this is skipped if the asset's current status is already some other archived-type status (`existing.StatusLabel.StatusType == "archived"`) — e.g. Donated/Stolen/Lost — so a generic "Archived" doesn't clobber a more specific manual classification. There is deliberately no auto-restore: if the release date later clears, axm2snipe leaves the status as-is rather than guessing it should go back to the default status. `diffAsset` didn't compare `StatusLabel` at all before this — it's the only path that can produce a status change on update, so normal syncs still never touch a status someone set manually in Snipe-IT.
- **Released flag**: the `is_released` field-mapping source emits `true` when `ReleasedFromOrgDateTime` is non-zero and `false` otherwise. `setup` scaffolds it as the filterable BOOLEAN listbox field `AXM: Released?` alongside the `AXM: Released from Org` date.
- **warranty_months calculated from purchase date**: `warranty_months = purchase_date → applecare_end` so Snipe-IT's auto-calculated "Warranty Expires" matches the actual coverage end date.
- **Paced, resumable device fetch**: `sync.fetchAllDevicesPaced` (used by both `download --devices` and non-cached `sync`) fetches `orgDevices` via `abmclient.FetchDevicesPaged` instead of the upstream library's `FetchAllOrgDevices`, persisting progress to `<cache_dir>/devices.progress.json` after every page. If Apple returns a 429 mid-fetch, the next run resumes from the last successful page instead of re-fetching everything (or starts fresh with `download --restart`). This exists because Apple's ABM rate limit is undocumented and appears to be **shared across every integration polling the same organization** (e.g. an MDM's own ABM sync) — see `abm.page_delay_seconds` / `abm.page_size` in settings.yaml.
- **Historical released-device bootstrap**: Apple's bulk endpoint excludes released devices and its audit API can expose less history than requested. `import-released --csv <Apple device export>` extracts released serials from the export, fetches canonical records through the working single-device endpoint, checkpoints every 10 successes, and writes `<cache_dir>/released-devices.json`. A permanent Apple 404 falls back to the CSV's available model, purchase, network, and release-date attributes; transient failures still stop the chained workflow so they can be retried. Every later download merges the persistent file before audit recovery, so older releases cannot age out of the cache. The command is idempotent and safe to rerun.
- **Incremental AppleCare fetch by default**: `download --applecare` (and the AppleCare phase of a plain `download`) only fetches coverage for devices missing from the existing `applecare.json` — see `sync.filterDevicesNeedingAppleCare`. This exists because AppleCare has no bulk endpoint (one API call per device) and the upstream `abm` library self-throttles to ~20 req/min regardless of worker count (see the Gotchas entry below), so a full re-fetch across a large fleet takes hours even though AppleCare coverage essentially never changes once a device is purchased. `download --applecare-full` forces a complete re-fetch of every device — run this periodically (e.g. weekly) rather than never, since `AXM: AppleCare Status` can still transition Active → Expired over time for a device the incremental default would otherwise never look at again. Recommended cadence: nightly `download --devices && download --applecare && sync --use-cache` (fast — AppleCare phase only touches genuinely new devices), plus a weekly `download --applecare-full` to catch status changes on existing devices.
- **setup scaffolds supplier_mapping**: The `setup` command connects to ABM, fetches MDM server names (used as listbox options for the Assigned MDM Server field) and all purchase sources. It writes a `supplier_mapping` scaffold to the config with TODO entries for each purchase source, so you can fill in Snipe-IT supplier IDs.
- **Per-family category IDs**: `snipe_it.computer_category_id` is used for Mac models, `snipe_it.mobile_category_id` for iPhone/iPad/Watch/Vision. Falls back to `snipe_it.category_id`.
- **Model images from appledb.dev**: When `sync.model_images: true`, `ensureModel()` fetches a PNG from appledb.dev via `Engine.appleDBInfoFor`/`appleDBImageDataURI` using the hardware identifier (e.g. "Mac16,10") and attaches it as a base64 data URI. Failures are silently skipped. Only applies to newly created models.
- **Apple regulatory model / chip / model year (also from appledb.dev)**: The same appledb.dev lookup used for model images also exposes the printed regulatory model number (e.g. "A3238" — distinct from `part_number`'s SKU and `product_type`'s hardware identifier like "Mac16,10"), chip/SoC name (e.g. "M4"), and release year. These are opt-in: they're only fetched if `apple_model_number`/`chip`/`model_year` appear as `field_mapping` source values (mapped to asset-level custom fields — Snipe-IT has no per-Model custom fields, only per-Asset via Fieldsets). `Engine.appleDBCache` caches lookups per `product_type` for the life of a run so devices sharing a hardware identifier only trigger one appledb.dev call.
- **Serial is always the asset tag**: `createAsset()` forces `asset_tag = serial` after `applyFieldMapping()`, so a `field_mapping` entry of `asset_tag: imei` cannot override it.
- **Exact serial matching**: `GetAssetBySerial` post-filters Snipe-IT's `/byserial` results (which do substring matching) to exact case-insensitive matches, preventing wrong assets from being updated.
- **Existing assets preloaded once per run**: `Run()` calls `loadAssets()` (alongside `loadModels`/`loadSuppliers`) to fetch every asset via paginated `ListAllAssets` and index it by serial in `Engine.assetsBySerial`, so `processDevice` resolves create-vs-update from memory via `lookupExistingAsset()` instead of calling `GetAssetBySerial` once per device. Snipe-IT has no bulk write endpoint, so per-device create/update calls are unavoidable, but the old per-device *lookup* made a full sync's request volume dominated by reads that a handful of list calls can replace — see the API throttle gotcha below. `RunSingle` (single-serial `sync --serial`) skips `loadAssets` and `lookupExistingAsset` falls back to the live `GetAssetBySerial` call, since preloading the whole asset table to look up one device would be wasted work.
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
- ABM `fields[orgDevices]` is a JSON:API sparse fieldset — it filters attributes to only the listed fields, not adds to them. `sync.orgDeviceFields` explicitly requests every field the sync engine reads, plus `releasedFromOrgDateTime`. Apple documents that `releasedFromOrgDateTime` is only supported for single-device queries: released devices are completely absent from `/v1/orgDevices`, regardless of fields requested or a fresh `--restart`. `/v1/orgDevices/{id}` does return released devices and their release date. There is no dependable `RELEASED` status value; use the non-null release date.
- **Recent released devices are recovered via the audit log; older ones require a CSV seed.** `sync.recoverReleasedDevices` queries `GET /v1/auditEvents?filter[type]=DEVICE_REMOVED_FROM_ORG`, then fetches each discovered serial through the single-device endpoint. Apple does not document audit retention and live tenants can return less history than the requested `release_lookback_days`. Run `import-released --csv <Apple device export>` once for the historical gap. Both sources are persisted in `released-devices.json`, and normal downloads merge that file before querying new audit events.
- If a stale `devices.progress.json` exists from a run before the `fields[orgDevices]` fix, resuming it continues using the old (fieldless) request URL for whatever pages are left — use `download --restart` to force a clean pull with the new fields.
- The `warranty_months` field is auto-calculated and is the only non-configurable field mapping.
- Apple does not publicly document the ABM API rate limit. The upstream `abm` library self-throttles to ~20 req/min (`abmclient.NewClient`'s underlying transport), but that only paces axm2snipe's *own* requests — it has no visibility into other tools (e.g. an MDM's own background ABM sync) hitting the same organization, which can still trigger 429s even when axm2snipe is well-behaved. `abmclient.FetchDevicesPaged` (used for the device list) works around this with caller-controlled pacing (`abm.page_delay_seconds`) and resumable pagination, but AppleCare coverage fetches (`sync.fetchAppleCareParallel`, one API call per device, no bulk endpoint exists) still go through the upstream library's fixed limiter and are not resumable — a 429 there loses progress since the last full run's cache write.
- The upstream `go-snipeit` library retries ANY transient network error by default (not just safe/idempotent requests) — `snipe.NewClient` now passes `DisableRetries: true` to avoid it silently double-submitting a create when Snipe-IT processes the write but the response is lost in transit (manifests as a spurious "asset tag must be unique" error for an asset that was actually created successfully by the first attempt).
- `snipeit.Model.MarshalJSON` (upstream `go-snipeit` library) flattens Category/Manufacturer to `category_id`/`manufacturer_id` for the write API but does not carry the `Image` field through — a plain `Models.CreateContext` call silently drops any `sync.model_images`-fetched image with no error. `snipe.Client.CreateModel` works around this by re-marshaling through the same MarshalJSON to get the correct flattened payload, then patching `image` back in and sending the request itself via the exported `NewRequest`/`DoContext` helpers.
- `BuildDeviceServerMap` caps each MDM server's device-linkage fetch at 1000 (`GetMDMServerDeviceLinkagesOptions{Limit: 1000}`) and only logs a warning if a server has more; it does not paginate further, so devices beyond the first 1000 linked to a single MDM server will have an empty `AssignedServer`, which can make `mdm_only` filtering incorrectly skip them.
- Snipe-IT sometimes returns an asset create/update response's embedded `payload.model.fieldset_id` as a JSON string instead of a number. The upstream `go-snipeit` library declares `Model.FieldsetID` as a plain `int` (unlike `EOL`/`WarrantyMonths`, which use its own `FlexInt` for exactly this kind of inconsistency), so `Assets.CreateContext`/`UpdateContext`/`PatchContext` fail to decode the response with `json: cannot unmarshal string into Go struct field ...fieldset_id of type int` — even though Snipe-IT applied the write successfully. `snipe.Client.CreateAsset`/`PatchAsset` work around this the same way `CreateModel`/`UpdateModelImage` work around the dropped `Image` field: bypass the library's typed `AssetsService` methods and decode the response into a local `assetWriteResponse` struct that only reads `payload.id`, since axm2snipe never uses the returned asset for anything else.
- `snipe.NewClient`'s `DisableRetries: true` (see above) is client-wide, and the upstream library's `RequestOptions.DisableRetries` can only force retries *off* per-request, never re-enable them once disabled at the client level. That silently killed the library's automatic retry-on-429 for reads too. `snipe.retryOn429` (used by both `GetAssetBySerial` and `createOrUpdateAsset`, i.e. `CreateAsset`/`PatchAsset`) implements its own retry-with-backoff for 429s (honoring `Retry-After` when present) instead. This is safe even for writes: a 429 means Snipe-IT's rate-limit middleware rejected the request before it reached the create/update logic, so nothing was written — unlike a generic network error, where the original request may already have been processed and only the response was lost (the actual scenario `DisableRetries` protects against).
- Snipe-IT's documented API throttle is 120 req/min by default (configurable via `.env`'s `API_THROTTLE_PER_MINUTE`), but on some installs a Laravel framework default effectively caps it at 60 req/min regardless of that setting. Either way, thousands of individual per-device `GetAssetBySerial` calls on a full sync (axm2snipe's old behavior) is an easy way to exceed it — see "Existing assets preloaded once per run" above for the fix, and `snipe.retryOn429` above for the fallback when it still happens (e.g. on the write calls that remain unavoidable, or for `RunSingle`).
- appledb.dev's per-device JSON fields aren't consistently typed: `released` is usually a string but is an array for at least some devices (e.g. `MacBook10,1`). `fetchAppleDBInfo` decodes `released`/`soc` through `appleDBFlexString`, a small `UnmarshalJSON` shim that accepts either shape (taking the first array element), so one oddly-typed field doesn't fail the whole appledb.dev lookup — which would otherwise also silently kill that device's model image, since both come from the same lookup.
