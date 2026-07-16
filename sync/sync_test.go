package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CampusTech/abm"
	snipeit "github.com/michellepellon/go-snipeit"

	"github.com/CampusTech/axm2snipe/abmclient"
	"github.com/CampusTech/axm2snipe/config"
	"github.com/CampusTech/axm2snipe/snipe"
)

// --- Pure function tests ---

func TestTitleCase(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"SILVER", "Silver"},
		{"SPACE GRAY", "Space Gray"},
		{"SKY BLUE", "Sky Blue"},
		{"Paid_up_front", "Paid Up Front"},
		{"MANUALLY_ADDED", "Manually Added"},
		{"active", "Active"},
		{"", ""},
		{"SPACE  BLACK", "Space Black"},
	}
	for _, tt := range tests {
		got := titleCase(tt.input)
		if got != tt.want {
			t.Errorf("titleCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatMAC(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"2CCA164BD29D", "2C:CA:16:4B:D2:9D"},
		{"aabbccddeeff", "AA:BB:CC:DD:EE:FF"},
		{"2C:CA:16:4B:D2:9D", "2C:CA:16:4B:D2:9D"}, // already formatted
		{"AA-BB-CC-DD-EE-FF", "AA:BB:CC:DD:EE:FF"}, // dash-separated
		{"short", "short"},                         // too short
		{"", ""},
	}
	for _, tt := range tests {
		got := formatMAC(tt.input)
		if got != tt.want {
			t.Errorf("formatMAC(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCleanOrderNumber(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"CDW/1CJ6QLW/002", "1CJ6QLW"},
		{"SIMPLE-ORDER", "SIMPLE-ORDER"},
		{"A/B/C", "B"},
		{"NO-SLASHES", "NO-SLASHES"},
		{"ONE/SLASH", "ONE/SLASH"},
		{"", ""},
	}
	for _, tt := range tests {
		got := cleanOrderNumber(tt.input)
		if got != tt.want {
			t.Errorf("cleanOrderNumber(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeStorage(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"256GB", "256"},
		{"512GB", "512"},
		{"1TB", "1024"},
		{"2TB", "2048"},
		{"128", "128"},
		{" 256GB ", "256"},
		{"", ""},
	}
	for _, tt := range tests {
		got := normalizeStorage(tt.input)
		if got != tt.want {
			t.Errorf("normalizeStorage(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDeviceSerial(t *testing.T) {
	// With attributes
	d := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			ID: "DEV001",
			Attributes: &abm.OrgDeviceAttributes{
				SerialNumber: "ABC123",
			},
		},
	}
	if got := deviceSerial(d); got != "ABC123" {
		t.Errorf("deviceSerial with attrs = %q, want ABC123", got)
	}

	// Without attributes
	d2 := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			ID: "DEV002",
		},
	}
	if got := deviceSerial(d2); got != "DEV002" {
		t.Errorf("deviceSerial without attrs = %q, want DEV002", got)
	}
}

// --- Filter tests ---

func TestFilterByProductFamily(t *testing.T) {
	devices := []abmclient.Device{
		makeDevice("D1", "SN001", "Mac", "MacBookPro18,1"),
		makeDevice("D2", "SN002", "iPhone", "iPhone14,5"),
		makeDevice("D3", "SN003", "iPad", "iPad13,1"),
		makeDevice("D4", "SN004", "Mac", "Mac14,7"),
	}

	tests := []struct {
		name     string
		families []string
		want     int
	}{
		{"no filter", nil, 4},
		{"mac only", []string{"Mac"}, 2},
		{"iphone only", []string{"iPhone"}, 1},
		{"mac+ipad", []string{"Mac", "iPad"}, 3},
		{"case insensitive", []string{"mac"}, 2},
		{"no match", []string{"Watch"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{cfg: &config.Config{Sync: config.SyncConfig{ProductFamilies: tt.families}}}
			got := e.filterByProductFamily(devices)
			if len(got) != tt.want {
				t.Errorf("filterByProductFamily(%v) returned %d devices, want %d", tt.families, len(got), tt.want)
			}
		})
	}
}

// --- diffAsset tests ---

func TestNormalizeBoolStr(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"0", "false"},
		{"false", "false"},
		{"FALSE", "false"},
		{"1", "true"},
		{"true", "true"},
		{"TRUE", "true"},
		{"", ""},
	}
	for _, tt := range tests {
		got := normalizeBoolStr(tt.input)
		if got != tt.want {
			t.Errorf("normalizeBoolStr(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDiffAsset_BooleanNormalization(t *testing.T) {
	// Snipe-IT returns "0"/"1" for BOOLEAN fields; we write "false"/"true".
	// diffAsset must treat them as equal so we don't send no-op updates.
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_renewable_1":    "false",
				"_snipeit_mdm_assigned_2": "true",
			},
		},
	}
	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_renewable_1":    "0", // Snipe-IT stores "0" for false
				"_snipeit_mdm_assigned_2": "1", // Snipe-IT stores "1" for true
			},
		},
	}
	if diff := e.diffAsset(desired, existing); diff != nil {
		t.Errorf("diffAsset should return nil when BOOLEAN values are equivalent (0/false, 1/true), got %+v", diff)
	}
}

func TestDiffAsset_NoChanges(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_color_1": "Silver",
			},
		},
	}
	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_color_1": "Silver",
			},
		},
	}
	if diff := e.diffAsset(desired, existing); diff != nil {
		t.Errorf("diffAsset should return nil when no changes, got %+v", diff)
	}
}

func TestDiffAsset_CustomFieldDiff(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_color_1":  "Silver",
				"_snipeit_status_2": "Active",
			},
		},
	}
	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_color_1":  "Silver",
				"_snipeit_status_2": "Expired",
			},
		},
	}
	diff := e.diffAsset(desired, existing)
	if diff == nil {
		t.Fatal("diffAsset should return diff when custom fields differ")
	}
	if diff.CustomFields["_snipeit_status_2"] != "Active" {
		t.Errorf("expected _snipeit_status_2=Active, got %q", diff.CustomFields["_snipeit_status_2"])
	}
	if _, ok := diff.CustomFields["_snipeit_color_1"]; ok {
		t.Error("unchanged field _snipeit_color_1 should not be in diff")
	}
}

