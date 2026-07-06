package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CampusTech/axm2snipe/abmclient"
	"github.com/CampusTech/axm2snipe/config"
	"github.com/CampusTech/axm2snipe/snipe"
)

// NewSetupCmd creates the setup command.
func NewSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create AXM custom fields in Snipe-IT",
		Long:  "Creates AXM custom fields in Snipe-IT, associates them with the configured fieldset, and saves the resulting field mappings to the config file.",
		RunE:  runSetup,
	}
	cmd.Flags().Bool("use-cache", true, "Use cached devices.json for purchase source discovery instead of fetching from ABM")
	return cmd
}

func runSetup(cmd *cobra.Command, args []string) error {
	useCache, _ := cmd.Flags().GetBool("use-cache")
	applyBoolFlag(cmd, "use-cache", &Cfg.Sync.UseCache)

	if err := Cfg.ValidateSnipeIT(); err != nil {
		return err
	}
	if Cfg.SnipeIT.CustomFieldsetID == 0 {
		return fmt.Errorf("snipe_it.custom_fieldset_id must be set to use setup")
	}

	if Cfg.Sync.DryRun {
		log.Info("Running in DRY RUN mode - no changes will be made")
	}

	ctx, cancel := contextWithSignal()
	defer cancel()

	// ABM client is only needed when not using cache (for MDM servers + purchase sources)
	var abmClient *abmclient.Client
	if !useCache {
		if err := Cfg.ValidateABM(); err != nil {
			return err
		}
		var err error
		abmClient, err = newABMClient(ctx)
		if err != nil {
			return err
		}
	}

	snipeClient, err := newSnipeClient()
	if err != nil {
		return err
	}

	// Fetch MDM server names from ABM for the Assigned MDM Server field options
	mdmServerField := snipe.FieldDef{Name: "AXM: Assigned MDM Server", Element: "text", Format: "ANY", HelpText: "MDM server assigned in Apple Business/School Manager"}
	if useCache {
		log.Info("Skipping MDM server fetch (--use-cache); Assigned MDM Server field will be a text field")
	} else {
		log.Info("Fetching MDM servers from ABM...")
		mdmServers, err := abmClient.GetMDMServers(ctx)
		if err != nil {
			log.Warnf("Could not fetch MDM servers: %v (Assigned MDM Server field will be a text field)", err)
		} else {
			var names []string
			for _, name := range mdmServers {
				if name != "" {
					names = append(names, name)
				}
			}
			if len(names) > 0 {
				mdmServerField.Element = "listbox"
				mdmServerField.FieldValues = strings.Join(names, "\n")
				log.Infof("Found %d MDM servers: %s", len(names), strings.Join(names, ", "))
			}
		}
	}

	fields := []snipe.FieldDef{
		{Name: "AXM: MDM Assigned?", Element: "text", Format: "BOOLEAN", HelpText: "Whether this device is assigned to an MDM server in ABM/ASM"},
		{Name: "AXM: Added to Org", Element: "text", Format: "DATE", HelpText: "Date device was added to ABM/ASM organization"},
		{Name: "AXM: AppleCare Description", Element: "text", Format: "ANY", HelpText: "AppleCare coverage description"},
		{Name: "AXM: AppleCare Payment Type", Element: "radio", Format: "ANY", HelpText: "AppleCare payment type", FieldValues: "Paid Up Front\nSubscription\nAbe Subscription\nNone"},
		{Name: "AXM: AppleCare Renewable", Element: "listbox", Format: "BOOLEAN", HelpText: "Whether AppleCare coverage is renewable", FieldValues: "true\nfalse"},
		{Name: "AXM: AppleCare Start Date", Element: "text", Format: "DATE", HelpText: "AppleCare coverage start date"},
		{Name: "AXM: AppleCare Status", Element: "radio", Format: "ANY", HelpText: "AppleCare coverage status", FieldValues: "Active\nInactive\nExpired"},
		{Name: "AXM: Apple Model Number", Element: "text", Format: "ANY", HelpText: "Apple's printed regulatory model number (e.g. A3238), from appledb.dev -- distinct from Part Number's SKU and the internal hardware identifier (e.g. Mac16,10)"},
		{Name: "AXM: Chip", Element: "text", Format: "ANY", HelpText: "Chip/SoC name (e.g. M4), from appledb.dev"},
		{Name: "AXM: Model Year", Element: "text", Format: "ANY", HelpText: "Year the model was released (e.g. 2024), from appledb.dev"},
		mdmServerField,
		{Name: "AXM: Bluetooth MAC Address", Element: "text", Format: "MAC", HelpText: "Bluetooth MAC address (colon-separated)"},
		{Name: "AXM: EID", Element: "text", Format: "ANY", HelpText: "eSIM Embedded Identity Document (cellular iPads only)"},
		{Name: "AXM: Ethernet MAC Address", Element: "text", Format: "ANY", HelpText: "Ethernet MAC address(es) (colon-separated, comma-separated if multiple)"},
		{Name: "AXM: IMEI", Element: "text", Format: "ANY", HelpText: "International Mobile Equipment Identity (cellular devices, comma-separated if multiple)"},
		{Name: "AXM: MEID", Element: "text", Format: "ANY", HelpText: "Mobile Equipment Identifier (CDMA devices, comma-separated if multiple)"},
		{Name: "AXM: Part Number", Element: "text", Format: "ANY", HelpText: "Apple part number (e.g. MW0Y3LL/A)"},
		{Name: "AXM: Purchase Source", Element: "radio", Format: "ANY", HelpText: "How the device was acquired in ABM/ASM", FieldValues: "Apple\nReseller\nManually Added"},
		{Name: "AXM: Purchase Source ID", Element: "text", Format: "ANY", HelpText: "Apple Customer Number (for APPLE) or Reseller Number (for RESELLER)"},
		{Name: "AXM: Released from Org", Element: "text", Format: "DATE", HelpText: "Date device was released from ABM/ASM organization"},
		{Name: "AXM: Wi-Fi MAC Address", Element: "text", Format: "MAC", HelpText: "Wi-Fi MAC address (colon-separated)"},
	}

	log.Info("Creating custom fields in Snipe-IT...")
	results, err := snipeClient.SetupFields(Cfg.SnipeIT.CustomFieldsetID, fields)
	if err != nil {
		return fmt.Errorf("setting up fields: %w", err)
	}

	// Map field names to their suggested ABM attribute
	abmAttr := map[string]string{
		"AXM: Added to Org":           "added_to_org",
		"AXM: Released from Org":      "released_from_org",
		"AXM: MDM Assigned?":          "status",
		"AXM: AppleCare Status":       "applecare_status",
		"AXM: Apple Model Number":    "apple_model_number",
		"AXM: Chip":                  "chip",
		"AXM: Model Year":            "model_year",
		"AXM: AppleCare Description":  "applecare_description",
		"AXM: AppleCare Start Date":   "applecare_start",
		"AXM: AppleCare Renewable":    "applecare_renewable",
		"AXM: AppleCare Payment Type": "applecare_payment_type",
		"AXM: Assigned MDM Server":    "assigned_server",
		"AXM: Bluetooth MAC Address":  "bluetooth_mac",
		"AXM: EID":                    "eid",
		"AXM: Ethernet MAC Address":   "ethernet_mac",
		"AXM: IMEI":                   "imei",
		"AXM: MEID":                   "meid",
		"AXM: Part Number":            "part_number",
		"AXM: Purchase Source":        "purchase_source",
		"AXM: Purchase Source ID":     "purchase_source_id",
		"AXM: Wi-Fi MAC Address":      "wifi_mac",
	}

	// Build field mapping: DB column -> ABM attribute
	fieldMapping := make(map[string]string)
	// replaceValues is the set of ABM attribute values managed by setup —
	// existing config entries with these values will be replaced so stale
	// field IDs from a previous run don't accumulate.
	replaceValues := make(map[string]bool)
	for name, dbCol := range results {
		if attr, ok := abmAttr[name]; ok {
			fieldMapping[dbCol] = attr
			replaceValues[attr] = true
		}
	}

	// Native Snipe-IT order info: route ABM's orderDateTime/orderNumber to the
	// built-in purchase_date and order_number fields so they land in Snipe-IT's
	// "Order Information" UI panel instead of as custom fields.
	fieldMapping["purchase_date"] = "order_date"
	fieldMapping["order_number"] = "order_number"
	replaceValues["order_date"] = true
	replaceValues["order_number"] = true

	// Save to config file, replacing any stale mappings for attributes we manage
	if err := config.MergeFieldMapping(ConfigFile, fieldMapping, replaceValues); err != nil {
		log.Warnf("Could not save field mappings to %s: %v", ConfigFile, err)
		fmt.Println("\nAdd these to your settings.yaml field_mapping manually:")
		for dbCol, attr := range fieldMapping {
			fmt.Printf("    %s: %s\n", dbCol, attr)
		}
	} else {
		fmt.Printf("\nField mappings saved to %s\n", ConfigFile)
	}

	fmt.Println("\nCustom fields created and associated with fieldset:")
	for name, dbCol := range results {
		if attr, ok := abmAttr[name]; ok {
			fmt.Printf("  %s: %s -> %s\n", name, dbCol, attr)
		} else {
			fmt.Printf("  %s: %s\n", name, dbCol)
		}
	}

	// Fetch purchase sources and write supplier_mapping scaffold
	var purchaseSources []abmclient.PurchaseSource
	cacheDir := Cfg.Sync.CacheDir
	if cacheDir == "" {
		cacheDir = ".cache"
	}
	if useCache {
		log.Infof("Loading purchase sources from cache (%s/devices.json)...", cacheDir)
		purchaseSources, err = abmclient.GetPurchaseSourcesFromCache(cacheDir)
		if err != nil {
			log.Warnf("Could not load purchase sources from cache: %v", err)
		}
	} else {
		log.Info("Fetching purchase sources from ABM (this fetches all devices)...")
		purchaseSources, err = abmClient.GetAllPurchaseSources(ctx)
		if err != nil {
			log.Warnf("Could not fetch purchase sources: %v", err)
		}
	}
	if len(purchaseSources) > 0 {
		var entries []config.SupplierEntry
		for _, ps := range purchaseSources {
			if ps.Type == "MANUALLY_ADDED" {
				continue // no supplier to map for manually added devices
			}
			if ps.ID != "" {
				entries = append(entries, config.SupplierEntry{
					Key:     ps.ID,
					Comment: fmt.Sprintf("%s (id: %s)", ps.Type, ps.ID),
				})
			} else {
				entries = append(entries, config.SupplierEntry{
					Key:     ps.Type,
					Comment: ps.Type,
				})
			}
		}

		if len(entries) > 0 {
			if Cfg.Sync.DryRun {
				fmt.Println("\nDRY RUN - no changes will be made. Add these to your settings.yaml supplier_mapping manually:")
				for _, e := range entries {
					fmt.Printf("    # %s\n", e.Comment)
					fmt.Printf("    %s: 0  # TODO: set Snipe-IT supplier ID\n", e.Key)
				}
			} else if err := config.MergeSupplierMapping(ConfigFile, entries); err != nil {
				log.Warnf("Could not save supplier mappings to %s: %v", ConfigFile, err)
				fmt.Println("\nAdd these to your settings.yaml supplier_mapping manually:")
				for _, e := range entries {
					fmt.Printf("    # %s\n", e.Comment)
					fmt.Printf("    %s: 0  # TODO: set Snipe-IT supplier ID\n", e.Key)
				}
			} else {
				fmt.Printf("\nSupplier mapping scaffold saved to %s (set the Snipe-IT supplier IDs)\n", ConfigFile)
			}

			fmt.Println("\nPurchase sources found:")
			for _, e := range entries {
				fmt.Printf("  %s: %s\n", e.Key, e.Comment)
			}
		}
	}

	return nil
}
