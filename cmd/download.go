package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	axmsync "github.com/CampusTech/axm2snipe/sync"
)

// NewDownloadCmd creates the download command.
func NewDownloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download ABM/ASM data to local cache",
		Long:  "Fetches devices and/or AppleCare coverage from Apple Business Manager / Apple School Manager and saves them as JSON files in the cache directory. Use 'sync --use-cache' to sync from the cache without hitting ABM API rate limits.",
		RunE:  runDownload,
	}
	cmd.Flags().Bool("progress", false, "Show progress bar during AppleCare coverage download")
	cmd.Flags().Bool("devices", false, "Download only the device list (default: both)")
	cmd.Flags().Bool("applecare", false, "Download only AppleCare coverage (uses cached devices if --devices not also set)")
	cmd.Flags().Int("delay", 0, "Seconds to wait between paginated ABM device requests (overrides abm.page_delay_seconds, default 5). Increase this if you're hitting 429 RATE_LIMIT_EXCEEDED errors.")
	cmd.Flags().Int("page-size", 0, "Devices per page when fetching from ABM, max 1000 (overrides abm.page_size, default 100)")
	cmd.Flags().Bool("restart", false, "Ignore any saved device-fetch progress from an interrupted run and start from page one")
	cmd.Flags().Bool("applecare-full", false, "Re-fetch AppleCare coverage for every device instead of only devices missing from the cache (default: incremental). Run this periodically (e.g. weekly) to catch AppleCare Status transitions like Active -> Expired; use the fast default for routine/nightly downloads.")
	return cmd
}

func runDownload(cmd *cobra.Command, args []string) error {
	if err := Cfg.ValidateABM(); err != nil {
		return err
	}

	onlyDevices, _ := cmd.Flags().GetBool("devices")
	onlyAppleCare, _ := cmd.Flags().GetBool("applecare")
	// If neither flag is set, download everything (default behaviour)
	downloadAll := !onlyDevices && !onlyAppleCare

	applyIntFlag(cmd, "delay", &Cfg.ABM.PageDelaySeconds)
	applyIntFlag(cmd, "page-size", &Cfg.ABM.PageSize)

	ctx, cancel := contextWithSignal()
	defer cancel()

	abmClient, err := newABMClient(ctx)
	if err != nil {
		return err
	}

	engine := axmsync.NewDownloadEngine(abmClient, Cfg)
	engine.ShowProgress, _ = cmd.Flags().GetBool("progress")
	engine.AppleCareFullRefresh, _ = cmd.Flags().GetBool("applecare-full")

	if restart, _ := cmd.Flags().GetBool("restart"); restart {
		if err := engine.ResetDeviceProgress(); err != nil {
			return fmt.Errorf("resetting device fetch progress: %w", err)
		}
	}

	switch {
	case downloadAll:
		if err := engine.FetchAndSaveCache(ctx); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
	case onlyDevices && onlyAppleCare:
		if err := engine.FetchAndSaveCache(ctx); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
	case onlyDevices:
		if _, err := engine.FetchAndSaveDevices(ctx); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
	case onlyAppleCare:
		if err := engine.FetchAndSaveAppleCare(ctx, nil); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
	}

	fmt.Printf("ABM data saved to %s/\n", engine.CacheDir())
	return nil
}
