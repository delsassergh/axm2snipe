package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// NewAssignFieldsetCmd creates the assign-fieldset maintenance command.
func NewAssignFieldsetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assign-fieldset",
		Short: "Assign selected existing models to the configured custom fieldset",
		Long:  "Assigns only the explicitly selected Snipe-IT model IDs to snipe_it.custom_fieldset_id, preserving the models' other attributes.",
		RunE:  runAssignFieldset,
	}
	cmd.Flags().String("model-ids", "", "Comma-separated Snipe-IT model IDs to update (required)")
	_ = cmd.MarkFlagRequired("model-ids")
	return cmd
}

func runAssignFieldset(cmd *cobra.Command, args []string) error {
	if err := Cfg.ValidateSnipeIT(); err != nil {
		return err
	}
	if Cfg.SnipeIT.CustomFieldsetID == 0 {
		return fmt.Errorf("snipe_it.custom_fieldset_id must be set")
	}

	rawIDs, _ := cmd.Flags().GetString("model-ids")
	wanted := make(map[int]bool)
	for _, raw := range strings.Split(rawIDs, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid model ID %q", strings.TrimSpace(raw))
		}
		wanted[id] = true
	}

	ctx, cancel := contextWithSignal()
	defer cancel()
	snipeClient, err := newSnipeClient()
	if err != nil {
		return err
	}
	models, err := snipeClient.ListAllModels(ctx)
	if err != nil {
		return err
	}

	selected := make(map[int]int)
	for i, model := range models {
		if wanted[model.ID] {
			selected[model.ID] = i
		}
	}
	var missing []int
	for id := range wanted {
		if _, ok := selected[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Ints(missing)
		return fmt.Errorf("model IDs not found: %v", missing)
	}

	var ids []int
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	updated := 0
	for _, id := range ids {
		model := models[selected[id]]
		if model.FieldsetID == Cfg.SnipeIT.CustomFieldsetID {
			fmt.Printf("Model %d (%s) already uses fieldset %d\n", model.ID, model.Name, model.FieldsetID)
			continue
		}
		if Cfg.Sync.DryRun {
			fmt.Printf("[DRY RUN] Model %d (%s): fieldset %d -> %d\n", model.ID, model.Name, model.FieldsetID, Cfg.SnipeIT.CustomFieldsetID)
			updated++
			continue
		}
		if _, err := snipeClient.UpdateModelFieldset(ctx, model, Cfg.SnipeIT.CustomFieldsetID); err != nil {
			return fmt.Errorf("updating model %d (%s): %w", model.ID, model.Name, err)
		}
		fmt.Printf("Model %d (%s): fieldset %d -> %d\n", model.ID, model.Name, model.FieldsetID, Cfg.SnipeIT.CustomFieldsetID)
		updated++
	}

	fmt.Printf("\nModels changed: %d\n", updated)
	return nil
}
