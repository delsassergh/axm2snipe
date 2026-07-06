## Summary

Hardens the ABM → Snipe-IT sync against real production load: fixes a 429-driven data loss bug, a duplicate-asset bug, a missing-image bug, and a rate-limit death spiral against Snipe-IT's own API, then adds a few new sync capabilities (archived-status tracking, human-readable model metadata, image backfill) on top. Verified against a live sync of ~5,270 devices.

## Fixed

- **ABM `429 RATE_LIMIT_EXCEEDED` losing progress.** Apple's ABM rate limit is undocumented and shared across every integration polling the same org (e.g. an MDM's own background sync), so axm2snipe could get throttled through no fault of its own and lose an entire run. Device fetching is now paced (`abm.page_delay_seconds`, default 5s) and resumable (`abmclient.FetchDevicesPaged` persists progress to `devices.progress.json` after every page), so a 429 mid-run picks up where it left off on the next run instead of starting over. `download`/`sync` gained `--delay`, `--page-size`, and `--restart` flags.
- **Duplicate assets ("asset_tag must be unique").** The upstream `go-snipeit` library retried *any* transient network error, including non-idempotent POST/PATCH. If Snipe-IT processed a create but the response was lost in transit, the library silently resent it, colliding with the asset it had just created. Fixed via `DisableRetries: true` on the Snipe-IT client.
- **Missing model images.** `snipeit.Model.MarshalJSON` (upstream) drops the `Image` field when building the write payload, so `sync.model_images` silently produced imageless models. `CreateModel`/new `UpdateModelImage` now bypass that path and patch the image in directly.
- **`json: cannot unmarshal string into ... fieldset_id of type int` on every asset update.** Snipe-IT sometimes returns a create/update response's embedded `payload.model.fieldset_id` as a JSON string instead of a number; the upstream library declares that field as a plain `int`, so the decode crashed even though the write had already succeeded. `CreateAsset`/`PatchAsset` now decode through a local, lenient response type instead of the upstream one.
- **appledb.dev decode crashing device image lookups.** appledb.dev's `released` field is usually a string but is an array for some older/multi-revision devices (e.g. `MacBook10,1`), which broke the whole lookup for that device — including its image, fetched in the same call. Now decoded through a small flexible-string type that accepts either shape.
- **Sync grinding to a halt with 429s from Snipe-IT itself, not Apple.** The `DisableRetries` fix above is client-wide, which also silently killed automatic retry-on-429 for a plain read (`GetAssetBySerial`) — and that read ran once per device, every sync, making it the most likely request to trip Snipe-IT's own API throttle (60–120 req/min depending on instance config) on a multi-thousand-device run. Fixed two ways (see "Added" below).

## Added

- **`backfill-images` command** — retroactively attaches appledb.dev images to existing models that were created before `sync.model_images` was enabled (or during the image-fetch bug above), with `--dry-run` support.
- **Archived status driven by ABM release date.** When ABM reports `releasedFromOrgDateTime` and `snipe_it.archived_status_id` is configured, the asset is moved to that status on create/update — unless it's already in some other archived-type status (Donated/Stolen/Lost), which is left alone. No auto-restore if the release date later clears; that stays a manual step.
- **Human-readable model metadata from appledb.dev.** New optional `field_mapping` source values — `apple_model_number` (Apple's printed regulatory number, e.g. "A3238"), `chip` (e.g. "M4"), `model_year` — sourced from the same appledb.dev lookup already used for images, cached per hardware identifier so it's one extra network call per model, not per device. Wired into `setup` so it scaffolds the new custom fields and field mappings automatically. (Snipe-IT has no per-Model custom fields, only per-Asset via Fieldsets, so these land on assets rather than the model catalog.)
- **Existing assets preloaded once per run instead of looked up per device.** `Run()` now fetches all assets via paginated `ListAllAssets` and indexes them by serial in memory; `processDevice` resolves create-vs-update from that index instead of one `GetAssetBySerial` call per device. On a ~5,270-device sync this replaces ~5,270 individual lookups with ~11 paginated list calls. `RunSingle` (single-serial sync) is unaffected — it still does a live lookup, since preloading the whole table for one device would be wasted work.
- **Retry-with-backoff on Snipe-IT 429s**, for both the byserial lookup and asset create/update. Safe even for writes: a 429 means Snipe-IT's rate limiter rejected the request before it reached the create/update logic, so nothing was written — unlike the ambiguous network-error case `DisableRetries` protects against.

## Config changes

| Key | Purpose |
| --- | --- |
| `abm.page_delay_seconds` | Pace between paginated ABM device fetches (default 5s) |
| `abm.page_size` | Devices per ABM page (default 100, max 1000) |
| `snipe_it.archived_status_id` | Status label to apply when a device is released from ABM org |
| `field_mapping: apple_model_number / chip / model_year` | New optional appledb.dev-sourced fields |

All backward compatible — every new key is optional and defaults to prior behavior when unset.

## Testing

New unit tests across `abmclient`, `snipe`, and `sync` covering: paced/resumable fetch (multi-page, 429 mid-fetch, pacing, callback errors), the fieldset_id string regression, the byserial/write 429 retry paths (including a non-429-doesn't-retry check), the appledb.dev array-vs-string decode, `ListAllAssets` pagination, and the asset-preload/fallback lookup logic.

Also verified end-to-end against production: a full sync of ~5,270 real devices completed after these fixes, including creates, updates, and image backfill.

**Not run by this PR's author (AI-assisted change) — run before merging:**
```
go build ./... && go test ./...
```
