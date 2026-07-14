package sync

import (
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
