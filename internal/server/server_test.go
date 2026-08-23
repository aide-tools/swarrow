package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunValidatesConfigurationBeforeExternalConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarrow.yaml")
	if err := os.WriteFile(path, []byte("version: 0\n"), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}

	err := Run(context.Background(), path, "dev", discardLogger())

	if err == nil || !strings.Contains(err.Error(), "validate configuration") {
		t.Fatalf("Run() error = %v, want configuration validation error", err)
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	if err := Run(nil, "config.yaml", "dev", discardLogger()); !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("Run() error = %v, want ErrInvalidConfiguration", err)
	}
}

func TestServeShutsDownWhenTheContextEnds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	httpServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- serve(ctx, httpServer, listener) }()

	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("GET server: %v", err)
	}
	response.Body.Close()
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Errorf("serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve() did not stop after cancellation")
	}
}

func TestDurationWithGraceDoesNotOverflow(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	if got := durationWithGrace(maxDuration, time.Second); got != maxDuration {
		t.Errorf("durationWithGrace() = %v, want maximum duration", got)
	}
	if got := durationWithGrace(time.Minute, time.Second); got != 61*time.Second {
		t.Errorf("durationWithGrace() = %v, want 61s", got)
	}
}

func TestRestartReadinessLogsWarmupAndStopsWithServer(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())

	stop := startRestartReadinessLog(ctx, logger, now.Add(time.Minute), now)
	cancel()
	stop()

	logOutput := output.String()
	if !strings.Contains(logOutput, `"msg":"deployment authentication warming up"`) ||
		!strings.Contains(logOutput, `"ready_at":"2026-08-16T12:01:00Z"`) {
		t.Errorf("logs = %q, want warm-up warning with ready time", logOutput)
	}
	if strings.Contains(logOutput, `"msg":"deployment authentication ready"`) {
		t.Errorf("logs = %q, do not want readiness after shutdown", logOutput)
	}
}

func TestRestartReadinessLogsWhenAlreadyReady(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	now := time.Date(2026, time.August, 16, 12, 1, 0, 0, time.UTC)

	stop := startRestartReadinessLog(context.Background(), logger, now, now)
	stop()

	if !strings.Contains(output.String(), `"msg":"deployment authentication ready"`) {
		t.Errorf("logs = %q, want readiness message", output.String())
	}
}

func TestRestartReadinessLogsReadyTransition(t *testing.T) {
	var output lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	now := time.Now()

	stop := startRestartReadinessLog(context.Background(), logger, now.Add(10*time.Millisecond), now)
	defer stop()

	deadline := time.Now().Add(time.Second)
	for !strings.Contains(output.String(), `"msg":"deployment authentication ready"`) {
		if time.Now().After(deadline) {
			t.Fatalf("logs = %q, want readiness transition", output.String())
		}
		time.Sleep(time.Millisecond)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
