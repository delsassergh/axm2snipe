// Package sync implements the core synchronization logic between
// Apple Business Manager and Snipe-IT.
package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CampusTech/abm"
	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"

	"github.com/CampusTech/axm2snipe/abmclient"
	"github.com/CampusTech/axm2snipe/config"
	"github.com/CampusTech/axm2snipe/notify"
	"github.com/CampusTech/axm2snipe/snipe"
)

var log = logrus.New()

// SetLogLevel sets the logger level.
func SetLogLevel(level logrus.Level) {
	log.SetLevel(level)
}

// SetLogFormatter sets the logger formatter.
func SetLogFormatter(formatter logrus.Formatter) {
	log.SetFormatter(formatter)
}

// SetLogOutput sets the logger output.
func SetLogOutput(output io.Writer) {
	log.SetOutput(output)
}

// Stats tracks sync operation counts.
type Stats struct {
	Total    int
	Created  int
	Updated  int
	Skipped  int
	Errors   int
	ModelNew int
}

// ABMCache holds cached ABM device and AppleCare data loaded from the cache directory.
type ABMCache struct {
	Devices   []abmclient.Device
	AppleCare map[string]*abmclient.CoverageResult // device ID -> coverage
}

// Engine performs the sync between ABM and Snipe-IT.
type Engine struct {
	abm          *abmclient.Client
	snipe        *snipe.Client
	cfg          *config.Config
	notifier     *notify.Notifier
	models       map[string]int // model identifier -> snipe model ID
	suppliers    map[string]int // supplier name (lowercased) -> snipe supplier ID
	stats        Stats
	cache        *ABMCache // populated when using --use-cache
	ShowProgress bool      // show progress bars during download

	// AppleCareFullRefresh forces FetchAndSaveAppleCare to re-fetch coverage
	// for every device instead of skipping ones already present in
	// applecare.json. See FetchAndSaveAppleCare for why the default
	// (incremental) mode exists and when a full refresh is still needed.
	AppleCareFullRefresh bool

	// appleDBCache caches appledb.dev metadata lookups by hardware identifier
	// (e.g. "Mac16,10") so devices/models sharing an identifier only trigger
	// one network call per run. A nil value for a given key means the lookup
	// was attempted and failed — this is cached too, so a bad/unknown
	// identifier isn't retried on every device.
	appleDBCache map[string]*appleDBDeviceInfo

	// assetsBySerial indexes every existing Snipe-IT asset by lowercased
	// serial, populated once by loadAssets. When non-nil, processDevice uses
	// it instead of calling snipe.Client.GetAssetBySerial per device — see
	// loadAssets for why. Multiple entries under one key mean Snipe-IT has
	// duplicate assets for that serial, mirroring what a live lookup would
	// find.
	assetsBySerial map[string][]snipeit.Asset
}

// NewEngine creates a new sync engine.
func NewEngine(abmClient *abmclient.Client, snipeClient *snipe.Client, cfg *config.Config) *Engine {
	var n *notify.Notifier
	if cfg.Slack.Enabled {
		n = notify.NewNotifier(cfg.Slack.WebhookURL, cfg.SnipeIT.URL)
	}
	return &Engine{
		abm:       abmClient,
		snipe:     snipeClient,
		cfg:       cfg,
		notifier:  n,
		models:    make(map[string]int),
		suppliers: make(map[string]int),
	}
}

// NewDownloadEngine creates a lightweight engine for downloading ABM data
// without needing a Snipe-IT client.
func NewDownloadEngine(abmClient *abmclient.Client, cfg *config.Config) *Engine {
	return &Engine{
		abm: abmClient,
		cfg: cfg,
	}
}

// NewSnipeOnlyEngine creates a lightweight engine for Snipe-IT-only
// operations (e.g. BackfillModelImages) that don't need an ABM client.
func NewSnipeOnlyEngine(snipeClient *snipe.Client, cfg *config.Config) *Engine {
	return &Engine{
		snipe: snipeClient,
		cfg:   cfg,
	}
}

// CacheDir returns the configured cache directory, defaulting to ".cache".
func (e *Engine) CacheDir() string {
	if e.cfg.Sync.CacheDir != "" {
		return e.cfg.Sync.CacheDir
	}
	return ".cache"
}

// FetchAndSaveCache fetches all devices and AppleCare coverage from ABM
// and writes them to the cache directory as individual JSON files.
// Each section is saved immediately after fetching so that partial data
// is preserved if a later stage fails or is interrupted.
func (e *Engine) FetchAndSaveCache(ctx context.Context) error {
	devices, err := e.FetchAndSaveDevices(ctx)
	if err != nil {
		return err
	}
	return e.FetchAndSaveAppleCare(ctx, devices)
}

// FetchAndSaveDevices fetches devices from ABM, applies configured filters,
// writes devices.json, and returns the device list for further use.
func (e *Engine) FetchAndSaveDevices(ctx context.Context) ([]abmclient.Device, error) {
	cacheDir := e.CacheDir()

	log.Info("Fetching all devices from ABM...")
	devices, err := e.fetchAllDevicesPaced(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching ABM devices: %w", err)
	}
	log.Infof("Fetched %d devices from Apple Business Manager", len(devices))

	// Filter by product family if configured
	devices = e.filterByProductFamily(devices)

	// Filter out devices not assigned to an MDM server if configured
	if e.cfg.Sync.MDMOnly && e.cfg.Sync.MDMOnlyCache {
		var filtered []abmclient.Device
		for _, d := range devices {
			if d.AssignedServer != "" {
				filtered = append(filtered, d)
			}
		}
		log.Infof("Filtered to %d devices assigned to MDM (from %d total)", len(filtered), len(devices))
		devices = filtered
	} else if e.cfg.Sync.MDMOnlyCache && !e.cfg.Sync.MDMOnly {
		log.Warn("sync.mdm_only_cache is enabled but sync.mdm_only is false; cache filtering will be skipped")
	}

	// Normalize to empty slice so callers can distinguish "no devices" from
	// the nil sentinel used by FetchAndSaveAppleCare to mean "load from cache".
	if devices == nil {
		devices = []abmclient.Device{}
	}
	if err := writeJSON(cacheDir, "devices.json", devices); err != nil {
		return nil, fmt.Errorf("writing devices cache: %w", err)
	}
	log.Infof("Saved %d devices to %s/devices.json", len(devices), cacheDir)
	return devices, nil
}

// devicesProgressFile stores org devices already fetched from a paginated
// orgDevices pull, plus the URL of the next page, so an interrupted fetch
// (e.g. a 429 partway through) can resume without re-requesting pages already
// collected.
const devicesProgressFile = "devices.progress.json"

// devicesProgress is the on-disk resume state written by fetchOrgDevicesPaced.
type devicesProgress struct {
	Devices []abm.OrgDevice `json:"devices"`
	NextURL string          `json:"next_url"`
}

// fetchAllDevicesPaced fetches every ABM device with its assigned MDM server
// name resolved, using a paced pagination strategy (see fetchOrgDevicesPaced)
// instead of the upstream library's all-at-once FetchAllOrgDevices. Used by
// both FetchAndSaveDevices (download) and fetchABMDevices (sync without
// --use-cache) so both paths get the same pacing and resume behavior.
func (e *Engine) fetchAllDevicesPaced(ctx context.Context) ([]abmclient.Device, error) {
	log.Info("Building device-to-MDM-server map...")
	serverMap, smErr := e.abm.BuildDeviceServerMap(ctx)
	if smErr != nil {
		// Non-fatal: continue without server names, but warn since mdm_only
		// filtering will treat all devices as unassigned if this fails.
		log.WithError(smErr).Warn("Could not build device-server map; AssignedServer will be empty for all devices (mdm_only filtering may incorrectly skip managed devices)")
		serverMap = make(map[string]string)
	}

	rawDevices, err := e.fetchOrgDevicesPaced(ctx)
	if err != nil {
		return nil, err
	}

	devices := make([]abmclient.Device, len(rawDevices))
	knownSerials := make(map[string]bool, len(rawDevices))
	for i, od := range rawDevices {
		devices[i] = abmclient.Device{OrgDevice: od}
		if name, ok := serverMap[od.ID]; ok {
			devices[i].AssignedServer = name
		}
		knownSerials[strings.ToUpper(deviceSerial(devices[i]))] = true
	}

	// Released devices disappear from Apple's bulk endpoint. Preserve all
	// previously recovered records before querying the limited audit history,
	// preferring the fresh bulk record if a device was later re-added.
	persistent, perr := e.loadPersistentReleasedDevices()
	if perr != nil {
		log.WithError(perr).Warn("Could not load persistent released-device cache")
	} else {
		devices = mergeDevicesBySerial(devices, persistent)
		for _, d := range persistent {
			knownSerials[normalizeSerial(deviceSerial(d))] = true
		}
	}

	recovered := e.recoverReleasedDevices(ctx, knownSerials)
	devices = mergeDevicesBySerial(devices, recovered)
	if len(persistent) > 0 || len(recovered) > 0 {
		if err := e.savePersistentReleasedDevices(mergeDevicesBySerial(persistent, recovered)); err != nil {
			log.WithError(err).Warn("Could not save persistent released-device cache")
		}
	}
	return devices, nil
}

