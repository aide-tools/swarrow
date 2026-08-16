package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/spf13/cobra"
)

type serveRunner func(context.Context, string, string, *slog.Logger) error

func newServeCommand(version string, run serveRunner) *cobra.Command {
	var configurationPath string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the deployment service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if configurationPath == "" {
				return errors.New("required flag \"config\" not set")
			}

			logger := slog.New(slog.NewJSONHandler(command.ErrOrStderr(), nil))
			return run(command.Context(), configurationPath, version, logger)
		},
	}

	command.Flags().StringVar(&configurationPath, "config", "", "Path to the configuration file")
	return command
}
