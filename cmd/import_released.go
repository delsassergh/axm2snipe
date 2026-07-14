package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	axmsync "github.com/CampusTech/axm2snipe/sync"
)

// NewImportReleasedCmd creates the historical released-device bootstrap command.
func NewImportReleasedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import-released --csv <Apple device export.csv>",
		Short: "Import historical released devices from an Apple CSV export",
		Long:  "Uses an Apple Business Manager device CSV export to discover historical released serials, fetches canonical device details from Apple's single-device API, and saves them in a persistent cache that future downloads preserve.",
		Args:  cobra.NoArgs,
		RunE:  runImportReleased,
	}
	cmd.Flags().String("csv", "", "Path to an Apple Business Manager device export CSV")
	_ = cmd.MarkFlagRequired("csv")
	return cmd
}

func runImportReleased(cmd *cobra.Command, args []string) error {
	if err := Cfg.ValidateABM(); err != nil {
		return err
	}
	csvPath, _ := cmd.Flags().GetString("csv")
	ctx, cancel := contextWithSignal()
	defer cancel()

	abmClient, err := newABMClient(ctx)
	if err != nil {
		return err
	}
	engine := axmsync.NewDownloadEngine(abmClient, Cfg)
	stats, importErr := engine.ImportReleasedDevicesCSV(ctx, csvPath)
	if stats != nil {
		fmt.Printf("\nHistorical Release Import:\n")
		fmt.Printf("  Released devices in CSV: %d\n", stats.CSVReleased)
		fmt.Printf("  Already cached:          %d\n", stats.AlreadyKnown)
		fmt.Printf("  Fetched this run:        %d\n", stats.Fetched)
		fmt.Printf("  Failed this run:         %d\n", stats.Failed)
		fmt.Printf("  Persistent cache total:  %d\n", stats.CachedTotal)
	}
	return importErr
}