// recoverReleasedDevices backfills devices that /v1/orgDevices' bulk list
// silently excludes because they've been released from the org (confirmed:
// the list never returns a released device, or a non-null
// releasedFromOrgDateTime, regardless of fields[orgDevices] -- see
// ABMConfig.ReleaseLookbackDays). It discovers which serials were released
// via the audit events log, then fetches each one's full current data via
// the single-device endpoint, which does return released devices correctly.
// knownSerials (uppercased) is the set already present in the bulk pull, so
// a device isn't fetched twice. Failures here are non-fatal -- a device that
// can't be recovered this way is no worse off than before this feature
// existed, so a partial failure shouldn't fail the whole download/sync.
func (e *Engine) recoverReleasedDevices(ctx context.Context, knownSerials map[string]bool) []abmclient.Device {
	lookback := e.cfg.ABM.ReleaseLookback()
	log.Debugf("Querying auditEvents for devices released between %s and %s", time.Now().Add(-lookback).UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	released, err := e.abm.FetchReleasedDevices(ctx, lookback)
	if err != nil {
		log.WithError(err).Warn("Could not fetch released-device audit events; devices released from the org may be missing from this sync")
		return nil
	}
	log.Debugf("Audit log returned %d total DEVICE_REMOVED_FROM_ORG event(s) in the lookback window", len(released))

	var missing []string
	for serial := range released {
		if !knownSerials[strings.ToUpper(serial)] {
			missing = append(missing, serial)
		}
	}
	if len(missing) == 0 {
		log.Debugf("Audit log shows %d device(s) released within the lookback window, all already accounted for", len(released))
		return nil
	}
	log.Infof("Audit log shows %d released device(s) missing from the bulk device list; fetching them individually", len(missing))

	var recovered []abmclient.Device
	for _, serial := range missing {
		device, gerr := e.abm.GetDeviceInfo(ctx, serial)
		if gerr != nil {
			log.WithError(gerr).WithField("serial", serial).Warn("Could not fetch released device individually; it will be missing from this sync")
			continue
		}
		recovered = append(recovered, *device)
	}
	log.Infof("Recovered %d/%d released device(s) via individual lookups", len(recovered), len(missing))
	return recovered
}

// orgDeviceFields is the explicit fields[orgDevices] sparse fieldset we
// request from ABM. This must be passed explicitly: Apple's /v1/orgDevices
// endpoint does not return releasedFromOrgDateTime (or, seemingly, released
// devices at all -- they were completely absent from the device list, not
// just missing that one field) unless fields[orgDevices] is set and
// includes it. Every field the rest of the sync engine reads via
// abm.OrgDeviceAttributes is listed here so switching to an explicit
// fieldset can't silently drop something that used to come back by
// default. assignedServer and appleCareCoverage are deliberately excluded:
// those are relationships (linked via their own endpoints), not attributes
// -- resolved separately via BuildDeviceServerMap's MDM-server linkages and
// a dedicated per-device AppleCare fetch. Note there is no distinct
// "RELEASED" status value: a released device shows status=UNASSIGNED with
// releasedFromOrgDateTime also set, per Apple's own example response.
//
// releaserId/releaserEntityType were tried and rejected outright by the API
// ("... is not a valid field name") despite appearing in some Apple
// documentation -- do not re-add them without confirming against a live
// request first.
var orgDeviceFields = []string{
	"serialNumber", "addedToOrgDateTime", "releasedFromOrgDateTime",
	"updatedDateTime",
	"deviceModel", "productFamily", "productType", "deviceCapacity",
	"partNumber", "orderNumber", "color", "status", "orderDateTime",
	"imei", "meid", "eid", "purchaseSourceId", "purchaseSourceType",
	"wifiMacAddress", "bluetoothMacAddress", "ethernetMacAddress",
}

// fetchOrgDevicesPaced fetches all org devices via abmclient.FetchDevicesPaged,
// pacing requests per Cfg.ABM.PageDelay()/PageSizeOrDefault() and persisting
// progress to devices.progress.json after every page. If a previous run left
// progress behind (e.g. it was interrupted by a 429), this resumes from where
// it left off instead of starting over and re-spending API calls on pages
// already collected.
func (e *Engine) fetchOrgDevicesPaced(ctx context.Context) ([]abm.OrgDevice, error) {
	cacheDir := e.CacheDir()
	progressPath := filepath.Join(cacheDir, devicesProgressFile)

	var collected []abm.OrgDevice
	resume := ""
	if data, rerr := os.ReadFile(progressPath); rerr == nil {
		var p devicesProgress
		if jerr := json.Unmarshal(data, &p); jerr == nil && p.NextURL != "" {
			collected = p.Devices
			resume = p.NextURL
			log.Infof("Resuming device fetch from previous run (%d devices already collected)", len(collected))
		}
	}

	opts := abmclient.PagedFetchOptions{
		Fields:   orgDeviceFields,
		PageSize: e.cfg.ABM.PageSizeOrDefault(),
		Delay:    e.cfg.ABM.PageDelay(),
		Resume:   resume,
	}

	_, err := e.abm.FetchDevicesPaged(ctx, opts, func(page abmclient.PagedDevicesResult) error {
		collected = append(collected, page.Devices...)
		return writeJSON(cacheDir, devicesProgressFile, devicesProgress{Devices: collected, NextURL: page.NextURL})
	})
	if err != nil {
		log.WithError(err).Warnf("Device fetch interrupted after %d devices; progress saved to %s/%s — re-run to resume from where it left off", len(collected), cacheDir, devicesProgressFile)
		return nil, err
	}

	// Completed successfully — clean up the resume file.
	if rmErr := os.Remove(progressPath); rmErr != nil && !os.IsNotExist(rmErr) {
		log.WithError(rmErr).Debug("Could not remove devices progress file")
	}
	return collected, nil
}

// ResetDeviceProgress discards any saved paginated-device-fetch resume state
// (see devicesProgressFile), forcing the next fetch to start from page one
// instead of resuming. Used by `download --restart`.
func (e *Engine) ResetDeviceProgress() error {
	err := os.Remove(filepath.Join(e.CacheDir(), devicesProgressFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// loadDevicesFromCache reads only devices.json into e.cache.Devices without
// touching applecare.json. Used by FetchAndSaveAppleCare so that an
// AppleCare-only refresh does not warn about a missing applecare.json.
func (e *Engine) loadDevicesFromCache() error {
	cacheDir := e.CacheDir()
	devicesPath := filepath.Join(cacheDir, "devices.json")
	data, err := os.ReadFile(devicesPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", devicesPath, err)
	}
	var devices []abmclient.Device
	if err := json.Unmarshal(data, &devices); err != nil {
		return fmt.Errorf("parsing %s: %w", devicesPath, err)
	}
	if e.cache == nil {
		e.cache = &ABMCache{}
	}
	e.cache.Devices = devices
	return nil
}

// filterDevicesNeedingAppleCare returns the subset of devices that need an
// AppleCare API call: all of them if fullRefresh is true or existing is
// empty (nothing cached yet, e.g. first run), otherwise only devices whose
// ID isn't already a key in existing.
func filterDevicesNeedingAppleCare(devices []abmclient.Device, existing map[string]*abmclient.CoverageResult, fullRefresh bool) []abmclient.Device {
	if fullRefresh || len(existing) == 0 {
		return devices
	}
	var toFetch []abmclient.Device
	for _, d := range devices {
		if _, ok := existing[d.ID]; !ok {
			toFetch = append(toFetch, d)
		}
	}
	return toFetch
}

// FetchAndSaveAppleCare fetches AppleCare coverage for the given device list
// and writes applecare.json. If devices is nil, it loads devices from cache.
//
// By default this is incremental: any device that already has an entry in
// the existing applecare.json is skipped, and only devices missing from
// that cache (i.e. new to ABM since the last AppleCare pull) are fetched.
// This matters because there's no bulk AppleCare endpoint -- it's one API
// call per device, paced by the private abm library's own internal rate
// limiter -- so a full pull across a large fleet can take hours even though
// AppleCare coverage essentially never changes once a device is purchased.
//
// Set e.AppleCareFullRefresh to force a complete re-fetch of every device
// instead. That's still worth doing periodically (e.g. weekly) rather than
// never, since AXM: AppleCare Status can transition Active -> Expired over
// time for a device that incremental mode would otherwise never look at
// again once it has any cached entry.
func (e *Engine) FetchAndSaveAppleCare(ctx context.Context, devices []abmclient.Device) error {
	cacheDir := e.CacheDir()

	if devices == nil {
		// Load only devices.json to avoid spurious warnings about a missing
		// applecare.json (the file we're about to create).
		if err := e.loadDevicesFromCache(); err != nil {
			return fmt.Errorf("loading device cache for AppleCare refresh: %w", err)
		}
		devices = e.cache.Devices
		log.Infof("Loaded %d devices from cache for AppleCare refresh", len(devices))
	}

	existing := make(map[string]*abmclient.CoverageResult)
	if !e.AppleCareFullRefresh {
		if data, err := os.ReadFile(filepath.Join(cacheDir, "applecare.json")); err == nil {
			if jerr := json.Unmarshal(data, &existing); jerr != nil {
				log.WithError(jerr).Warn("Could not parse existing applecare.json, falling back to a full AppleCare fetch")
				existing = make(map[string]*abmclient.CoverageResult)
			}
		}
	}

	incremental := !e.AppleCareFullRefresh && len(existing) > 0
	toFetch := filterDevicesNeedingAppleCare(devices, existing, e.AppleCareFullRefresh)
	if incremental {
		log.Infof("AppleCare cache already has coverage for %d/%d devices; fetching %d new device(s)", len(existing), len(devices), len(toFetch))
	} else if e.AppleCareFullRefresh {
		log.Info("Fetching AppleCare coverage for all devices (full refresh)...")
	} else {
		log.Info("Fetching AppleCare coverage for all devices...")
	}

	appleCareMap := existing
	if len(toFetch) == 0 {
		log.Info("No new devices need AppleCare coverage; applecare.json is already up to date")
	} else {
		fetched := e.fetchAppleCareParallel(ctx, toFetch)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if incremental {
			for id, cov := range fetched {
				appleCareMap[id] = cov
			}
		} else {
			appleCareMap = fetched
		}
	}

	if err := writeJSON(cacheDir, "applecare.json", appleCareMap); err != nil {
		return fmt.Errorf("writing applecare cache: %w", err)
	}
	log.Infof("Saved %d AppleCare records to %s/applecare.json", len(appleCareMap), cacheDir)
	return nil
}

// LoadCache reads ABM cache from individual JSON files in the cache directory.
func (e *Engine) LoadCache() error {
	cacheDir := e.CacheDir()

	devicesPath := filepath.Join(cacheDir, "devices.json")
	data, err := os.ReadFile(devicesPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", devicesPath, err)
	}
	var devices []abmclient.Device
	if err := json.Unmarshal(data, &devices); err != nil {
		return fmt.Errorf("parsing %s: %w", devicesPath, err)
	}

	appleCareMap := make(map[string]*abmclient.CoverageResult)
	acPath := filepath.Join(cacheDir, "applecare.json")
	acData, err := os.ReadFile(acPath)
	if err != nil {
		log.Warnf("Could not read %s, continuing without AppleCare cache: %v", acPath, err)
	} else if err := json.Unmarshal(acData, &appleCareMap); err != nil {
		log.Warnf("Could not parse %s, continuing without AppleCare cache: %v", acPath, err)
	}

	log.Infof("Loaded cache from %s/ (%d devices, %d AppleCare records)", cacheDir, len(devices), len(appleCareMap))
	e.cache = &ABMCache{
		Devices:   devices,
		AppleCare: appleCareMap,
	}
	return nil
}

// appleCareWorkers is the number of concurrent AppleCare fetch goroutines.
const appleCareWorkers = 10

// fetchAppleCareParallel fetches AppleCare coverage for all devices concurrently
// using a bounded worker pool. Returns a map of device ID → coverage.
// Saves partial results to disk if the context is cancelled mid-way.
func (e *Engine) fetchAppleCareParallel(ctx context.Context, devices []abmclient.Device) map[string]*abmclient.CoverageResult {
	type result struct {
		deviceID string
		serial   string
		coverage *abmclient.CoverageResult
		err      error
	}

	n := len(devices)
	if n == 0 {
		return make(map[string]*abmclient.CoverageResult)
	}

	jobs := make(chan abmclient.Device, n)
	results := make(chan result, n)

	workers := appleCareWorkers
	if workers > n {
		workers = n
	}
	for range workers {
		go func() {
			for d := range jobs {
				serial := deviceSerial(d)
				if ctx.Err() != nil {
					results <- result{deviceID: d.ID, serial: serial}
					continue
				}
				ac, err := e.abm.GetAppleCareCoverage(ctx, d.ID)
				results <- result{deviceID: d.ID, serial: serial, coverage: ac, err: err}
			}
		}()
	}

	for _, d := range devices {
		jobs <- d
	}
	close(jobs)

	appleCareMap := make(map[string]*abmclient.CoverageResult)
	var bar *progressbar.ProgressBar
	if e.ShowProgress {
		bar = progressbar.NewOptions(n,
			progressbar.OptionSetDescription("  AppleCare"),
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionShowCount(),
			progressbar.OptionSetWidth(40),
			progressbar.OptionOnCompletion(func() { fmt.Fprintln(os.Stderr) }),
		)
	}
	for i := range n {
		r := <-results
		if r.err != nil {
			log.WithError(r.err).WithField("device_id", r.deviceID).Debug("Could not fetch AppleCare coverage")
		} else if r.coverage != nil {
			appleCareMap[r.deviceID] = r.coverage
		}
		if bar != nil {
			bar.Describe(fmt.Sprintf("  AppleCare for %-14s", r.serial))
			_ = bar.Add(1)
		} else if (i+1)%50 == 0 {
			log.Infof("AppleCare progress: %d/%d devices", i+1, n)
		}
	}
	if bar != nil {
		_ = bar.Finish()
	}

	if ctx.Err() != nil && len(appleCareMap) > 0 {
		cacheDir := e.CacheDir()
		if wErr := writeJSON(cacheDir, "applecare.json", appleCareMap); wErr != nil {
			log.WithError(wErr).Warn("Could not save partial AppleCare cache")
		} else {
			log.Infof("Saved partial AppleCare cache (%d/%d devices) to %s/applecare.json", len(appleCareMap), n, cacheDir)
		}
	}

	return appleCareMap
}

// writeJSON writes a value as indented JSON to a file in the given directory.
func writeJSON(dir, filename string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", filename, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// filterByProductFamily filters devices by configured product families.
func (e *Engine) filterByProductFamily(devices []abmclient.Device) []abmclient.Device {
	if len(e.cfg.Sync.ProductFamilies) == 0 {
		return devices
	}
	families := make(map[string]bool)
	for _, f := range e.cfg.Sync.ProductFamilies {
		families[strings.ToLower(f)] = true
	}
	var filtered []abmclient.Device
	for _, d := range devices {
		if d.Attributes != nil && families[strings.ToLower(string(d.Attributes.ProductFamily))] {
			filtered = append(filtered, d)
		}
	}
	log.Infof("Filtered to %d devices (from %d) by product family: %v", len(filtered), len(devices), e.cfg.Sync.ProductFamilies)
	return filtered
}

// RunSingle syncs a single device identified by serial number.
func (e *Engine) RunSingle(ctx context.Context, serial string) (*Stats, error) {
	serial = strings.ToUpper(serial)
	log.Infof("Syncing single device: %s", serial)

	if err := e.loadModels(ctx); err != nil {
		return nil, fmt.Errorf("loading snipe models: %w", err)
	}
	if err := e.loadSuppliers(ctx); err != nil {
		return nil, fmt.Errorf("loading snipe suppliers: %w", err)
	}

	// Check cache first, otherwise fetch single device directly from ABM
	var device *abmclient.Device
	if e.cache != nil {
		for _, d := range e.cache.Devices {
			if strings.EqualFold(deviceSerial(d), serial) {
				device = &d
				break
			}
		}
		if device == nil {
			return nil, fmt.Errorf("device %s not found in cache", serial)
		}
	} else {
		var err error
		device, err = e.abm.GetDevice(ctx, serial)
		if err != nil {
			return nil, fmt.Errorf("fetching device from ABM: %w", err)
		}
	}

	if err := e.processDevice(ctx, *device); err != nil {
		log.WithError(err).WithField("serial", serial).Error("Failed to process device")
		e.stats.Errors++
	}

	return &e.stats, nil
}

// Run executes the full sync process.
func (e *Engine) Run(ctx context.Context) (*Stats, error) {
	log.Info("Starting axm2snipe sync")

	// Step 1: Load existing Snipe-IT models and suppliers into cache
	if err := e.loadModels(ctx); err != nil {
		return nil, fmt.Errorf("loading snipe models: %w", err)
	}
	log.Infof("Loaded %d existing models from Snipe-IT", len(e.models))

	if err := e.loadSuppliers(ctx); err != nil {
		return nil, fmt.Errorf("loading snipe suppliers: %w", err)
	}
	log.Infof("Loaded %d existing suppliers from Snipe-IT", len(e.suppliers))

	if err := e.loadAssets(ctx); err != nil {
		return nil, fmt.Errorf("loading snipe assets: %w", err)
	}
	log.Infof("Loaded %d existing assets from Snipe-IT", len(e.assetsBySerial))

	// Step 2: Fetch all devices from ABM
	devices, err := e.fetchABMDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching ABM devices: %w", err)
	}
	log.Infof("Fetched %d devices from Apple Business Manager", len(devices))

	// Step 3: Process each device
	for i, device := range devices {
		if err := ctx.Err(); err != nil {
			return &e.stats, err
		}

		if err := e.processDevice(ctx, device); err != nil {
			log.WithError(err).WithField("serial", deviceSerial(device)).Error("Failed to process device")
			e.stats.Errors++
		}

		if (i+1)%50 == 0 {
			log.WithFields(logrus.Fields{"progress": i + 1, "total": len(devices)}).Info("Processing devices")
		}
	}

	log.Infof("Sync complete: total=%d created=%d updated=%d skipped=%d errors=%d new_models=%d",
		e.stats.Total, e.stats.Created, e.stats.Updated, e.stats.Skipped, e.stats.Errors, e.stats.ModelNew)

	return &e.stats, nil
}

// BackfillModelImages fetches and attaches appledb.dev images to existing
// Snipe-IT models that don't have one yet. Unlike ensureModel's image fetch
// (which only runs at the moment a model is newly created), this can
// retroactively fill in images for models created before sync.model_images
// was enabled, or before CreateModel's image-attach bug (see CLAUDE.md) was
// fixed. Only considers models under the configured Apple manufacturer_id
// with a model_number set (appledb.dev is keyed by hardware identifier, e.g.
// "Mac16,10", which is what ensureModel stores as model_number).
func (e *Engine) BackfillModelImages(ctx context.Context) (updated int, skipped int, err error) {
	models, err := e.snipe.ListAllModels(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("listing models: %w", err)
	}

	for _, m := range models {
		if m.Image != "" || m.ModelNumber == "" || m.Manufacturer.ID != e.cfg.SnipeIT.ManufacturerID {
			continue
		}

		logger := log.WithFields(logrus.Fields{"model": m.Name, "model_number": m.ModelNumber})

		img := fetchModelImage(ctx, m.ModelNumber)
		if img == "" {
			logger.Warn("Could not fetch an image for this model from AppleDB")
			skipped++
			continue
		}

		if _, updErr := e.snipe.UpdateModelImage(ctx, m, img); updErr != nil {
			if errors.Is(updErr, snipe.ErrDryRun) {
				logger.Info("[DRY RUN] Would attach image")
				updated++
				continue
			}
			logger.WithError(updErr).Warn("Could not update model image")
			skipped++
			continue
		}

		logger.Info("Attached image to existing model")
		updated++
	}

	return updated, skipped, nil
}

// loadModels fetches all models from Snipe-IT and builds a lookup map.
// Models are indexed by both model number and name for flexible matching.
func (e *Engine) loadModels(ctx context.Context) error {
	models, err := e.snipe.ListAllModels(ctx)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.ModelNumber != "" {
			e.models[m.ModelNumber] = m.ID
		}
		if m.Name != "" {
			e.models[m.Name] = m.ID
		}
	}
	return nil
}

// loadSuppliers fetches all suppliers from Snipe-IT and builds a lookup map.
func (e *Engine) loadSuppliers(ctx context.Context) error {
	suppliers, err := e.snipe.ListAllSuppliers(ctx)
	if err != nil {
		return err
	}
	for _, s := range suppliers {
		if s.Name != "" {
			e.suppliers[strings.ToLower(s.Name)] = s.ID
		}
	}
	return nil
}

// loadAssets fetches all existing assets from Snipe-IT and indexes them by
// serial, so Run can decide create-vs-update for every device from memory
// instead of calling snipe.Client.GetAssetBySerial once per device.
//
// Snipe-IT has no bulk create/update endpoint, so the per-device write
// calls are unavoidable — but the byserial *lookup* that used to precede
// every write is a plain paginated list under the hood, and Snipe-IT's API
// throttle (60-120 req/min depending on instance config, per Snipe-IT's own
// docs) makes thousands of individual lookups on a full sync a much bigger
// rate-limit risk than the handful of paginated list calls this replaces
// them with (500 assets per page, same page size loadModels/loadSuppliers
// already use).
func (e *Engine) loadAssets(ctx context.Context) error {
	assets, err := e.snipe.ListAllAssets(ctx)
	if err != nil {
		return err
	}
	e.assetsBySerial = make(map[string][]snipeit.Asset, len(assets))
	for _, a := range assets {
		if a.Serial == "" {
			continue
		}
		key := strings.ToLower(a.Serial)
		e.assetsBySerial[key] = append(e.assetsBySerial[key], a)
	}
	return nil
}

// lookupExistingAsset resolves a device's serial to its existing Snipe-IT
// asset(s). When loadAssets has populated the in-memory index (the normal
// Run path), it's used directly with no API call. Otherwise (e.g.
// RunSingle, which only ever processes one device and would gain nothing
// from preloading the entire asset table) this falls back to a live
// snipe.Client.GetAssetBySerial call.
func (e *Engine) lookupExistingAsset(ctx context.Context, serial string) (*snipeit.AssetsResponse, error) {
	if e.assetsBySerial != nil {
		rows := e.assetsBySerial[strings.ToLower(serial)]
		return &snipeit.AssetsResponse{Response: snipeit.Response{Total: len(rows)}, Rows: rows}, nil
	}
	return e.snipe.GetAssetBySerial(ctx, serial)
}

// ensureSupplier resolves the supplier for an ABM device and ensures it exists in Snipe-IT.
// It checks supplier_mapping for purchaseSourceId, then purchaseSourceType, then the
// resolved name. Falls back to name-based lookup and auto-creation.
func (e *Engine) ensureSupplier(ctx context.Context, attrs *abm.OrgDeviceAttributes) (int, error) {
	purchaseSource := string(attrs.PurchaseSourceType)
	if purchaseSource == "" && attrs.PurchaseSourceID == "" {
		return 0, nil
	}

	// Resolve a human-readable supplier name from purchaseSourceType
	name := purchaseSource
	if strings.EqualFold(name, "APPLE") {
		name = "Apple"
	}

	if name == "" || strings.EqualFold(name, "MANUALLY_ADDED") {
		return 0, nil
	}

	// Check supplier_mapping for direct ID match (purchaseSourceId -> Snipe-IT supplier ID)
	// then purchaseSourceType match
	if attrs.PurchaseSourceID != "" {
		if id, ok := e.cfg.Sync.SupplierMapping[attrs.PurchaseSourceID]; ok {
			return id, nil
		}
	}
	for mappedKey, supplierID := range e.cfg.Sync.SupplierMapping {
		if strings.EqualFold(mappedKey, purchaseSource) {
			return supplierID, nil
		}
	}
	// Warn regardless of whether supplier_mapping is configured — this is a
	// discovery hint so admins know what to add to their config.
	log.WithField("purchase_source_id", attrs.PurchaseSourceID).WithField("purchase_source_type", purchaseSource).Warn("Purchase source not found in supplier_mapping — falling back to name-based lookup. Add this source to supplier_mapping in your config to suppress this warning.")

	if id, ok := e.suppliers[strings.ToLower(name)]; ok {
		return id, nil
	}

	if e.cfg.Sync.UpdateOnly {
		log.WithField("supplier", name).Debug("Supplier not found in Snipe-IT (update_only mode), skipping")
		return 0, nil
	}

	if e.cfg.Sync.DryRun {
		log.WithField("supplier", name).Info("[DRY RUN] Would create supplier")
		return 0, nil
	}

	newSupplier, err := e.snipe.CreateSupplier(ctx, name)
	if err != nil {
		return 0, err
	}

	log.WithFields(logrus.Fields{
		"supplier": name,
		"snipe_id": newSupplier.ID,
	}).Info("Created new supplier in Snipe-IT")

	e.suppliers[strings.ToLower(name)] = newSupplier.ID
	return newSupplier.ID, nil
}

// fetchABMDevices retrieves all devices from ABM (or cache), with optional product family filtering.
func (e *Engine) fetchABMDevices(ctx context.Context) ([]abmclient.Device, error) {
	var allDevices []abmclient.Device

	if e.cache != nil {
		allDevices = e.cache.Devices
		log.Infof("Using %d cached devices", len(allDevices))
	} else {
		var err error
		allDevices, err = e.fetchAllDevicesPaced(ctx)
		if err != nil {
			return nil, err
		}
	}

	// Filter by product family if configured
	allDevices = e.filterByProductFamily(allDevices)

	return allDevices, nil
}

// processDevice handles a single ABM device - creating or updating it in Snipe-IT.
func (e *Engine) processDevice(ctx context.Context, device abmclient.Device) error {
	e.stats.Total++

	attrs := device.Attributes
	if attrs == nil {
		log.Debug("Skipping device with nil attributes")
		e.stats.Skipped++
		return nil
	}

	serial := attrs.SerialNumber
	if serial == "" || serial == "Not Available" {
		log.WithField("device_id", device.ID).Debug("Skipping device with no serial number")
		e.stats.Skipped++
		return nil
	}

	logger := log.WithField("serial", serial)

	// Skip devices not assigned to an MDM server if configured
	if e.cfg.Sync.MDMOnly && device.AssignedServer == "" {
		logger.Info("Skipping device not assigned to any MDM server (mdm_only mode)")
		e.stats.Skipped++
		return nil
	}

	// Look up asset in Snipe-IT by serial first to decide create vs update.
	// snipe.Client.GetAssetBySerial's own error already identifies the
	// serial, so it's returned as-is here rather than wrapped again.
	existing, err := e.lookupExistingAsset(ctx, serial)
	if err != nil {
		return err
	}

	if existing.Total == 0 && e.cfg.Sync.UpdateOnly {
		logger.Info("Skipping asset not found in Snipe-IT (update_only mode)")
		e.stats.Skipped++
		return nil
	}

	// Fetch AppleCare coverage for this device (from cache or API)
	var coverage *abmclient.CoverageResult
	if e.cache != nil && e.cache.AppleCare != nil {
		if ac, ok := e.cache.AppleCare[device.ID]; ok {
			coverage = ac
			if ac.Best != nil {
				logger.WithField("applecare_status", ac.Best.Status).Debug("Found AppleCare coverage (cached)")
			}
		}
	} else {
		ac, err := e.abm.GetAppleCareCoverage(ctx, device.ID)
		if err != nil {
			logger.WithError(err).Warn("Could not fetch AppleCare coverage, continuing without it")
		} else if ac != nil {
			coverage = ac
			if ac.Best != nil {
				logger.WithField("applecare_status", ac.Best.Status).Debug("Found AppleCare coverage")
			}
		}
	}

	// Resolve supplier from ABM data
	supplierID, err := e.ensureSupplier(ctx, attrs)
	if err != nil {
		logger.WithError(err).Warn("Could not resolve supplier, continuing without it")
	}

	switch existing.Total {
	case 0:
		// Create new asset — need to resolve model
		modelID, err := e.ensureModel(ctx, attrs)
		if err != nil {
			return fmt.Errorf("ensuring model for %s: %w", serial, err)
		}
		return e.createAsset(ctx, logger, device, modelID, supplierID, coverage)
	case 1:
		// Update existing asset — model already assigned in Snipe-IT
		return e.updateAsset(ctx, logger, device, &existing.Rows[0], supplierID, coverage)
	default:
		logger.Warnf("Multiple assets (%d) found for serial, skipping", existing.Total)
		e.stats.Skipped++
		return nil
	}
}

// ensureModel checks if the device model exists in Snipe-IT, creating it if needed.
// It tries matching by DeviceModel (marketing name), PartNumber, and ProductType
// (hardware identifier like "Mac16,10") against Snipe-IT model numbers and names.
func (e *Engine) ensureModel(ctx context.Context, attrs *abm.OrgDeviceAttributes) (int, error) {
	// Try matching ProductType (e.g. "Mac16,10") first — hardware model identifiers
	// that may already exist in Snipe-IT as model numbers from MDM tools like Jamf
	if attrs.ProductType != "" {
		if id, ok := e.models[attrs.ProductType]; ok {
			return id, nil
		}
	}

	// Try matching DeviceModel (e.g. "Mac mini (2024)") against model numbers and names
	if attrs.DeviceModel != "" {
		if id, ok := e.models[attrs.DeviceModel]; ok {
			return id, nil
		}
	}

	// Try matching PartNumber (e.g. "MW0Y3LL/A") against model numbers
	if attrs.PartNumber != "" {
		if id, ok := e.models[attrs.PartNumber]; ok {
			return id, nil
		}
	}

	if attrs.DeviceModel == "" && attrs.ProductType == "" {
		return 0, fmt.Errorf("device has no model identifier")
	}

	// Use DeviceModel as the display name, ProductType as the model number
	modelName := attrs.DeviceModel
	modelNumber := attrs.ProductType
	if modelName == "" {
		modelName = modelNumber
	}
	if modelNumber == "" {
		modelNumber = modelName
	}

	if e.cfg.Sync.UpdateOnly {
		log.WithFields(logrus.Fields{
			"model_name":   modelName,
			"model_number": modelNumber,
		}).Warn("Model not found in Snipe-IT and update_only mode is enabled, skipping")
		return 0, fmt.Errorf("model %q not found (update_only mode)", modelName)
	}

	if e.cfg.Sync.DryRun {
		log.WithFields(logrus.Fields{
			"model_name":   modelName,
			"model_number": modelNumber,
		}).Info("[DRY RUN] Would create model")
		e.stats.ModelNew++
		return 0, nil
	}

	model := snipeit.Model{
		CommonFields: snipeit.CommonFields{Name: modelName},
		ModelNumber:  modelNumber,
		Category: snipeit.Category{
			CommonFields: snipeit.CommonFields{ID: e.cfg.SnipeIT.CategoryIDForFamily(string(attrs.ProductFamily))},
		},
		Manufacturer: snipeit.Manufacturer{
			CommonFields: snipeit.CommonFields{ID: e.cfg.SnipeIT.ManufacturerID},
		},
		FieldsetID: e.cfg.SnipeIT.CustomFieldsetID,
	}

	if e.cfg.Sync.ModelImages && attrs.ProductType != "" {
		if img := appleDBImageDataURI(ctx, e.appleDBInfoFor(ctx, attrs.ProductType)); img != "" {
			model.Image = img
		}
	}

	newModel, err := e.snipe.CreateModel(ctx, model)
	if err != nil {
		return 0, err
	}

	log.WithFields(logrus.Fields{
		"model_name":   modelName,
		"model_number": modelNumber,
		"snipe_id":     newModel.ID,
	}).Info("Created new model in Snipe-IT")

	e.models[modelName] = newModel.ID
	e.models[modelNumber] = newModel.ID
	e.stats.ModelNew++
	return newModel.ID, nil
}

// createAsset creates a new asset in Snipe-IT from ABM device data.
func (e *Engine) createAsset(ctx context.Context, logger *logrus.Entry, device abmclient.Device, modelID int, supplierID int, coverage *abmclient.CoverageResult) error {
	attrs := device.Attributes

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: make(map[string]string),
		},
		Serial:   attrs.SerialNumber,
		AssetTag: attrs.SerialNumber,
		Model: snipeit.Model{
			CommonFields: snipeit.CommonFields{ID: modelID},
		},
		StatusLabel: snipeit.StatusLabel{
			CommonFields: snipeit.CommonFields{ID: e.cfg.SnipeIT.DefaultStatusID},
		},
	}

	// If ABM already shows this device as released from the org (e.g. a
	// historical device appearing for the first time), create it directly
	// into the archived status instead of the default status.
	if !attrs.ReleasedFromOrgDateTime.IsZero() {
		if e.cfg.SnipeIT.ArchivedStatusID != 0 {
			asset.StatusLabel.ID = e.cfg.SnipeIT.ArchivedStatusID
		} else {
			logger.Warn("Device was released from ABM org but snipe_it.archived_status_id is not configured; using default status instead")
		}
	}

	if e.cfg.Sync.SetName {
		name := attrs.DeviceModel
		if attrs.Color != "" {
			name = fmt.Sprintf("%s (%s)", name, titleCase(attrs.Color))
		}
		if name != "" {
			asset.Name = name
		}
	}

	if supplierID > 0 {
		asset.Supplier = snipeit.Supplier{
			CommonFields: snipeit.CommonFields{ID: supplierID},
		}
	}

	e.applyFieldMapping(ctx, &asset, device, coverage)
	applyWarrantyNotes(&asset, coverage)
	// Always use serial as asset tag regardless of field_mapping overrides.
	asset.AssetTag = attrs.SerialNumber

	if e.cfg.Sync.DryRun {
		logger.WithField("payload", asset).Info("[DRY RUN] Would create asset")
		e.stats.Created++
		return nil
	}

	if _, err := e.snipe.CreateAsset(ctx, asset); err != nil {
		return err
	}

	logger.Info("Created asset in Snipe-IT")
	e.stats.Created++

	// Send Slack notification for new asset
	if e.notifier != nil {
		var best *abmclient.AppleCareCoverage
		if coverage != nil {
			best = coverage.Best
		}
		e.notifier.NotifyNewAsset(ctx, device, attrs.DeviceModel, best)
	}

	return nil
}

// updateAsset updates an existing Snipe-IT asset with current ABM data.
func (e *Engine) updateAsset(ctx context.Context, logger *logrus.Entry, device abmclient.Device, existing *snipeit.Asset, supplierID int, coverage *abmclient.CoverageResult) error {
	attrs := device.Attributes

	desired := snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: make(map[string]string),
		},
	}

	if supplierID > 0 {
		desired.Supplier = snipeit.Supplier{
			CommonFields: snipeit.CommonFields{ID: supplierID},
		}
	}

	// If ABM shows this device as released from the org, move it to the
	// archived status — unless it's already in some other archived-type
	// status (Donated, Stolen, Lost, etc.), which we leave alone rather than
	// overwrite with a generic "Archived". If the release date later clears
	// (device re-added to ABM), we deliberately do NOT auto-restore the
	// previous status; that's left as a manual step in Snipe-IT.
	if !attrs.ReleasedFromOrgDateTime.IsZero() && existing.StatusLabel.StatusType != "archived" {
		if e.cfg.SnipeIT.ArchivedStatusID != 0 {
			desired.StatusLabel = snipeit.StatusLabel{
				CommonFields: snipeit.CommonFields{ID: e.cfg.SnipeIT.ArchivedStatusID},
			}
		} else {
			logger.Warn("Device was released from ABM org but snipe_it.archived_status_id is not configured; leaving status unchanged")
		}
	}

	// Seed notes from existing asset so applyWarrantyNotes replaces only the
	// sentinel block and leaves any manual notes outside the block intact.
	desired.Notes = existing.Notes

	e.applyFieldMapping(ctx, &desired, device, coverage)
	applyWarrantyNotes(&desired, coverage)
	stripOrderInfoOnUpdate(&desired, existing, e.cfg.Sync.PreserveOrderInfoOnUpdate)

	// Unless force mode, compare desired values against current Snipe-IT values
	// and only send fields that are missing or different.
	update := &desired
	if !e.cfg.Sync.Force {
		update = e.diffAsset(&desired, existing)
		if update == nil {
			logger.Debug("All fields already match, skipping update")
			e.stats.Skipped++
			return nil
		}
	}

	if e.cfg.Sync.DryRun {
		logger.WithFields(logrus.Fields{
			"snipe_id": existing.ID,
			"updates":  formatAssetDiff(update),
		}).Info("[DRY RUN] Would update asset")
		e.stats.Updated++
		return nil
	}

	logger.WithField("payload", update).Debug("Sending update to Snipe-IT")

	// Carry model metadata on the update so PatchAsset can include it in
	// fieldset-error warnings without needing a separate lookup.
	if update.Model.ID == 0 {
		update.Model = existing.Model
	}

	if _, err := e.snipe.PatchAsset(ctx, existing.ID, *update); err != nil {
		return err
	}

	logger.Info("Updated asset in Snipe-IT")
	e.stats.Updated++
	return nil
}

