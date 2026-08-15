package cli_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aide-tools/swarrow/internal/cli"
)

func TestVersionOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "flag", args: []string{"--version"}},
		{name: "command", args: []string{"version"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := cli.NewRootCommand("1.2.3")
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs(test.args)

			err := command.ExecuteContext(context.Background())
			if err != nil {
				t.Fatalf("ExecuteContext() error = %v", err)
			}

			if got, want := output.String(), "swarrow 1.2.3\n"; got != want {
				t.Errorf("version output = %q, want %q", got, want)
			}
		})
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	command := cli.NewRootCommand("dev")
	command.SetArgs([]string{"version", "unexpected"})

	err := command.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext() error = nil, want an argument error")
	}

	if got, want := err.Error(), `unknown command "unexpected" for "swarrow version"`; got != want {
		t.Errorf("ExecuteContext() error = %q, want %q", got, want)
	}
}

func TestVersionCommandReturnsOutputError(t *testing.T) {
	want := errors.New("write failed")
	command := cli.NewRootCommand("dev")
	command.SetOut(errorWriter{err: want})
	command.SetArgs([]string{"version"})

	err := command.ExecuteContext(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("ExecuteContext() error = %v, want %v", err, want)
	}
}

type errorWriter struct {
	err error
}

func (writer errorWriter) Write(_ []byte) (int, error) {
	return 0, writer.err
}
