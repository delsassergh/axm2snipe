package sync

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CampusTech/abm"
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
	CSVFallback  int
	Failed       int
	CachedTotal  int
}

// ImportReleasedDevicesCSV seeds the persistent released-device cache from an
// Apple Business Manager device export. The CSV is used only to discover
// historical released serials; canonical attributes are fetched from Apple's
// single-device endpoint, which continues to return released devices even
// though the bulk endpoint omits them.
func (e *Engine) ImportReleasedDevicesCSV(ctx context.Context, csvPath string) (*ReleasedImportStats, error) {
	csvDevices, err := releasedDevicesFromCSV(csvPath)
	if err != nil {
		return nil, err
	}
	serials := sortedDeviceSerials(csvDevices)

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
			if isABMNotFound(fetchErr) {
				fallback := csvDevices[normalizeSerial(serial)]
				persistent = append(persistent, fallback)
				known[normalizeSerial(serial)] = fallback
				stats.CSVFallback++
				stats.CachedTotal = len(persistent)
				successesSinceCheckpoint++
				log.WithField("serial", serial).Warn("Apple no longer returns this released device; using the Apple CSV record")
				continue
			}
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
	devices, err := releasedDevicesFromCSV(path)
	if err != nil {
		return nil, err
	}
	return sortedDeviceSerials(devices), nil
}

func sortedDeviceSerials(devices map[string]abmclient.Device) []string {
	keys := make([]string, 0, len(devices))
	for key := range devices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	serials := make([]string, 0, len(keys))
	for _, key := range keys {
		serials = append(serials, deviceSerial(devices[key]))
	}
	return serials
}

func releasedDevicesFromCSV(path string) (map[string]abmclient.Device, error) {
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

	devices := make(map[string]abmclient.Device)
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
		if key == "" {
			continue
		}
		if _, exists := devices[key]; exists {
			continue
		}
		device, parseErr := releasedDeviceFromCSVRecord(columns, record)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing Apple device export row %d: %w", row, parseErr)
		}
		devices[key] = device
	}
	return devices, nil
}

func releasedDeviceFromCSVRecord(columns map[string]int, record []string) (abmclient.Device, error) {
	serial := csvRecordValue(columns, record, releasedCSVSerialColumn)
	released, err := parseCSVTime(csvRecordValue(columns, record, releasedCSVDateColumn))
	if err != nil {
		return abmclient.Device{}, fmt.Errorf("invalid release date for %s: %w", serial, err)
	}
	added, err := parseCSVTime(csvRecordValue(columns, record, "DATE_ADDED_TO_ORGANIZATION"))
	if err != nil {
		return abmclient.Device{}, fmt.Errorf("invalid added date for %s: %w", serial, err)
	}
	purchaseType, purchaseID := parseCSVPurchaseSource(csvRecordValue(columns, record, "PURCHASE_SOURCE"))

	attrs := &abm.OrgDeviceAttributes{
		SerialNumber:            serial,
		AddedToOrgDateTime:      added,
		ReleasedFromOrgDateTime: released,
		UpdatedDateTime:         released,
		DeviceModel:             csvRecordValue(columns, record, "MODEL_NAME"),
		ProductFamily:           abm.OrgDeviceAttributesProductFamily(csvRecordValue(columns, record, "PRODUCT_FAMILY")),
		PartNumber:              csvRecordValue(columns, record, "PART_NUMBER"),
		DeviceCapacity:          csvRecordValue(columns, record, "DEVICE_CAPACITY"),
		Color:                   csvRecordValue(columns, record, "COLOR"),
		PurchaseSourceType:      purchaseType,
		PurchaseSourceID:        purchaseID,
		OrderNumber:             csvRecordValue(columns, record, "ORDER_NUMBER"),
		Status:                  abm.StatusUnAssigned,
	}
	attrs.IMEI = nonEmptyCSVValues(csvRecordValue(columns, record, "IMEI"), csvRecordValue(columns, record, "IMEI2"))
	attrs.MEID = nonEmptyCSVValues(csvRecordValue(columns, record, "MEID"))
	attrs.WifiMacAddress = abm.FlexStringSlice(nonEmptyCSVValues(csvRecordValue(columns, record, "WIFI_MAC_ADDRESS")))
	attrs.BluetoothMacAddress = abm.FlexStringSlice(nonEmptyCSVValues(csvRecordValue(columns, record, "BLUETOOTH_MAC_ADDRESS")))
	attrs.EthernetMacAddress = nonEmptyCSVValues(
		csvRecordValue(columns, record, "ETHERNET_MAC_ADDRESS_1"),
		csvRecordValue(columns, record, "ETHERNET_MAC_ADDRESS_2"),
		csvRecordValue(columns, record, "ETHERNET_MAC_ADDRESS_3"),
	)
	id := csvRecordValue(columns, record, "DEVICE_ID")
	if id == "" {
		id = serial
	}
	return abmclient.Device{OrgDevice: abm.OrgDevice{ID: id, Type: "orgDevices", Attributes: attrs}}, nil
}

func csvRecordValue(columns map[string]int, record []string, name string) string {
	i, ok := columns[name]
	if !ok || i >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[i])
}

func parseCSVTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func parseCSVPurchaseSource(value string) (abm.OrgDeviceAttributesPurchaseSourceType, string) {
	value = strings.TrimSpace(value)
	upper := strings.ToUpper(value)
	var sourceType abm.OrgDeviceAttributesPurchaseSourceType
	switch {
	case strings.HasPrefix(upper, "APPLE"):
		sourceType = abm.PurchaseSourceTypeApple
	case strings.HasPrefix(upper, "RESELLER"):
		sourceType = abm.PurchaseSourceTypeReseller
	case strings.HasPrefix(upper, "MANUALLY ADDED"):
		sourceType = abm.PurchaseSourceTypeManuallyAdded
	}
	start := strings.LastIndex(value, "(")
	if start >= 0 && strings.HasSuffix(value, ")") {
		return sourceType, strings.TrimSpace(value[start+1 : len(value)-1])
	}
	return sourceType, ""
}

func nonEmptyCSVValues(values ...string) []string {
	var result []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func isABMNotFound(err error) bool {
	var apiErr *abm.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
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