func TestDiffAsset_CustomFieldHTMLEncoding(t *testing.T) {
	// Snipe-IT HTML-encodes custom field values. A description like
	// "AppleCare+ Theft & Loss" comes back as "AppleCare+ Theft &amp; Loss".
	// diffAsset must unescape before comparing.
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_axm_applecare_description_26": "AppleCare+ Theft & Loss",
			},
		},
	}
	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_axm_applecare_description_26": "AppleCare+ Theft &amp; Loss",
			},
		},
	}
	if diff := e.diffAsset(desired, existing); diff != nil {
		t.Errorf("diffAsset should return nil when custom field values differ only by HTML encoding, got %+v", diff)
	}
}

func TestDiffAsset_NotesHTMLEncoding(t *testing.T) {
	// Snipe-IT HTML-encodes special characters (e.g. "&" → "&amp;") when storing
	// notes. diffAsset must unescape before comparing so descriptions like
	// "AppleCare+ Theft & Loss" don't trigger a spurious update every sync.
	e := &Engine{cfg: &config.Config{}}
	// Generate a real table-formatted notes block via applyWarrantyNotes so the
	// test stays in sync with the actual format automatically.
	ac := abmclient.AppleCareCoverage{
		Status:        "INACTIVE",
		Description:   "AppleCare+ Theft & Loss",
		StartDateTime: time.Date(2025, 9, 27, 0, 0, 0, 0, time.UTC),
		EndDateTime:   time.Date(2025, 9, 28, 0, 0, 0, 0, time.UTC),
	}
	base := &snipeit.Asset{}
	applyWarrantyNotes(base, &abmclient.CoverageResult{Best: &ac, All: []abmclient.AppleCareCoverage{ac}}, true)
	notes := base.Notes

	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			Notes:        notes,
			CustomFields: map[string]string{},
		},
	}
	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			// Snipe-IT returns HTML-encoded version: & → &amp;
			Notes:        strings.ReplaceAll(notes, "&", "&amp;"),
			CustomFields: map[string]string{},
		},
	}
	if diff := e.diffAsset(desired, existing); diff != nil {
		t.Errorf("diffAsset should return nil when notes differ only by HTML encoding, got diff with notes=%q", diff.Notes)
	}
}

func TestDiffAsset_SupplierDiff(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}},
	}
	desired.Supplier = snipeit.Supplier{CommonFields: snipeit.CommonFields{ID: 5}}

	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}},
	}
	existing.Supplier = snipeit.Supplier{CommonFields: snipeit.CommonFields{ID: 3}}

	diff := e.diffAsset(desired, existing)
	if diff == nil {
		t.Fatal("diffAsset should detect supplier change")
	}
	if diff.Supplier.ID != 5 {
		t.Errorf("expected supplier ID 5, got %d", diff.Supplier.ID)
	}
}

func TestDiffAsset_PurchaseDateDiff(t *testing.T) {
	// Desired purchase_date is set via CustomFields (the workaround for
	// upstream SnipeTime serialization); existing comes from a Snipe-IT
	// GET response with the native PurchaseDate field populated.
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{CommonFields: snipeit.CommonFields{
		CustomFields: map[string]string{"purchase_date": "2024-06-15"},
	}}

	existing := &snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}}}
	existing.PurchaseDate = &snipeit.SnipeTime{Time: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)}

	diff := e.diffAsset(desired, existing)
	if diff == nil {
		t.Fatal("diffAsset should detect purchase_date change")
	}
	if got := diff.CustomFields["purchase_date"]; got != "2024-06-15" {
		t.Errorf("diff.CustomFields[purchase_date] = %q, want 2024-06-15", got)
	}
}

func TestDiffAsset_PurchaseDateUnchanged(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	date := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	desired := &snipeit.Asset{CommonFields: snipeit.CommonFields{
		CustomFields: map[string]string{"purchase_date": "2024-06-15"},
	}}

	existing := &snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}}}
	existing.PurchaseDate = &snipeit.SnipeTime{Time: date}

	if diff := e.diffAsset(desired, existing); diff != nil {
		t.Errorf("diffAsset should return nil when purchase_date matches, got %+v", diff)
	}
}

func TestDiffAsset_OrderNumberDiff(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}}}
	desired.OrderNumber = "NEW123"

	existing := &snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}}}
	existing.OrderNumber = "OLD456"

	diff := e.diffAsset(desired, existing)
	if diff == nil {
		t.Fatal("diffAsset should detect order_number change")
	}
	if diff.OrderNumber != "NEW123" {
		t.Errorf("diff.OrderNumber = %q, want NEW123", diff.OrderNumber)
	}
}

// TestApplyFieldMapping_SkipsOrderForManuallyAdded verifies that ABM's
// orderDateTime and orderNumber are NOT synced when the device was added
// via Apple Configurator (purchaseSourceType=MANUALLY_ADDED). In that
// case ABM's values are Configurator enrollment metadata (e.g.
// "CE-2024-12-13-04-11-12-826" and the enrollment date), not the real
// purchase data, and would overwrite better data already in Snipe-IT.
// The sync.sync_configurator_order_info flag is an opt-in escape hatch.
func TestApplyFieldMapping_SkipsOrderForManuallyAdded(t *testing.T) {
	tests := []struct {
		name                 string
		syncConfiguratorInfo bool
		wantOrderNumber      string
		wantDateInCF         bool
	}{
		{name: "default skip", syncConfiguratorInfo: false, wantOrderNumber: "", wantDateInCF: false},
		{name: "opt-in sync", syncConfiguratorInfo: true, wantOrderNumber: "CE-2024-12-13-04-11-12-826", wantDateInCF: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{cfg: &config.Config{
				Sync: config.SyncConfig{
					SyncConfiguratorOrderInfo: tt.syncConfiguratorInfo,
					FieldMapping: map[string]string{
						"purchase_date": "order_date",
						"order_number":  "order_number",
					},
				},
			}}
			device := abmclient.Device{
				OrgDevice: abm.OrgDevice{
					Attributes: &abm.OrgDeviceAttributes{
						OrderDateTime:      time.Date(2024, 12, 13, 0, 0, 0, 0, time.UTC),
						OrderNumber:        "CE-2024-12-13-04-11-12-826",
						PurchaseSourceType: abm.OrgDeviceAttributesPurchaseSourceType("MANUALLY_ADDED"),
					},
				},
			}
			asset := snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)}}
			e.applyFieldMapping(context.Background(), &asset, device, nil)

			if asset.OrderNumber != tt.wantOrderNumber {
				t.Errorf("OrderNumber = %q, want %q", asset.OrderNumber, tt.wantOrderNumber)
			}
			_, gotDateInCF := asset.CustomFields["purchase_date"]
			if gotDateInCF != tt.wantDateInCF {
				t.Errorf("purchase_date in CustomFields = %v, want %v", gotDateInCF, tt.wantDateInCF)
			}
		})
	}
}