// diffAsset compares desired asset values against the existing Snipe-IT asset
// and returns an asset containing only the fields that differ, or nil if everything matches.
func (e *Engine) diffAsset(desired *snipeit.Asset, existing *snipeit.Asset) *snipeit.Asset {
	diff := snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: make(map[string]string),
		},
	}
	hasChanges := false

	// Compare supplier ID
	if desired.Supplier.ID != 0 && desired.Supplier.ID != existing.Supplier.ID {
		diff.Supplier = desired.Supplier
		hasChanges = true
	}

	// Compare status label. Only ever set on desired by updateAsset's
	// archived-status handling (see attrs.ReleasedFromOrgDateTime below) —
	// axm2snipe otherwise never touches an asset's status on update, so a
	// normal sync can't clobber a status someone set manually in Snipe-IT.
	if desired.StatusLabel.ID != 0 && desired.StatusLabel.ID != existing.StatusLabel.ID {
		diff.StatusLabel = desired.StatusLabel
		hasChanges = true
	}

	// Compare warranty months
	if desired.WarrantyMonths != 0 && desired.WarrantyMonths != existing.WarrantyMonths {
		diff.WarrantyMonths = desired.WarrantyMonths
		hasChanges = true
	}

	// Compare native order info fields.
	// purchase_date is held in CustomFields as a workaround for upstream
	// SnipeTime.MarshalJSON (see applyFieldMapping). Compare the desired
	// date string against the existing asset's native PurchaseDate field
	// returned by GET, since Snipe-IT echoes purchase_date natively, not
	// in CustomFields.
	if desiredDate, ok := desired.CustomFields["purchase_date"]; ok {
		existingDate := ""
		if existing.PurchaseDate != nil && !existing.PurchaseDate.IsZero() {
			existingDate = existing.PurchaseDate.Format("2006-01-02")
		}
		if desiredDate != existingDate {
			diff.CustomFields["purchase_date"] = desiredDate
			hasChanges = true
		}
	}
	if desired.OrderNumber != "" && desired.OrderNumber != existing.OrderNumber {
		diff.OrderNumber = desired.OrderNumber
		hasChanges = true
	}

	// Compare full notes string; desired.Notes already contains the existing
	// content with only the sentinel block replaced, so a difference here means
	// the warranty block changed.
	// Snipe-IT HTML-encodes special characters (e.g. "&" → "&amp;") when storing
	// notes, so unescape the existing value before comparing.
	if desired.Notes != "" && desired.Notes != html.UnescapeString(existing.Notes) {
		diff.Notes = desired.Notes
		hasChanges = true
	}

	// Compare custom fields. Snipe-IT HTML-encodes field values via e()
	// (htmlspecialchars) in its API transformer, and BOOLEAN fields are stored
	// as "0"/"1" while we write "false"/"true". Normalize both before comparing.
	for key, desiredVal := range desired.CustomFields {
		// purchase_date is compared explicitly above against the native
		// existing.PurchaseDate field, not against existing.CustomFields
		// (which is always empty for this key on GET responses).
		if key == "purchase_date" {
			continue
		}
		currentVal := html.UnescapeString(existing.CustomFields[key])
		if normalizeBoolStr(currentVal) != normalizeBoolStr(desiredVal) {
			diff.CustomFields[key] = desiredVal
			hasChanges = true
		}
	}

	if !hasChanges {
		return nil
	}
	return &diff
}

