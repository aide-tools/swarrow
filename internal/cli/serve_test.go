package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
)

func TestServeCommandRunsWithExplicitConfiguration(t *testing.T) {
	wantContext := context.WithValue(context.Background(), contextKey{}, "request")
	called := false
	command := newServeCommand("1.2.3", func(ctx context.Context, path string, version string, logger *slog.Logger) error {
		called = true
		if ctx != wantContext || path != "/etc/swarrow/config.yaml" || version != "1.2.3" || logger == nil {
			t.Errorf("run() arguments = (%v, %q, %q, %v)", ctx, path, version, logger)
		}
		return nil
	})
	command.SetArgs([]string{"--config", "/etc/swarrow/config.yaml"})

	if err := command.ExecuteContext(wantContext); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !called {
		t.Fatal("run() was not called")
	}
}

func TestServeCommandRequiresConfiguration(t *testing.T) {
	command := newServeCommand("dev", func(context.Context, string, string, *slog.Logger) error {
		t.Fatal("run() was called")
		return nil
	})
	command.SetArgs([]string{})

	err := command.ExecuteContext(context.Background())

	if err == nil || err.Error() != `required flag "config" not set` {
		t.Errorf("ExecuteContext() error = %v, want required config error", err)
	}
}

func TestServeCommandReturnsRuntimeErrors(t *testing.T) {
	want := errors.New("startup failed")
	command := newServeCommand("dev", func(context.Context, string, string, *slog.Logger) error { return want })
	command.SetArgs([]string{"--config", "config.yaml"})

	err := command.ExecuteContext(context.Background())

	if !errors.Is(err, want) {
		t.Errorf("ExecuteContext() error = %v, want %v", err, want)
	}
}

func TestServeCommandWritesLogsToStandardError(t *testing.T) {
	command := newServeCommand("dev", func(_ context.Context, _ string, _ string, logger *slog.Logger) error {
		logger.Info("started")
		return nil
	})
	var standardError bytes.Buffer
	command.SetErr(&standardError)
	command.SetArgs([]string{"--config", "config.yaml"})

	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !bytes.Contains(standardError.Bytes(), []byte(`"msg":"started"`)) {
		t.Errorf("standard error = %q, want JSON log entry", standardError.String())
	}
}

type contextKey struct{}
