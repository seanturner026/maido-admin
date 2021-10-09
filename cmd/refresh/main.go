package refresh

import "github.com/spf13/cobra"

var Command = &cobra.Command{
	Use:          "refresh",
	Short:        "Updates the DynamoDB backend.",
	SilenceUsage: true,
}