// applyFieldMapping applies user-configured field mappings from config.
// All field mappings — ABM device attributes, AppleCare coverage, and standard
// Snipe-IT fields like purchase_date — are driven entirely by settings.yaml.
// Custom field keys (starting with _snipeit_) go into Asset.CustomFields;
// recognized native Snipe-IT field names (asset_tag, order_number, purchase_date)
// are routed to their corresponding Asset struct fields so they land in
// Snipe-IT's built-in UI (Order Information, etc.) instead of as custom fields.
// All other mapped keys go into CustomFields.
func (e *Engine) applyFieldMapping(ctx context.Context, asset *snipeit.Asset, device abmclient.Device, coverage *abmclient.CoverageResult) {
	var ac *abmclient.AppleCareCoverage
	if coverage != nil {
		ac = coverage.Best
	}
	attrs := device.Attributes
	for snipeField, abmField := range e.cfg.Sync.FieldMapping {
		var value string
		switch strings.ToLower(abmField) {
		// --- ABM device attributes ---
		case "serialnumber", "serial_number":
			value = attrs.SerialNumber
		case "devicemodel", "device_model":
			value = attrs.DeviceModel
		case "color":
			value = titleCase(attrs.Color)
		case "devicecapacity", "device_capacity":
			if attrs.DeviceCapacity != "" && !strings.EqualFold(attrs.DeviceCapacity, "Unknown") {
				value = normalizeStorage(attrs.DeviceCapacity)
			}
		case "partnumber", "part_number":
			value = attrs.PartNumber
		case "productfamily", "product_family":
			value = string(attrs.ProductFamily)
		case "producttype", "product_type":
			value = attrs.ProductType
		case "ordernumber", "order_number":
			// Skip Configurator-enrolled devices: ABM emits a synthetic
			// "CE-YYYY-MM-DD-HH-MM-SS-XXX" enrollment ID, not the real
			// order number. The sync.sync_configurator_order_info flag
			// is an opt-in escape hatch.
			if skipConfiguratorOrderInfo(e.cfg, attrs) {
				break
			}
			if attrs.OrderNumber != "" {
				value = cleanOrderNumber(attrs.OrderNumber)
			}
		case "orderdate", "order_date":
			// Same reasoning as order_number above — ABM emits the
			// Configurator enrollment date, not the actual purchase date.
			if skipConfiguratorOrderInfo(e.cfg, attrs) {
				break
			}
			if !attrs.OrderDateTime.IsZero() {
				value = attrs.OrderDateTime.Format("2006-01-02")
			}
		case "purchasesource", "purchase_source":
			value = titleCase(string(attrs.PurchaseSourceType))
		case "purchasesourceid", "purchase_source_id":
			value = attrs.PurchaseSourceID
		case "status":
			if strings.EqualFold(string(attrs.Status), "ASSIGNED") {
				value = "true"
			} else {
				value = "false"
			}
		case "imei":
			if len(attrs.IMEI) > 0 {
				value = strings.Join([]string(attrs.IMEI), ", ")
			}
		case "meid":
			if len(attrs.MEID) > 0 {
				value = strings.Join([]string(attrs.MEID), ", ")
			}
		case "wifi_mac", "wifimac":
			if len(attrs.WifiMacAddress) > 0 {
				value = formatMAC(strings.Join([]string(attrs.WifiMacAddress), ", "))
			}
		case "bluetooth_mac", "bluetoothmac":
			if len(attrs.BluetoothMacAddress) > 0 {
				value = formatMAC(strings.Join([]string(attrs.BluetoothMacAddress), ", "))
			}
		case "ethernet_mac", "ethernetmac":
			if len(attrs.EthernetMacAddress) > 0 {
				value = formatMAC(strings.Join(attrs.EthernetMacAddress, ", "))
			}
		case "eid":
			value = attrs.EID
		case "added_to_org", "addedtoorg":
			if !attrs.AddedToOrgDateTime.IsZero() {
				value = attrs.AddedToOrgDateTime.Format("2006-01-02")
			}
		case "assigned_server", "assignedserver", "mdm_server":
			value = device.AssignedServer
		case "released_from_org", "releasedfromorg":
			if !attrs.ReleasedFromOrgDateTime.IsZero() {
				value = attrs.ReleasedFromOrgDateTime.Format("2006-01-02")
			}
		case "is_released", "released":
			value = strconv.FormatBool(!attrs.ReleasedFromOrgDateTime.IsZero())

		// --- appledb.dev metadata (looked up by ProductType, cached per run) ---
		// These are not ABM fields — they come from the same appledb.dev
		// lookup used for sync.model_images, exposed here so the printed
		// regulatory model number, chip, and release year can be surfaced
		// as human-readable custom fields (ProductType alone, e.g.
		// "Mac16,10", isn't easy to eyeball at a glance).
		case "apple_model_number", "regulatory_model_number":
			if info := e.appleDBInfoFor(ctx, attrs.ProductType); info != nil {
				value = info.RegulatoryModel
			}
		case "chip", "chipset", "soc":
			if info := e.appleDBInfoFor(ctx, attrs.ProductType); info != nil {
				value = info.Chip
			}
		case "model_year", "release_year":
			if info := e.appleDBInfoFor(ctx, attrs.ProductType); info != nil {
				value = info.ReleaseYear
			}

		// --- AppleCare coverage fields ---
		case "applecare_status":
			if ac != nil {
				value = titleCase(ac.Status)
			}
		case "applecare_agreement":
			if ac != nil {
				value = ac.AgreementNumber
			}
		case "applecare_description":
			if ac != nil {
				value = ac.Description
			}
		case "applecare_start":
			if ac != nil && !ac.StartDateTime.IsZero() {
				value = ac.StartDateTime.Format("2006-01-02")
			}
		case "applecare_end":
			if ac != nil && !ac.EndDateTime.IsZero() {
				value = ac.EndDateTime.Format("2006-01-02")
			}
		case "applecare_renewable":
			if ac != nil {
				value = fmt.Sprintf("%t", ac.IsRenewable)
			}
		case "applecare_payment_type":
			if ac != nil {
				value = titleCase(ac.PaymentType)
			}
		}
		if value != "" {
			// Route top-level Snipe-IT fields to their proper struct fields
			// rather than CustomFields, so MarshalJSON does not overwrite them
			// and so they land in Snipe-IT's native UI (Order Information, etc.)
			// instead of as custom fields.
			switch snipeField {
			case "asset_tag":
				asset.AssetTag = value
			case "order_number":
				asset.OrderNumber = value
			case "purchase_date":
				// Snipe-IT's purchase_date validator requires YYYY-MM-DD, but
				// upstream go-snipeit's SnipeTime.MarshalJSON unconditionally
				// emits "YYYY-MM-DD HH:MM:SS" (datetime). Bypass by writing the
				// date-only string to CustomFields: Asset.MarshalJSON flattens
				// CustomFields to top-level keys *after* the native PurchaseDate
				// line, so the plain string overrides the bad serialization.
				// Snipe-IT routes the "purchase_date" key to its native column
				// regardless. TODO: remove once upstream serializes date-only.
				asset.CustomFields[snipeField] = value
			default:
				asset.CustomFields[snipeField] = value
			}
		}
	}

	// warranty_months: calculated from purchase_date to AppleCare end so that
	// Snipe-IT's auto-calculated "Warranty Expires" matches the actual coverage end.
	if ac != nil && !ac.EndDateTime.IsZero() && !attrs.OrderDateTime.IsZero() {
		months := int(ac.EndDateTime.Sub(attrs.OrderDateTime).Hours() / (24 * 30))
		if months > 0 {
			asset.WarrantyMonths = snipeit.FlexInt(months)
		}
	}
}

