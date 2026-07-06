package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	axmsync "github.com/CampusTech/axm2snipe/sync"
)

// NewBackfillImagesCmd creates the backfill-images command.
func NewBackfillImagesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backfill-images",
		Short: "Attach AppleDB images to existing models that don't have one",
		Long:  "Fetches device images from appledb.dev and attaches them to existing Snipe-IT models under the configured Apple manufacturer that don't already have an image. Use this to fill in images for models created before sync.model_images was enabled, or before older axm2snipe versions' image-attach bug was fixed. Does not touch models that already have an image, or models outside the configured manufacturer_id.",
		RunE:  runBackfillImages,
	}
}

func runBackfillImages(cmd *cobra.Command, args []string) error {
	if err := Cfg.ValidateSnipeIT(); err != nil {
		return err
	}

	snipeClient, err := newSnipeClient()
	if err != nil {
		return err
	}

	engine := axmsync.NewSnipeOnlyEngine(snipeClient, Cfg)

	ctx, cancel := contextWithSignal()
	defer cancel()

	updated, skipped, err := engine.BackfillModelImages(ctx)
	if err != nil {
		return fmt.Errorf("backfill-images failed: %w", err)
	}

	fmt.Printf("\nBackfill Results:\n")
	fmt.Printf("  Models updated: %d\n", updated)
	fmt.Printf("  Models skipped: %d\n", skipped)

	return nil
}
