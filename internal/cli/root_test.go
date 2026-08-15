package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aide-tools/swarrow/internal/cli"
)

func TestRootCommandShowsHelp(t *testing.T) {
	command := cli.NewRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{})

	err := command.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}

	for _, expected := range []string{
		"Securely deploy Docker images to Docker Swarm",
		"Usage:",
		"swarrow [flags]",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("help output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestRootCommandRejectsArguments(t *testing.T) {
	command := cli.NewRootCommand()
	command.SetArgs([]string{"unexpected"})

	err := command.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext() error = nil, want an argument error")
	}

	if got, want := err.Error(), `unknown command "unexpected" for "swarrow"`; got != want {
		t.Errorf("ExecuteContext() error = %q, want %q", got, want)
	}
}