const (
	warrantyNotesStart = "=== axm2snipe:warranty-start ==="
	warrantyNotesEnd   = "=== axm2snipe:warranty-end ==="
)

// applyWarrantyNotes writes all AppleCare coverage records into a sentinel-delimited
// block in asset.Notes, preserving any existing notes outside the block.
// If coverage is nil or empty, any existing sentinel block is removed.
func applyWarrantyNotes(asset *snipeit.Asset, coverage *abmclient.CoverageResult) {
	existing := asset.Notes
	startIdx := strings.Index(existing, warrantyNotesStart)

	if coverage == nil || len(coverage.All) == 0 {
		// Remove any stale sentinel block so old warranty data is not left behind.
		if startIdx < 0 {
			return
		}
		endIdx := strings.Index(existing[startIdx:], warrantyNotesEnd)
		if endIdx < 0 {
			// Malformed: no end marker — remove from start onward.
			asset.Notes = strings.TrimSpace(existing[:startIdx])
			return
		}
		endIdx += startIdx // make absolute
		before := strings.TrimSpace(existing[:startIdx])
		after := strings.TrimSpace(existing[endIdx+len(warrantyNotesEnd):])
		switch {
		case before != "" && after != "":
			asset.Notes = before + "\n\n" + after
		case before != "":
			asset.Notes = before
		case after != "":
			asset.Notes = after
		default:
			asset.Notes = ""
		}
		return
	}

	// Build rows: [Status, Coverage, Start, End, Agreement, Payment]
	headers := []string{"Status", "Coverage", "Start", "End", "Agreement", "Payment"}
	rows := make([][]string, len(coverage.All))
	for i, c := range coverage.All {
		agreement := c.AgreementNumber
		if agreement == "" {
			agreement = "N/A"
		}
		payment := titleCase(c.PaymentType)
		if payment == "" || strings.ToUpper(c.PaymentType) == "NONE" {
			payment = "None"
		}
		rows[i] = []string{
			titleCase(c.Status),
			c.Description,
			c.StartDateTime.Format("2006-01-02"),
			c.EndDateTime.Format("2006-01-02"),
			agreement,
			payment,
		}
	}

	// Render pipe-separated table with header row
	var sb strings.Builder
	sb.WriteString(warrantyNotesStart + "\n")
	sb.WriteString(strings.Join(headers, " | ") + "\n")
	for _, row := range rows {
		sb.WriteString(strings.Join(row, " | ") + "\n")
	}
	sb.WriteString(warrantyNotesEnd)
	block := sb.String()

	if startIdx >= 0 {
		endIdx := strings.Index(existing[startIdx:], warrantyNotesEnd)
		if endIdx >= 0 {
			endIdx += startIdx // make absolute
			// Replace existing block in place
			before := strings.TrimSpace(existing[:startIdx])
			tail := strings.TrimSpace(existing[endIdx+len(warrantyNotesEnd):])
			switch {
			case before != "" && tail != "":
				asset.Notes = before + "\n\n" + block + "\n\n" + tail
			case before != "":
				asset.Notes = before + "\n\n" + block
			case tail != "":
				asset.Notes = block + "\n\n" + tail
			default:
				asset.Notes = block
			}
			return
		}
	}

	// No existing block — append.
	if existing != "" {
		asset.Notes = strings.TrimSpace(existing) + "\n\n" + block
	} else {
		asset.Notes = block
	}
}