// TestStripOrderInfoOnUpdate covers the preserve_order_info_on_update flag:
// when enabled and the existing Snipe-IT asset already has order info, the
// desired update must drop those fields so we don't overwrite manually
// corrected data.
func TestStripOrderInfoOnUpdate(t *testing.T) {
	tests := []struct {
		name          string
		preserve      bool
		existingOrder string
		existingDate  *snipeit.SnipeTime
		desiredOrder  string
		desiredDate   string
		wantOrder     string
		wantDateInCF  bool
	}{
		{
			name:          "preserve off — overwrites everything",
			preserve:      false,
			existingOrder: "OLD",
			existingDate:  &snipeit.SnipeTime{Time: time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC)},
			desiredOrder:  "NEW",
			desiredDate:   "2024-12-13",
			wantOrder:     "NEW",
			wantDateInCF:  true,
		},
		{
			name:          "preserve on, existing populated — desired cleared",
			preserve:      true,
			existingOrder: "BBY01-806929230921",
			existingDate:  &snipeit.SnipeTime{Time: time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC)},
			desiredOrder:  "CE-2024-12-13-04-11-12-826",
			desiredDate:   "2024-12-13",
			wantOrder:     "",
			wantDateInCF:  false,
		},
		{
			name:          "preserve on, existing empty — desired kept (first sync)",
			preserve:      true,
			existingOrder: "",
			existingDate:  nil,
			desiredOrder:  "NEW",
			desiredDate:   "2024-12-13",
			wantOrder:     "NEW",
			wantDateInCF:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := &snipeit.Asset{CommonFields: snipeit.CommonFields{
				CustomFields: map[string]string{"purchase_date": tt.desiredDate},
			}}
			desired.OrderNumber = tt.desiredOrder
			existing := &snipeit.Asset{}
			existing.OrderNumber = tt.existingOrder
			existing.PurchaseDate = tt.existingDate

			stripOrderInfoOnUpdate(desired, existing, tt.preserve)

			if desired.OrderNumber != tt.wantOrder {
				t.Errorf("OrderNumber = %q, want %q", desired.OrderNumber, tt.wantOrder)
			}
			_, gotDateInCF := desired.CustomFields["purchase_date"]
			if gotDateInCF != tt.wantDateInCF {
				t.Errorf("purchase_date in CustomFields = %v, want %v", gotDateInCF, tt.wantDateInCF)
			}
		})
	}
}

// TestApplyFieldMapping_PurchaseDateWireFormat verifies that purchase_date
// is serialized to JSON as "YYYY-MM-DD" (date-only), not as upstream
// go-snipeit's default SnipeTime "YYYY-MM-DD HH:MM:SS" datetime format
// which Snipe-IT's purchase_date validator rejects.
func TestApplyFieldMapping_PurchaseDateWireFormat(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{"purchase_date": "order_date"},
		},
	}}
	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				OrderDateTime: time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	asset := snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)}}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	body, err := json.Marshal(asset)
	if err != nil {
		t.Fatalf("marshal asset: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal asset body: %v", err)
	}
	got, ok := parsed["purchase_date"]
	if !ok {
		t.Fatalf("purchase_date missing from JSON body: %s", body)
	}
	if got != "2024-06-15" {
		t.Errorf("purchase_date wire format = %q, want %q (date-only)", got, "2024-06-15")
	}
}

func TestApplyFieldMapping_PurchaseSourceID(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{
				"_snipeit_src_id_1": "purchase_source_id",
			},
		},
	}}
	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				PurchaseSourceID: "ABC123",
			},
		},
	}
	asset := snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)}}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	if got := asset.CustomFields["_snipeit_src_id_1"]; got != "ABC123" {
		t.Errorf("purchase_source_id = %q, want ABC123", got)
	}
}

func TestApplyFieldMapping_IsReleasedFalse(t *testing.T) {
	e := &Engine{cfg: &config.Config{Sync: config.SyncConfig{FieldMapping: map[string]string{
		"_snipeit_is_released_1": "is_released",
	}}}}
	device := abmclient.Device{OrgDevice: abm.OrgDevice{Attributes: &abm.OrgDeviceAttributes{SerialNumber: "ACTIVE"}}}
	asset := snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)}}
	e.applyFieldMapping(context.Background(), &asset, device, nil)
	if got := asset.CustomFields["_snipeit_is_released_1"]; got != "false" {
		t.Fatalf("is_released = %q, want false", got)
	}
}

func TestDiffAsset_WarrantyMonthsDiff(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}},
	}
	desired.WarrantyMonths = snipeit.FlexInt(36)

	existing := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}},
	}
	existing.WarrantyMonths = snipeit.FlexInt(24)

	diff := e.diffAsset(desired, existing)
	if diff == nil {
		t.Fatal("diffAsset should detect warranty_months change")
	}
	if diff.WarrantyMonths.Int() != 36 {
		t.Errorf("expected warranty_months 36, got %d", diff.WarrantyMonths.Int())
	}
}

// --- applyFieldMapping tests ---

