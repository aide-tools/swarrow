// Package server assembles and runs the Swarrow deployment service.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/deploy"
	"github.com/aide-tools/swarrow/internal/githuboidc"
	"github.com/aide-tools/swarrow/internal/httpapi"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/swarm"
	clienttypes "github.com/moby/moby/client"
)

const (
	rolloutPollInterval = time.Second
	readHeaderTimeout   = 5 * time.Second
	readTimeout         = 10 * time.Second
	responseWriteGrace  = 5 * time.Second
	idleTimeout         = 2 * time.Minute
	shutdownTimeout     = 10 * time.Second
	maxHeaderBytes      = 16 << 10
)

var ErrInvalidConfiguration = errors.New("invalid server configuration")

// Run loads configuration, assembles Swarrow and serves until the context ends.
func Run(ctx context.Context, path string, version string, logger *slog.Logger) error {
	if ctx == nil || path == "" || logger == nil {
		return ErrInvalidConfiguration
	}

	configuration, err := loadConfiguration(path)
	if err != nil {
		return err
	}

	verifier, err := githuboidc.New(ctx, githuboidc.Options{Audience: configuration.GitHub.Audience})
	if err != nil {
		return err
	}

	dockerClient, err := clienttypes.New(
		clienttypes.WithHost(clienttypes.DefaultDockerHost),
		clienttypes.WithUserAgent("swarrow/"+version),
	)
	if err != nil {
		return fmt.Errorf("create Docker client: %w", err)
	}
	defer dockerClient.Close()

	observer, err := swarm.NewObserver(dockerClient, rolloutPollInterval)
	if err != nil {
		return fmt.Errorf("create rollout observer: %w", err)
	}
	coordinator, err := deploy.New(ctx, configuration, policy.New(configuration), swarm.NewUpdater(dockerClient), observer)
	if err != nil {
		return fmt.Errorf("create deployment coordinator: %w", err)
	}
	handler, err := httpapi.New(verifier, coordinator, configuration.Server.RequestTimeout, logger)
	if err != nil {
		return fmt.Errorf("create HTTP API: %w", err)
	}

	listener, err := net.Listen("tcp", configuration.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", configuration.Server.Listen, err)
	}

	httpServer := &http.Server{
		Addr:              configuration.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      durationWithGrace(configuration.Server.RequestTimeout, responseWriteGrace),
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	logger.InfoContext(ctx, "server listening", "listen", configuration.Server.Listen)
	return serve(ctx, httpServer, listener)
}

func loadConfiguration(path string) (config.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("open configuration %q: %w", path, err)
	}
	defer file.Close()

	configuration, err := config.Decode(file)
	if err != nil {
		return config.Config{}, err
	}

	return configuration, nil
}

func serve(ctx context.Context, httpServer *http.Server, listener net.Listener) error {
	serveDone := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			shutdownResult <- httpServer.Shutdown(shutdownCtx)
		case <-serveDone:
			shutdownResult <- nil
		}
	}()

	err := httpServer.Serve(listener)
	close(serveDone)
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}

	if shutdownErr := <-shutdownResult; shutdownErr != nil {
		return fmt.Errorf("shut down HTTP server: %w", shutdownErr)
	}

	return nil
}

func durationWithGrace(duration time.Duration, grace time.Duration) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	if duration > maxDuration-grace {
		return maxDuration
	}

	return duration + grace
}
