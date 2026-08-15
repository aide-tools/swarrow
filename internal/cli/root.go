package cli

import "github.com/spf13/cobra"

// NewRootCommand creates the top-level Swarrow command.
func NewRootCommand() *cobra.Command {
	command := &cobra.Command{
		Use:           "swarrow",
		Short:         "Securely deploy Docker images to Docker Swarm",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}

	command.CompletionOptions.DisableDefaultCmd = true

	return command
}