func TestApplyFieldMapping(t *testing.T) {
	orderDate := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	addedDate := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	releasedDate := time.Date(2025, 7, 2, 12, 0, 0, 0, time.UTC)
	acEnd := time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC)
	acStart := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{
				"_snipeit_color_1":        "color",
				"_snipeit_storage_2":      "device_capacity",
				"_snipeit_mac_3":          "wifi_mac",
				"_snipeit_status_4":       "applecare_status",
				"_snipeit_start_5":        "applecare_start",
				"_snipeit_end_6":          "applecare_end",
				"_snipeit_pay_7":          "applecare_payment_type",
				"_snipeit_renew_8":        "applecare_renewable",
				"_snipeit_server_9":       "assigned_server",
				"_snipeit_added_10":       "added_to_org",
				"_snipeit_released_17":    "released_from_org",
				"_snipeit_is_released_18": "is_released",
				"order_number":            "order_number",
				"purchase_date":           "order_date",
				"_snipeit_model_11":       "device_model",
				"_snipeit_part_12":        "part_number",
				"_snipeit_family_13":      "product_family",
				"_snipeit_type_14":        "product_type",
				"_snipeit_src_15":         "purchase_source",
				"_snipeit_stat_16":        "status",
			},
		},
	}}

	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				Color:                   "SPACE GRAY",
				DeviceCapacity:          "512GB",
				WifiMacAddress:          abm.FlexStringSlice{"AABBCCDDEEFF"},
				OrderNumber:             "CDW/1TESTORD/002",
				OrderDateTime:           orderDate,
				AddedToOrgDateTime:      addedDate,
				ReleasedFromOrgDateTime: releasedDate,
				DeviceModel:             "MacBook Pro (16-inch, 2024)",
				PartNumber:              "TEST1LL/A",
				ProductFamily:           abm.OrgDeviceAttributesProductFamily("Mac"),
				ProductType:             "Mac16,1",
				PurchaseSourceType:      abm.OrgDeviceAttributesPurchaseSourceType("RESELLER"),
				Status:                  abm.OrgDeviceAttributesStatus("ASSIGNED"),
			},
		},
		AssignedServer: "TestMDM",
	}

	acRecord := abmclient.AppleCareCoverage{
		Status:        "ACTIVE",
		StartDateTime: acStart,
		EndDateTime:   acEnd,
		PaymentType:   "PAID_UP_FRONT",
		IsRenewable:   true,
	}
	coverage := &abmclient.CoverageResult{
		Best: &acRecord,
		All:  []abmclient.AppleCareCoverage{acRecord},
	}

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)},
	}
	e.applyFieldMapping(context.Background(), &asset, device, coverage)

	checks := map[string]string{
		"_snipeit_color_1":        "Space Gray",
		"_snipeit_storage_2":      "512",
		"_snipeit_mac_3":          "AA:BB:CC:DD:EE:FF",
		"_snipeit_status_4":       "Active",
		"_snipeit_start_5":        "2024-06-15",
		"_snipeit_end_6":          "2027-06-15",
		"_snipeit_pay_7":          "Paid Up Front",
		"_snipeit_renew_8":        "true",
		"_snipeit_server_9":       "TestMDM",
		"_snipeit_added_10":       "2024-07-01",
		"_snipeit_released_17":    "2025-07-02",
		"_snipeit_is_released_18": "true",
		"_snipeit_model_11":       "MacBook Pro (16-inch, 2024)",
		"_snipeit_part_12":        "TEST1LL/A",
		"_snipeit_family_13":      "Mac",
		"_snipeit_type_14":        "Mac16,1",
		"_snipeit_src_15":         "Reseller",
		"_snipeit_stat_16":        "true",
	}

	for field, want := range checks {
		got := asset.CustomFields[field]
		if got != want {
			t.Errorf("field %q = %q, want %q", field, got, want)
		}
	}

	// order_number routes to the native top-level Asset struct field, not
	// CustomFields, so it lands in Snipe-IT's built-in "Order Information"
	// UI panel.
	if _, inCF := asset.CustomFields["order_number"]; inCF {
		t.Errorf("order_number should not land in CustomFields")
	}
	if asset.OrderNumber != "1TESTORD" {
		t.Errorf("asset.OrderNumber = %q, want %q", asset.OrderNumber, "1TESTORD")
	}

	// purchase_date is held in CustomFields as a workaround for upstream
	// SnipeTime.MarshalJSON (see sync.go). Snipe-IT still routes the key
	// to its native purchase_date column.
	if got := asset.CustomFields["purchase_date"]; got != "2024-06-15" {
		t.Errorf("CustomFields[purchase_date] = %q, want 2024-06-15", got)
	}

	// warranty_months auto-calculated
	expectedMonths := int(acEnd.Sub(orderDate).Hours() / (24 * 30))
	if asset.WarrantyMonths.Int() != expectedMonths {
		t.Errorf("warranty_months = %d, want %d", asset.WarrantyMonths.Int(), expectedMonths)
	}
}

func TestApplyFieldMapping_AppleDBFields(t *testing.T) {
	e := &Engine{
		cfg: &config.Config{
			Sync: config.SyncConfig{
				FieldMapping: map[string]string{
					"_snipeit_apple_model_1": "apple_model_number",
					"_snipeit_chip_2":        "chip",
					"_snipeit_year_3":        "model_year",
				},
			},
		},
		// Pre-populate the cache so the test never makes a real network call
		// to appledb.dev; this also exercises appleDBInfoFor's cache-hit path.
		appleDBCache: map[string]*appleDBDeviceInfo{
			"Mac16,10": {
				RegulatoryModel: "A3238",
				Chip:            "M4",
				ReleaseYear:     "2024",
			},
		},
	}

	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				ProductType: "Mac16,10",
			},
		},
	}

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)},
	}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	checks := map[string]string{
		"_snipeit_apple_model_1": "A3238",
		"_snipeit_chip_2":        "M4",
		"_snipeit_year_3":        "2024",
	}
	for field, want := range checks {
		if got := asset.CustomFields[field]; got != want {
			t.Errorf("field %q = %q, want %q", field, got, want)
		}
	}

	// The cache should still contain exactly the one entry we seeded --
	// appleDBInfoFor must not have made a real lookup for a cache hit.
	if len(e.appleDBCache) != 1 {
		t.Errorf("appleDBCache has %d entries, want 1 (no new lookups expected)", len(e.appleDBCache))
	}
}

func TestApplyFieldMapping_AppleDBFields_UnknownProductType(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{
				"_snipeit_apple_model_1": "apple_model_number",
			},
		},
	}}

	// No ProductType set -- appleDBInfoFor should short-circuit on the empty
	// string and never attempt a network call.
	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{},
		},
	}

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)},
	}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	if v, ok := asset.CustomFields["_snipeit_apple_model_1"]; ok {
		t.Errorf("apple_model_number should be empty with no ProductType, got %q", v)
	}
	if e.appleDBCache != nil {
		t.Errorf("appleDBCache should remain nil when ProductType is empty, got %v", e.appleDBCache)
	}
}

