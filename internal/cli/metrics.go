package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runmetrics"
	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
)

func newMetricsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "metrics <run-id>",
		Short: "Export local phase timing and review-value evidence for a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The ID also selects a journal directory; refuse path spellings.
			if _, err := ulid.ParseStrict(args[0]); err != nil {
				return fmt.Errorf("invalid run ID: expected a ULID")
			}
			p, err := paths.New()
			if err != nil {
				return err
			}
			database, err := db.OpenReadOnly(p.DB())
			if err != nil {
				return err
			}
			defer database.Close()
			a, err := runmetrics.Build(database, args[0], p.RunLogDir(args[0]), time.Now())
			if err != nil {
				return fmt.Errorf("read run metrics: %w", err)
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(a)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), runmetrics.Summary(a))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the versioned machine-readable artifact")
	return cmd
}