// normalizeBoolStr normalizes boolean string representations so that "0"/"false"
// and "1"/"true" compare as equal. Snipe-IT returns "0"/"1" for BOOLEAN format
// fields on GET, but callers may write "false"/"true". Non-boolean strings are
// returned as-is (lowercased for case-insensitive comparison).
func normalizeBoolStr(s string) string {
	switch strings.ToLower(s) {
	case "0", "false":
		return "false"
	case "1", "true":
		return "true"
	default:
		return strings.ToLower(s)
	}
}

// cleanOrderNumber extracts the middle segment from CDW-style order numbers
// like "CDW/1CJ6QLW/002" → "1CJ6QLW". Other formats are returned as-is.
// skipConfiguratorOrderInfo reports whether order_date / order_number should
// be skipped for a device because ABM's values for it aren't real purchase
// data. A device added to ABM via Apple Configurator
// (purchaseSourceType=MANUALLY_ADDED) gets a synthetic order number like
// "CE-2024-12-13-04-11-12-826" and an order date equal to the enrollment
// time. Syncing those would overwrite better data already in Snipe-IT.
// The sync.sync_configurator_order_info flag is an opt-in escape hatch.
func skipConfiguratorOrderInfo(cfg *config.Config, attrs *abm.OrgDeviceAttributes) bool {
	if cfg.Sync.SyncConfiguratorOrderInfo {
		return false
	}
	return string(attrs.PurchaseSourceType) == "MANUALLY_ADDED"
}