func TestApplyFieldMapping_NoAppleCare(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{
				"_snipeit_status_1": "applecare_status",
				"_snipeit_color_2":  "color",
			},
		},
	}}

	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				Color: "SILVER",
			},
		},
	}

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)},
	}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	if v, ok := asset.CustomFields["_snipeit_status_1"]; ok {
		t.Errorf("applecare_status should be empty with nil AC, got %q", v)
	}
	if asset.CustomFields["_snipeit_color_2"] != "Silver" {
		t.Errorf("color = %q, want Silver", asset.CustomFields["_snipeit_color_2"])
	}
}

func TestApplyFieldMapping_StatusUnassigned(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{
				"_snipeit_mdm_1": "status",
			},
		},
	}}

	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				Status: abm.OrgDeviceAttributesStatus("UNASSIGNED"),
			},
		},
	}

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)},
	}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	if asset.CustomFields["_snipeit_mdm_1"] != "false" {
		t.Errorf("status UNASSIGNED should map to 'false', got %q", asset.CustomFields["_snipeit_mdm_1"])
	}
}

func TestApplyFieldMapping_EthernetMAC(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Sync: config.SyncConfig{
			FieldMapping: map[string]string{
				"_snipeit_eth_1": "ethernet_mac",
			},
		},
	}}

	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{
				EthernetMacAddress: []string{"112233445566"},
			},
		},
	}

	asset := snipeit.Asset{
		CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)},
	}
	e.applyFieldMapping(context.Background(), &asset, device, nil)

	if asset.CustomFields["_snipeit_eth_1"] != "11:22:33:44:55:66" {
		t.Errorf("ethernet_mac = %q, want 11:22:33:44:55:66", asset.CustomFields["_snipeit_eth_1"])
	}
}

// --- ensureModel tests ---

func TestEnsureModel_MatchByProductType(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{},
		models: map[string]int{"Mac16,10": 42},
	}
	attrs := &abm.OrgDeviceAttributes{
		ProductType: "Mac16,10",
		DeviceModel: "Mac mini (2024)",
	}
	id, err := e.ensureModel(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("expected model ID 42, got %d", id)
	}
}

func TestEnsureModel_MatchByDeviceModel(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{},
		models: map[string]int{"Mac mini (2024)": 99},
	}
	attrs := &abm.OrgDeviceAttributes{
		ProductType: "Mac16,10",
		DeviceModel: "Mac mini (2024)",
	}
	id, err := e.ensureModel(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 99 {
		t.Errorf("expected model ID 99, got %d", id)
	}
}

func TestEnsureModel_MatchByPartNumber(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{},
		models: map[string]int{"MW0Y3LL/A": 55},
	}
	attrs := &abm.OrgDeviceAttributes{
		ProductType: "Mac16,10",
		DeviceModel: "Mac mini (2024)",
		PartNumber:  "MW0Y3LL/A",
	}
	id, err := e.ensureModel(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 55 {
		t.Errorf("expected model ID 55, got %d", id)
	}
}

func TestEnsureModel_NoIdentifier(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{},
		models: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{}
	_, err := e.ensureModel(context.Background(), attrs)
	if err == nil {
		t.Error("expected error for device with no model identifier")
	}
}

func TestEnsureModel_UpdateOnlyMode(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{Sync: config.SyncConfig{UpdateOnly: true}},
		models: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{
		ProductType: "Mac16,10",
		DeviceModel: "Mac mini (2024)",
	}
	_, err := e.ensureModel(context.Background(), attrs)
	if err == nil {
		t.Error("expected error in update_only mode when model not found")
	}
}

func TestEnsureModel_DryRunMode(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{Sync: config.SyncConfig{DryRun: true}},
		models: map[string]int{},
		stats:  Stats{},
	}
	attrs := &abm.OrgDeviceAttributes{
		ProductType: "Mac16,10",
		DeviceModel: "Mac mini (2024)",
	}
	id, err := e.ensureModel(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Errorf("dry run should return ID 0, got %d", id)
	}
	if e.stats.ModelNew != 1 {
		t.Errorf("dry run should increment ModelNew, got %d", e.stats.ModelNew)
	}
}

// --- ensureSupplier tests ---

