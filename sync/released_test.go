package sync

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/CampusTech/abm"
	"github.com/CampusTech/axm2snipe/abmclient"
	"github.com/CampusTech/axm2snipe/config"
)

func releasedTestDevice(serial string, released bool) abmclient.Device {
	attrs := &abm.OrgDeviceAttributes{SerialNumber: serial}
	if released {
		attrs.ReleasedFromOrgDateTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}
	return abmclient.Device{OrgDevice: abm.OrgDevice{ID: serial, Attributes: attrs}}
}

func TestReleasedSerialsFromCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.csv")
	data := "SERIAL_NO,MODEL_NAME,DATE_REMOVED_FROM_ORGANIZATION\n" +
		"LIVE1,MacBook Air,\n" +
		"REL1,\"MacBook Pro, 16-inch\",2024-01-02T03:04:05Z\n" +
		" rel2 ,iPad,2025-02-03T04:05:06Z\n" +
		"REL1,duplicate,2024-01-02T03:04:05Z\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := releasedSerialsFromCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"REL1", "rel2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("releasedSerialsFromCSV() = %v, want %v", got, want)
	}
}

func TestReleasedSerialsFromCSVRequiresAppleHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.csv")
	if err := os.WriteFile(path, []byte("serial,released\nA,yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := releasedSerialsFromCSV(path); err == nil {
		t.Fatal("expected missing-header error")
	}
}

func TestReleasedDevicesFromCSVBuildsFallbackRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.csv")
	data := "SERIAL_NO,DEVICE_ID,PRODUCT_FAMILY,MODEL_NAME,PART_NUMBER,DEVICE_CAPACITY,COLOR,PURCHASE_SOURCE,ORDER_NUMBER,DATE_ADDED_TO_ORGANIZATION,DATE_REMOVED_FROM_ORGANIZATION,IMEI,IMEI2,MEID,WIFI_MAC_ADDRESS,BLUETOOTH_MAC_ADDRESS,ETHERNET_MAC_ADDRESS_1,ETHERNET_MAC_ADDRESS_2\n" +
		"SER1,DEV1,Mac,MacBook Pro,ABC123,512GB,SPACE GRAY,Apple (1146073),ORDER1,2020-01-02T03:04:05Z,2022-06-07T08:09:10Z,I1,I2,M1,AA,BB,CC,DD\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	devices, err := releasedDevicesFromCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	device := devices["SER1"]
	attrs := device.Attributes
	if device.ID != "DEV1" || attrs == nil {
		t.Fatalf("unexpected fallback identity: %+v", device)
	}
	if attrs.SerialNumber != "SER1" || attrs.DeviceModel != "MacBook Pro" || attrs.PartNumber != "ABC123" {
		t.Fatalf("unexpected fallback attributes: %+v", attrs)
	}
	if got := attrs.ReleasedFromOrgDateTime.Format(time.RFC3339); got != "2022-06-07T08:09:10Z" {
		t.Fatalf("release date = %s", got)
	}
	if attrs.PurchaseSourceType != abm.PurchaseSourceTypeApple || attrs.PurchaseSourceID != "1146073" {
		t.Fatalf("purchase source = %q/%q", attrs.PurchaseSourceType, attrs.PurchaseSourceID)
	}
	if !reflect.DeepEqual(attrs.IMEI, []string{"I1", "I2"}) || !reflect.DeepEqual(attrs.EthernetMacAddress, []string{"CC", "DD"}) {
		t.Fatalf("network/identity fields not preserved: %+v", attrs)
	}
}

func TestIsABMNotFound(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &abm.APIError{StatusCode: http.StatusNotFound})
	if !isABMNotFound(err) {
		t.Fatal("wrapped ABM 404 was not recognized")
	}
	if isABMNotFound(&abm.APIError{StatusCode: http.StatusTooManyRequests}) {
		t.Fatal("non-404 ABM error was recognized as not found")
	}
}

func TestMergeDevicesBySerialPrefersPrimary(t *testing.T) {
	primary := releasedTestDevice("abc", false)
	supplementalDuplicate := releasedTestDevice("ABC", true)
	supplementalNew := releasedTestDevice("DEF", true)

	got := mergeDevicesBySerial([]abmclient.Device{primary}, []abmclient.Device{supplementalDuplicate, supplementalNew})
	if len(got) != 2 {
		t.Fatalf("got %d devices, want 2", len(got))
	}
	if !got[0].Attributes.ReleasedFromOrgDateTime.IsZero() {
		t.Fatal("supplemental released record replaced fresh primary record")
	}
	if deviceSerial(got[1]) != "DEF" {
		t.Fatalf("second serial = %q, want DEF", deviceSerial(got[1]))
	}
}

func TestLoadPersistentReleasedDevicesSeedsFromDeviceCache(t *testing.T) {
	cacheDir := t.TempDir()
	e := NewDownloadEngine(nil, &config.Config{Sync: config.SyncConfig{CacheDir: cacheDir}})
	devices := []abmclient.Device{releasedTestDevice("LIVE", false), releasedTestDevice("RELEASED", true)}
	if err := writeJSON(cacheDir, "devices.json", devices); err != nil {
		t.Fatal(err)
	}

	got, err := e.loadPersistentReleasedDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || deviceSerial(got[0]) != "RELEASED" {
		t.Fatalf("got %+v, want only RELEASED", got)
	}
}

func TestMergePersistentReleasedIntoDeviceCache(t *testing.T) {
	cacheDir := t.TempDir()
	e := NewDownloadEngine(nil, &config.Config{Sync: config.SyncConfig{CacheDir: cacheDir}})
	if err := writeJSON(cacheDir, "devices.json", []abmclient.Device{releasedTestDevice("LIVE", false)}); err != nil {
		t.Fatal(err)
	}
	if err := e.mergePersistentReleasedIntoDeviceCache([]abmclient.Device{releasedTestDevice("OLD", true)}); err != nil {
		t.Fatal(err)
	}

	if err := e.loadDevicesFromCache(); err != nil {
		t.Fatal(err)
	}
	if len(e.cache.Devices) != 2 {
		t.Fatalf("merged cache contains %d devices, want 2", len(e.cache.Devices))
	}
}
