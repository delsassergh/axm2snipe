package sync

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/CampusTech/axm2snipe/abmclient"
)

const (
	releasedDevicesFile     = "released-devices.json"
	releasedCheckpointEvery = 10
	releasedCSVSerialColumn = "SERIAL_NO"
	releasedCSVDateColumn   = "DATE_REMOVED_FROM_ORGANIZATION"
)

// ReleasedImportStats describes a historical released-device CSV import.
type ReleasedImportStats struct {
	CSVReleased  int
	AlreadyKnown int
	Fetched      int
	Failed       int
	CachedTotal  int
}

// ImportReleasedDevicesCSV seeds the persistent released-device cache from an
// Apple Business Manager device export. The CSV is used only to discover
// historical released serials; canonical attributes are fetched from Apple's
// single-device endpoint, which continues to return released devices even
// though the bulk endpoint omits them.
func (e *Engine) ImportReleasedDevicesCSV(ctx context.Context, csvPath string) (*ReleasedImportStats, error) {
	serials, err := releasedSerialsFromCSV(csvPath)
	if err != nil {
		return nil, err
	}

	persistent, err := e.loadPersistentReleasedDevices()
	if err != nil {
		return nil, err
	}
	known := indexDevicesBySerial(persistent)
	stats := &ReleasedImportStats{CSVReleased: len(serials), CachedTotal: len(persistent)}
	var toFetch []string
	for _, serial := range serials {
		if _, ok := known[normalizeSerial(serial)]; ok {
			stats.AlreadyKnown++
			continue
		}
		toFetch = append(toFetch, serial)
	}

	log.Infof("Apple export contains %d released device(s); %d already cached, %d need API lookup", len(serials), stats.AlreadyKnown, len(toFetch))
	var failedSerials []string
	successesSinceCheckpoint := 0

	for _, serial := range toFetch {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if saveErr := e.savePersistentReleasedDevices(persistent); saveErr != nil {
				return stats, fmt.Errorf("%w (also failed to save checkpoint: %v)", ctxErr, saveErr)
			}
			return stats, ctxErr
		}
		device, fetchErr := e.abm.GetDeviceInfo(ctx, serial)
		if fetchErr != nil {
			stats.Failed++
			failedSerials = append(failedSerials, serial)
			log.WithError(fetchErr).WithField("serial", serial).Warn("Could not fetch historical released device; rerun the import to retry")
			continue
		}

		persistent = append(persistent, *device)
		known[normalizeSerial(serial)] = *device
		stats.Fetched++
		stats.CachedTotal = len(persistent)
		successesSinceCheckpoint++
		if successesSinceCheckpoint >= releasedCheckpointEvery {
			if saveErr := e.savePersistentReleasedDevices(persistent); saveErr != nil {
				return stats, fmt.Errorf("saving released-device checkpoint: %w", saveErr)
			}
			successesSinceCheckpoint = 0
			log.Infof("Historical release import progress: %d/%d fetched this run (%d persistently cached)", stats.Fetched, len(toFetch), len(persistent))
		}
	}

	if err := e.savePersistentReleasedDevices(persistent); err != nil {
		return stats, fmt.Errorf("saving released-device cache: %w", err)
	}
	if err := e.mergePersistentReleasedIntoDeviceCache(persistent); err != nil {
		return stats, err
	}

	if stats.Failed > 0 {
		return stats, fmt.Errorf("%d historical device lookup(s) failed (first: %s); rerun to retry", stats.Failed, strings.Join(failedSerials[:min(len(failedSerials), 5)], ", "))
	}
	return stats, nil
}

func releasedSerialsFromCSV(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening Apple device export %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading Apple device export header: %w", err)
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[strings.ToUpper(strings.TrimSpace(name))] = i
	}
	serialCol, hasSerial := columns[releasedCSVSerialColumn]
	dateCol, hasDate := columns[releasedCSVDateColumn]
	if !hasSerial || !hasDate {
		return nil, fmt.Errorf("Apple device export must contain %s and %s columns", releasedCSVSerialColumn, releasedCSVDateColumn)
	}

	var serials []string
	seen := make(map[string]bool)
	for row := 2; ; row++ {
		record, readErr := r.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("reading Apple device export row %d: %w", row, readErr)
		}
		if serialCol >= len(record) || dateCol >= len(record) || strings.TrimSpace(record[dateCol]) == "" {
			continue
		}
		serial := strings.TrimSpace(record[serialCol])
		key := normalizeSerial(serial)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		serials = append(serials, serial)
	}
	return serials, nil
}

// loadPersistentReleasedDevices also seeds from released records already in
// devices.json. This makes upgrades safe when audit recovery ran before the
// dedicated persistent file was introduced.
func (e *Engine) loadPersistentReleasedDevices() ([]abmclient.Device, error) {
	var persistent []abmclient.Device
	persistentPath := filepath.Join(e.CacheDir(), releasedDevicesFile)
	if data, err := os.ReadFile(persistentPath); err == nil {
		if err := json.Unmarshal(data, &persistent); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", persistentPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", persistentPath, err)
	}

	devicesPath := filepath.Join(e.CacheDir(), "devices.json")
	if data, err := os.ReadFile(devicesPath); err == nil {
		var cached []abmclient.Device
		if err := json.Unmarshal(data, &cached); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", devicesPath, err)
		}
		var cachedReleased []abmclient.Device
		for _, device := range cached {
			if device.Attributes != nil && !device.Attributes.ReleasedFromOrgDateTime.IsZero() {
				cachedReleased = append(cachedReleased, device)
			}
		}
		persistent = mergeDevicesBySerial(persistent, cachedReleased)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", devicesPath, err)
	}
	return persistent, nil
}

func (e *Engine) savePersistentReleasedDevices(devices []abmclient.Device) error {
	return writeJSON(e.CacheDir(), releasedDevicesFile, mergeDevicesBySerial(nil, devices))
}

func (e *Engine) mergePersistentReleasedIntoDeviceCache(released []abmclient.Device) error {
	path := filepath.Join(e.CacheDir(), "devices.json")
	var devices []abmclient.Device
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &devices); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	devices = mergeDevicesBySerial(devices, released)
	if err := writeJSON(e.CacheDir(), "devices.json", devices); err != nil {
		return fmt.Errorf("writing merged device cache: %w", err)
	}
	log.Infof("Merged %d persistent released device(s) into %s (%d total devices)", len(released), path, len(devices))
	return nil
}

func mergeDevicesBySerial(primary, supplemental []abmclient.Device) []abmclient.Device {
	merged := append([]abmclient.Device(nil), primary...)
	seen := make(map[string]bool, len(primary)+len(supplemental))
	for _, d := range primary {
		seen[normalizeSerial(deviceSerial(d))] = true
	}
	for _, d := range supplemental {
		key := normalizeSerial(deviceSerial(d))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, d)
	}
	return merged
}

func indexDevicesBySerial(devices []abmclient.Device) map[string]abmclient.Device {
	indexed := make(map[string]abmclient.Device, len(devices))
	for _, d := range devices {
		if key := normalizeSerial(deviceSerial(d)); key != "" {
			indexed[key] = d
		}
	}
	return indexed
}

func normalizeSerial(serial string) string {
	return strings.ToUpper(strings.TrimSpace(serial))
}