func TestEnsureSupplier_MappingByID(t *testing.T) {
	e := &Engine{
		cfg: &config.Config{Sync: config.SyncConfig{
			SupplierMapping: map[string]int{"1C71B60": 10},
		}},
		suppliers: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{
		PurchaseSourceType: "RESELLER",
		PurchaseSourceID:   "1C71B60",
	}
	id, err := e.ensureSupplier(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 10 {
		t.Errorf("expected supplier ID 10, got %d", id)
	}
}

func TestEnsureSupplier_MappingByType(t *testing.T) {
	e := &Engine{
		cfg: &config.Config{Sync: config.SyncConfig{
			SupplierMapping: map[string]int{"APPLE": 5},
		}},
		suppliers: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{
		PurchaseSourceType: "APPLE",
	}
	id, err := e.ensureSupplier(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 5 {
		t.Errorf("expected supplier ID 5, got %d", id)
	}
}

func TestEnsureSupplier_AppleResolvesName(t *testing.T) {
	e := &Engine{
		cfg:       &config.Config{},
		suppliers: map[string]int{"apple": 7},
	}
	attrs := &abm.OrgDeviceAttributes{
		PurchaseSourceType: "APPLE",
	}
	id, err := e.ensureSupplier(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 7 {
		t.Errorf("expected supplier ID 7, got %d", id)
	}
}

func TestEnsureSupplier_ManuallyAddedSkipped(t *testing.T) {
	e := &Engine{
		cfg:       &config.Config{},
		suppliers: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{
		PurchaseSourceType: "MANUALLY_ADDED",
	}
	id, err := e.ensureSupplier(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Errorf("MANUALLY_ADDED should return 0, got %d", id)
	}
}

func TestEnsureSupplier_EmptySource(t *testing.T) {
	e := &Engine{
		cfg:       &config.Config{},
		suppliers: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{}
	id, err := e.ensureSupplier(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Errorf("empty source should return 0, got %d", id)
	}
}

func TestEnsureSupplier_DryRunMode(t *testing.T) {
	e := &Engine{
		cfg:       &config.Config{Sync: config.SyncConfig{DryRun: true}},
		suppliers: map[string]int{},
	}
	attrs := &abm.OrgDeviceAttributes{
		PurchaseSourceType: "RESELLER",
	}
	id, err := e.ensureSupplier(context.Background(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Errorf("dry run should return 0, got %d", id)
	}
}

// --- processDevice skip conditions ---

func TestProcessDevice_NilAttributes(t *testing.T) {
	e := &Engine{cfg: &config.Config{}, stats: Stats{}}
	device := abmclient.Device{OrgDevice: abm.OrgDevice{}}
	err := e.processDevice(context.Background(), device)
	if err != nil {
		t.Fatal(err)
	}
	if e.stats.Skipped != 1 {
		t.Errorf("nil attrs should skip, got skipped=%d", e.stats.Skipped)
	}
}

func TestProcessDevice_EmptySerial(t *testing.T) {
	e := &Engine{cfg: &config.Config{}, stats: Stats{}}
	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{SerialNumber: ""},
		},
	}
	err := e.processDevice(context.Background(), device)
	if err != nil {
		t.Fatal(err)
	}
	if e.stats.Skipped != 1 {
		t.Errorf("empty serial should skip, got skipped=%d", e.stats.Skipped)
	}
}

func TestProcessDevice_NotAvailableSerial(t *testing.T) {
	e := &Engine{cfg: &config.Config{}, stats: Stats{}}
	device := abmclient.Device{
		OrgDevice: abm.OrgDevice{
			Attributes: &abm.OrgDeviceAttributes{SerialNumber: "Not Available"},
		},
	}
	err := e.processDevice(context.Background(), device)
	if err != nil {
		t.Fatal(err)
	}
	if e.stats.Skipped != 1 {
		t.Errorf("'Not Available' serial should skip, got skipped=%d", e.stats.Skipped)
	}
}

// --- lookupExistingAsset tests ---
//
// loadAssets/lookupExistingAsset replace the old one-GetAssetBySerial-call-
// per-device pattern with a single paginated preload, since Snipe-IT's API
// throttle (60-120 req/min) made thousands of individual lookups on a full
// sync run a much bigger rate-limit risk than a handful of list calls.

func TestLookupExistingAsset_UsesPreloadedIndex(t *testing.T) {
	e := &Engine{
		assetsBySerial: map[string][]snipeit.Asset{
			"testserial1": {{CommonFields: snipeit.CommonFields{ID: 42}, Serial: "TESTSERIAL1"}},
		},
	}
	// e.snipe is deliberately left nil: if lookupExistingAsset fell through
	// to the live API path instead of using the preloaded index, this would
	// panic rather than succeed.
	resp, err := e.lookupExistingAsset(context.Background(), "TestSerial1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Rows) != 1 || resp.Rows[0].ID != 42 {
		t.Errorf("unexpected result: %+v", resp)
	}
}

func TestLookupExistingAsset_IndexMissChecksLiveAPI(t *testing.T) {
	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		resp := map[string]any{
			"total": 1,
			"rows": []map[string]any{
				{"id": 77, "name": "Archived Asset", "serial": "ARCHIVED1"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	snipeClient, err := snipe.NewClient(srv.URL, "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{snipe: snipeClient, assetsBySerial: map[string][]snipeit.Asset{}}
	resp, err := e.lookupExistingAsset(context.Background(), "ARCHIVED1")
	if err != nil {
		t.Fatal(err)
	}
	if !called || resp.Total != 1 || len(resp.Rows) != 1 || resp.Rows[0].ID != 77 {
		t.Errorf("expected archived asset from live fallback, got %+v", resp)
	}
}

func TestLookupExistingAsset_FallsBackWhenNotPreloaded(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"total": 1,
			"rows": []map[string]any{
				{"id": 99, "name": "Fallback Asset", "serial": "FALLBACK1"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	snipeClient, err := snipe.NewClient(srv.URL, "test-key", false)
	if err != nil {
		t.Fatal(err)
	}

	// assetsBySerial deliberately left nil, simulating RunSingle, which
	// never calls loadAssets.
	e := &Engine{snipe: snipeClient}
	resp, err := e.lookupExistingAsset(context.Background(), "FALLBACK1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Rows) != 1 || resp.Rows[0].ID != 99 {
		t.Errorf("unexpected fallback result: %+v", resp)
	}
}

// --- Cache round-trip tests ---

func TestCacheRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	devices := []abmclient.Device{
		makeDevice("D1", "SN001", "Mac", "Mac16,10"),
		makeDevice("D2", "SN002", "Mac", "Mac14,7"),
	}

	acRecord := abmclient.AppleCareCoverage{Status: "ACTIVE", Description: "AppleCare+ for Mac"}
	appleCare := map[string]*abmclient.CoverageResult{
		"D1": {
			Best: &acRecord,
			All:  []abmclient.AppleCareCoverage{acRecord},
		},
	}

	// Write
	if err := writeJSON(tmpDir, "devices.json", devices); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(tmpDir, "applecare.json", appleCare); err != nil {
		t.Fatal(err)
	}

	// Read back
	e := &Engine{cfg: &config.Config{Sync: config.SyncConfig{CacheDir: tmpDir}}}
	if err := e.LoadCache(); err != nil {
		t.Fatal(err)
	}

	if len(e.cache.Devices) != 2 {
		t.Errorf("expected 2 devices, got %d", len(e.cache.Devices))
	}
	if len(e.cache.AppleCare) != 1 {
		t.Errorf("expected 1 AppleCare entry, got %d", len(e.cache.AppleCare))
	}
	if e.cache.Devices[0].Attributes.SerialNumber != "SN001" {
		t.Errorf("device serial = %q, want SN001", e.cache.Devices[0].Attributes.SerialNumber)
	}
	if e.cache.AppleCare["D1"].Best.Status != "ACTIVE" {
		t.Errorf("AppleCare status = %q, want ACTIVE", e.cache.AppleCare["D1"].Best.Status)
	}
}

func TestLoadCache_MissingAppleCare(t *testing.T) {
	tmpDir := t.TempDir()

	devices := []abmclient.Device{makeDevice("D1", "SN001", "Mac", "Mac16,10")}
	if err := writeJSON(tmpDir, "devices.json", devices); err != nil {
		t.Fatal(err)
	}

	// No applecare.json — should still work
	e := &Engine{cfg: &config.Config{Sync: config.SyncConfig{CacheDir: tmpDir}}}
	if err := e.LoadCache(); err != nil {
		t.Fatal(err)
	}
	if len(e.cache.Devices) != 1 {
		t.Errorf("expected 1 device, got %d", len(e.cache.Devices))
	}
}

// --- Incremental AppleCare tests ---

func TestFilterDevicesNeedingAppleCare_FirstRunFetchesAll(t *testing.T) {
	devices := []abmclient.Device{
		makeDevice("D1", "SN001", "Mac", "Mac16,10"),
		makeDevice("D2", "SN002", "Mac", "Mac14,7"),
	}
	got := filterDevicesNeedingAppleCare(devices, map[string]*abmclient.CoverageResult{}, false)
	if len(got) != 2 {
		t.Errorf("expected all %d devices fetched on first run (empty cache), got %d", len(devices), len(got))
	}
}

func TestFilterDevicesNeedingAppleCare_SkipsAlreadyCached(t *testing.T) {
	devices := []abmclient.Device{
		makeDevice("D1", "SN001", "Mac", "Mac16,10"),
		makeDevice("D2", "SN002", "Mac", "Mac14,7"),
	}
	existing := map[string]*abmclient.CoverageResult{
		"D1": {},
	}
	got := filterDevicesNeedingAppleCare(devices, existing, false)
	if len(got) != 1 || got[0].ID != "D2" {
		t.Errorf("expected only D2 (new device) to need fetching, got %+v", got)
	}
}

func TestFilterDevicesNeedingAppleCare_NoneNeeded(t *testing.T) {
	devices := []abmclient.Device{makeDevice("D1", "SN001", "Mac", "Mac16,10")}
	existing := map[string]*abmclient.CoverageResult{"D1": {}}
	got := filterDevicesNeedingAppleCare(devices, existing, false)
	if len(got) != 0 {
		t.Errorf("expected no devices to need fetching when all are cached, got %+v", got)
	}
}

func TestFilterDevicesNeedingAppleCare_FullRefreshIgnoresCache(t *testing.T) {
	devices := []abmclient.Device{
		makeDevice("D1", "SN001", "Mac", "Mac16,10"),
		makeDevice("D2", "SN002", "Mac", "Mac14,7"),
	}
	existing := map[string]*abmclient.CoverageResult{
		"D1": {},
		"D2": {},
	}
	got := filterDevicesNeedingAppleCare(devices, existing, true)
	if len(got) != 2 {
		t.Errorf("expected --applecare-full to re-fetch all %d devices despite full cache, got %d", len(devices), len(got))
	}
}

// TestFetchAndSaveAppleCare_SkipsABMWhenAllDevicesCached is a regression test
// for the incremental AppleCare fetch: if every device already has a cached
// AppleCare entry, FetchAndSaveAppleCare must not call out to ABM at all. It
// verifies this by leaving e.abm nil -- fetchAppleCareParallel would panic on
// a nil ABM client if it were invoked, so a clean pass proves the ABM call
// was skipped entirely.
func TestFetchAndSaveAppleCare_SkipsABMWhenAllDevicesCached(t *testing.T) {
	tmpDir := t.TempDir()

	devices := []abmclient.Device{
		makeDevice("D1", "SN001", "Mac", "Mac16,10"),
		makeDevice("D2", "SN002", "Mac", "Mac14,7"),
	}
	acRecord := abmclient.AppleCareCoverage{Status: "ACTIVE", Description: "AppleCare+ for Mac"}
	existing := map[string]*abmclient.CoverageResult{
		"D1": {Best: &acRecord, All: []abmclient.AppleCareCoverage{acRecord}},
		"D2": {Best: &acRecord, All: []abmclient.AppleCareCoverage{acRecord}},
	}
	if err := writeJSON(tmpDir, "devices.json", devices); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(tmpDir, "applecare.json", existing); err != nil {
		t.Fatal(err)
	}

	e := &Engine{cfg: &config.Config{Sync: config.SyncConfig{CacheDir: tmpDir}}} // e.abm is nil
	if err := e.FetchAndSaveAppleCare(context.Background(), devices); err != nil {
		t.Fatalf("FetchAndSaveAppleCare returned error: %v (should have skipped ABM entirely)", err)
	}

	e2 := &Engine{cfg: &config.Config{Sync: config.SyncConfig{CacheDir: tmpDir}}}
	if err := e2.LoadCache(); err != nil {
		t.Fatal(err)
	}
	if len(e2.cache.AppleCare) != 2 {
		t.Errorf("expected the 2 pre-cached AppleCare records to survive untouched, got %d", len(e2.cache.AppleCare))
	}
}

func TestCacheDir_Default(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	if e.CacheDir() != ".cache" {
		t.Errorf("default CacheDir = %q, want .cache", e.CacheDir())
	}
}

func TestCacheDir_Custom(t *testing.T) {
	e := &Engine{cfg: &config.Config{Sync: config.SyncConfig{CacheDir: "/tmp/test"}}}
	if e.CacheDir() != "/tmp/test" {
		t.Errorf("custom CacheDir = %q, want /tmp/test", e.CacheDir())
	}
}

// --- formatAssetDiff tests ---

func TestFormatAssetDiff(t *testing.T) {
	a := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: map[string]string{
				"_snipeit_color_1": "Silver",
			},
		},
	}
	a.Supplier = snipeit.Supplier{CommonFields: snipeit.CommonFields{ID: 5}}
	a.WarrantyMonths = snipeit.FlexInt(36)

	m := formatAssetDiff(a)
	if m["supplier_id"] != 5 {
		t.Errorf("supplier_id = %v, want 5", m["supplier_id"])
	}
	if m["warranty_months"] != 36 {
		t.Errorf("warranty_months = %v, want 36", m["warranty_months"])
	}
	if m["_snipeit_color_1"] != "Silver" {
		t.Errorf("_snipeit_color_1 = %v, want Silver", m["_snipeit_color_1"])
	}
}

// --- applyWarrantyNotes tests ---

func TestApplyWarrantyNotes_PreservesManualNotes(t *testing.T) {
	ac := abmclient.AppleCareCoverage{
		Status:          "ACTIVE",
		Description:     "AppleCare+ for Mac",
		AgreementNumber: "123456",
		PaymentType:     "PAID_UP_FRONT",
	}
	coverage := &abmclient.CoverageResult{
		Best: &ac,
		All:  []abmclient.AppleCareCoverage{ac},
	}

	// First apply: no existing notes
	asset := &snipeit.Asset{}
	applyWarrantyNotes(asset, coverage, true)
	if !strings.Contains(asset.Notes, warrantyNotesStart) {
		t.Error("expected warranty sentinel start in notes")
	}
	if !strings.Contains(asset.Notes, "AppleCare+ for Mac") {
		t.Error("expected coverage description in notes")
	}

	// Second apply: manual notes before and after existing sentinel block
	asset.Notes = "Manual note before.\n\n" + asset.Notes + "\n\nManual note after."
	applyWarrantyNotes(asset, coverage, true)

	if !strings.HasPrefix(asset.Notes, "Manual note before.") {
		t.Errorf("manual prefix lost; notes = %q", asset.Notes)
	}
	if !strings.Contains(asset.Notes, "Manual note after.") {
		t.Errorf("manual suffix lost; notes = %q", asset.Notes)
	}
	// Sentinel block should appear exactly once
	if strings.Count(asset.Notes, warrantyNotesStart) != 1 {
		t.Errorf("expected exactly one sentinel start, got %d; notes = %q",
			strings.Count(asset.Notes, warrantyNotesStart), asset.Notes)
	}
}

func TestApplyWarrantyNotes_ReplaceBlockAtStart(t *testing.T) {
	// Block at position 0 (no manual notes before it) — replacement must not
	// produce a leading "\n\n" prefix, and re-applying must be idempotent.
	ac := abmclient.AppleCareCoverage{Status: "ACTIVE", Description: "AppleCare+"}
	coverage := &abmclient.CoverageResult{Best: &ac, All: []abmclient.AppleCareCoverage{ac}}

	asset := &snipeit.Asset{}
	applyWarrantyNotes(asset, coverage, true)
	firstNotes := asset.Notes

	// Simulate a second sync: block already present, nothing before it.
	applyWarrantyNotes(asset, coverage, true)

	if asset.Notes != firstNotes {
		t.Errorf("re-apply changed notes\ngot:  %q\nwant: %q", asset.Notes, firstNotes)
	}
	if strings.HasPrefix(asset.Notes, "\n") {
		t.Errorf("notes must not start with newline; got %q", asset.Notes)
	}
}

func TestApplyWarrantyNotes_NilCoverageRemovesBlock(t *testing.T) {
	// Build notes that contain a sentinel block flanked by manual text.
	existing := "Manual before.\n\n" + warrantyNotesStart + "\n│ Active │ AppleCare+ for Mac │\n" + warrantyNotesEnd + "\n\nManual after."
	asset := &snipeit.Asset{}
	asset.Notes = existing

	applyWarrantyNotes(asset, nil, true)

	if strings.Contains(asset.Notes, warrantyNotesStart) {
		t.Errorf("sentinel block should be removed when coverage is nil; notes = %q", asset.Notes)
	}
	if !strings.Contains(asset.Notes, "Manual before.") {
		t.Errorf("manual prefix lost; notes = %q", asset.Notes)
	}
	if !strings.Contains(asset.Notes, "Manual after.") {
		t.Errorf("manual suffix lost; notes = %q", asset.Notes)
	}
}

func TestApplyWarrantyNotes_NilCoverageNoBlock(t *testing.T) {
	asset := &snipeit.Asset{}
	asset.Notes = "Just a manual note."
	applyWarrantyNotes(asset, nil, true)
	if asset.Notes != "Just a manual note." {
		t.Errorf("notes should be unchanged; got %q", asset.Notes)
	}
}

func TestApplyWarrantyNotes_DisabledRemovesOnlyManagedBlock(t *testing.T) {
	existing := "Manual before.\n\n" + warrantyNotesStart + "\nold warranty data\n" + warrantyNotesEnd + "\n\nManual after."
	asset := &snipeit.Asset{CommonFields: snipeit.CommonFields{Notes: existing}}
	coverage := &abmclient.CoverageResult{All: []abmclient.AppleCareCoverage{{Status: "ACTIVE"}}}

	applyWarrantyNotes(asset, coverage, false)

	if strings.Contains(asset.Notes, warrantyNotesStart) || strings.Contains(asset.Notes, "old warranty data") {
		t.Errorf("managed warranty block was not removed; notes = %q", asset.Notes)
	}
	if asset.Notes != "Manual before.\n\nManual after." {
		t.Errorf("manual notes changed; notes = %q", asset.Notes)
	}
}

func TestDiffAsset_CanClearManagedNotes(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	desired := &snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: map[string]string{}}}
	existing := &snipeit.Asset{CommonFields: snipeit.CommonFields{
		Notes:        warrantyNotesStart + "\nold warranty data\n" + warrantyNotesEnd,
		CustomFields: map[string]string{},
	}}

	diff := e.diffAsset(desired, existing)
	if diff == nil {
		t.Fatal("diffAsset should return a diff when managed notes need to be cleared")
	}
	if diff.Notes != "" {
		t.Errorf("diff.Notes = %q, want empty", diff.Notes)
	}
	payload, err := json.Marshal(diff)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"notes":""`) {
		t.Errorf("clearing payload does not contain an explicit empty notes value: %s", payload)
	}
}

func TestFormatAssetDiff_IncludesNotes(t *testing.T) {
	a := &snipeit.Asset{
		CommonFields: snipeit.CommonFields{
			CustomFields: make(map[string]string),
		},
	}
	a.Notes = "some notes"
	m := formatAssetDiff(a)
	if m["notes"] != "some notes" {
		t.Errorf("notes = %v, want %q", m["notes"], "some notes")
	}

	// Notes absent when empty
	a2 := &snipeit.Asset{CommonFields: snipeit.CommonFields{CustomFields: make(map[string]string)}}
	m2 := formatAssetDiff(a2)
	if _, ok := m2["notes"]; ok {
		t.Error("notes key should be absent when empty")
	}
}

// --- helpers ---

func makeDevice(id, serial, family, productType string) abmclient.Device {
	return abmclient.Device{
		OrgDevice: abm.OrgDevice{
			ID: id,
			Attributes: &abm.OrgDeviceAttributes{
				SerialNumber:  serial,
				ProductFamily: abm.OrgDeviceAttributesProductFamily(family),
				ProductType:   productType,
				DeviceModel:   "Test Device",
				Color:         "SILVER",
			},
		},
	}
}
