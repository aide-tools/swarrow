package cli

import (
	"github.com/aide-tools/swarrow/internal/server"
	"github.com/spf13/cobra"
)

// NewRootCommand creates the top-level Swarrow command.
func NewRootCommand(version string) *cobra.Command {
	command := &cobra.Command{
		Use:           "swarrow",
		Short:         "Securely deploy Docker images to Docker Swarm",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}

	command.CompletionOptions.DisableDefaultCmd = true
	command.SetVersionTemplate("swarrow {{.Version}}\n")
	command.AddCommand(newServeCommand(version, server.Run))
	command.AddCommand(newVersionCommand(version))

	return command
}