// stripOrderInfoOnUpdate drops the order info from the desired asset when
// the sync.preserve_order_info_on_update flag is on and the existing asset
// already has values, so axm2snipe never overwrites order info on update.
// First-time syncs (existing has no value) still go through.
func stripOrderInfoOnUpdate(desired, existing *snipeit.Asset, preserve bool) {
	if !preserve {
		return
	}
	if existing.OrderNumber != "" {
		desired.OrderNumber = ""
	}
	if existing.PurchaseDate != nil && !existing.PurchaseDate.IsZero() {
		delete(desired.CustomFields, "purchase_date")
	}
}

func cleanOrderNumber(order string) string {
	parts := strings.Split(order, "/")
	if len(parts) == 3 {
		return parts[1]
	}
	return order
}

// titleCase converts "SPACE GRAY" to "Space Gray".
func titleCase(s string) string {
	// Replace underscores with spaces so "Paid_up_front" becomes "Paid Up Front"
	s = strings.ReplaceAll(s, "_", " ")
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// formatMAC inserts colons into a raw MAC address (e.g. "2CCA164BD29D" -> "2C:CA:16:4B:D2:9D").
// If the input already contains colons or is not 12 hex chars, it's returned as-is.
func formatMAC(s string) string {
	raw := strings.ReplaceAll(strings.ReplaceAll(s, ":", ""), "-", "")
	if len(raw) != 12 {
		return s
	}
	return strings.ToUpper(fmt.Sprintf("%s:%s:%s:%s:%s:%s",
		raw[0:2], raw[2:4], raw[4:6], raw[6:8], raw[8:10], raw[10:12]))
}

// normalizeStorage normalizes storage capacity to GB as a plain number.
// e.g. "256GB" -> "256", "1TB" -> "1024", "2TB" -> "2048".
func normalizeStorage(s string) string {
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	if strings.HasSuffix(upper, "TB") {
		num := strings.TrimSpace(s[:len(s)-2])
		if n, err := strconv.Atoi(num); err == nil {
			return strconv.Itoa(n * 1024)
		}
	}
	if strings.HasSuffix(upper, "GB") {
		return strings.TrimSpace(s[:len(s)-2])
	}
	return s
}

func deviceSerial(d abmclient.Device) string {
	if d.Attributes != nil {
		return d.Attributes.SerialNumber
	}
	return d.ID
}

// appleDBDeviceInfo holds selected metadata about a hardware identifier
// (e.g. "Mac16,10") fetched from appledb.dev.
type appleDBDeviceInfo struct {
	ImageKey        string // used (with ColorKey) to build the device image URL
	ColorKey        string // default color's key
	RegulatoryModel string // Apple's printed regulatory model number, e.g. "A3238"
	Chip            string // chip/SoC name, e.g. "M4"
	ReleaseYear     string // year extracted from the release date, e.g. "2024"
}

// appleDBInfoFor returns cached (or freshly fetched) appledb.dev metadata for
// the given hardware identifier. Results — including failed lookups (nil) —
// are cached on the Engine so multiple devices/models sharing an identifier
// only trigger one network call per run.
func (e *Engine) appleDBInfoFor(ctx context.Context, productType string) *appleDBDeviceInfo {
	if productType == "" {
		return nil
	}
	if e.appleDBCache == nil {
		e.appleDBCache = make(map[string]*appleDBDeviceInfo)
	}
	if info, ok := e.appleDBCache[productType]; ok {
		return info
	}
	info := fetchAppleDBInfo(ctx, productType)
	e.appleDBCache[productType] = info
	return info
}

// appleDBFlexString unmarshals an appledb.dev field that's usually a single
// JSON string but is sometimes an array of strings instead (observed on
// "released" for some older/multi-revision devices, e.g. "MacBook10,1" --
// presumably because such devices had multiple release dates across
// regions/revisions). Taking the first element rather than failing keeps one
// oddly-shaped field from breaking the whole lookup (and with it, the
// device's image, which used to be fetched independently of this field).
type appleDBFlexString string

func (s *appleDBFlexString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = appleDBFlexString(str)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		if len(arr) > 0 {
			*s = appleDBFlexString(arr[0])
		}
		return nil
	}
	// Unrecognized shape (e.g. an object) -- leave empty rather than fail
	// the whole appledb.dev decode over one non-critical field.
	return nil
}

// fetchAppleDBInfo fetches device metadata from appledb.dev for the given
// hardware identifier (e.g. "Mac16,10"). Returns nil on any error.
func fetchAppleDBInfo(ctx context.Context, productType string) *appleDBDeviceInfo {
	type appleDBDevice struct {
		ImageKey string            `json:"imageKey"`
		Model    []string          `json:"model"`
		SOC      appleDBFlexString `json:"soc"`
		Released appleDBFlexString `json:"released"`
		Colors   []struct {
			Key string `json:"key"`
		} `json:"colors"`
	}

	client := &http.Client{Timeout: 10 * time.Second}

	infoURL := fmt.Sprintf("https://api.appledb.dev/device/%s.json", productType)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		log.WithField("product_type", productType).WithError(err).Warn("AppleDB device lookup failed")
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.WithFields(logrus.Fields{"product_type": productType, "status": resp.StatusCode}).Warn("AppleDB returned non-200")
		return nil
	}

	var dev appleDBDevice
	if err := json.NewDecoder(resp.Body).Decode(&dev); err != nil {
		log.WithField("product_type", productType).WithError(err).Warn("AppleDB response could not be decoded")
		return nil
	}

	info := &appleDBDeviceInfo{ImageKey: dev.ImageKey, Chip: string(dev.SOC)}
	if len(dev.Colors) > 0 {
		info.ColorKey = dev.Colors[0].Key
	}
	if len(dev.Model) > 0 {
		info.RegulatoryModel = dev.Model[0]
	}
	if len(dev.Released) >= 4 {
		info.ReleaseYear = string(dev.Released)[:4]
	}
	return info
}

// appleDBImageDataURI downloads and validates the device image described by
// info, returning a base64 data URI suitable for Snipe-IT's image field, or
// "" on any failure (including a nil info, e.g. a failed metadata lookup).
func appleDBImageDataURI(ctx context.Context, info *appleDBDeviceInfo) string {
	if info == nil || info.ImageKey == "" || info.ColorKey == "" {
		return ""
	}

	client := &http.Client{Timeout: 10 * time.Second}
	imgURL := fmt.Sprintf("https://img.appledb.dev/device@main/%s/%s.png", info.ImageKey, info.ColorKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return ""
	}
	imgResp, err := client.Do(req)
	if err != nil {
		log.WithField("image_url", imgURL).WithError(err).Warn("AppleDB image fetch failed")
		return ""
	}
	defer imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK {
		log.WithFields(logrus.Fields{"image_url": imgURL, "status": imgResp.StatusCode}).Warn("AppleDB image returned non-200")
		return ""
	}

	// Validate content type and cap body size (2 MiB) before buffering.
	const maxModelImageBytes = 2 << 20
	if ct := imgResp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		log.WithFields(logrus.Fields{"image_url": imgURL, "content_type": ct}).Warn("AppleDB returned unexpected content type")
		return ""
	}
	imgBytes, err := io.ReadAll(io.LimitReader(imgResp.Body, maxModelImageBytes+1))
	if err != nil {
		log.WithField("image_url", imgURL).WithError(err).Warn("Reading AppleDB image failed")
		return ""
	}
	if len(imgBytes) > maxModelImageBytes {
		log.WithField("image_url", imgURL).Warn("AppleDB image too large, skipping")
		return ""
	}
	// Verify PNG magic bytes.
	if len(imgBytes) < 8 || string(imgBytes[:8]) != "\x89PNG\r\n\x1a\n" {
		log.WithField("image_url", imgURL).Warn("AppleDB image is not a valid PNG, skipping")
		return ""
	}
	log.WithField("image_url", imgURL).Debug("Fetched model image from AppleDB")
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes)
}

// fetchModelImage fetches a device image from appledb.dev for the given
// hardware identifier (e.g. "Mac16,10") and returns it as a base64 data URI
// suitable for Snipe-IT's image field. Returns "" on any error. Used by
// BackfillModelImages, which is not tied to an Engine's appleDBCache (each
// model is visited exactly once, so per-run caching wouldn't help there).
func fetchModelImage(ctx context.Context, productType string) string {
	return appleDBImageDataURI(ctx, fetchAppleDBInfo(ctx, productType))
}

// formatAssetDiff returns a human-readable summary of an asset diff for logging.
func formatAssetDiff(a *snipeit.Asset) map[string]any {
	m := make(map[string]any)
	if a.Supplier.ID != 0 {
		m["supplier_id"] = a.Supplier.ID
	}
	if a.WarrantyMonths != 0 {
		m["warranty_months"] = a.WarrantyMonths.Int()
	}
	if a.Notes != "" {
		m["notes"] = a.Notes
	}
	if a.OrderNumber != "" {
		m["order_number"] = a.OrderNumber
	}
	for k, v := range a.CustomFields {
		m[k] = v
	}
	return m
}
