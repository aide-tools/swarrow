package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aide-tools/swarrow/internal/config"
)

func TestDecode(t *testing.T) {
	input := `
version: 1
server:
  listen: 127.0.0.1:8080
github:
  audience: https://deploy.example.net
deployments:
  - name: example-web
    identity:
      repository_id: "123456789"
      repository: example/example-web
      workflow_ref: example/example-web/.github/workflows/deploy.yml@refs/heads/main
      environment: production
    target:
      service: example_web
      image: ghcr.io/example/example-web
`

	got, err := config.Decode(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	want := config.Config{
		Version: 1,
		Server: config.Server{
			Listen: "127.0.0.1:8080",
		},
		GitHub: config.GitHub{
			Audience: "https://deploy.example.net",
		},
		Deployments: []config.Deployment{
			{
				Name: "example-web",
				Identity: config.Identity{
					RepositoryID: "123456789",
					Repository:   "example/example-web",
					WorkflowRef:  "example/example-web/.github/workflows/deploy.yml@refs/heads/main",
					Environment:  "production",
				},
				Target: config.Target{
					Service: "example_web",
					Image:   "ghcr.io/example/example-web",
				},
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Decode() = %#v, want %#v", got, want)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	input := `
version: 1
server:
  listen: 127.0.0.1:8080
  timeout: 30s
`

	_, err := config.Decode(strings.NewReader(input))
	if err == nil {
		t.Fatal("Decode() error = nil, want an unknown field error")
	}

	if !strings.Contains(err.Error(), "field timeout not found in type config.Server") {
		t.Errorf("Decode() error = %q, want an unknown field error", err)
	}
}

func TestDecodeRejectsMultipleDocuments(t *testing.T) {
	input := `
version: 1
---
version: 1
`

	_, err := config.Decode(strings.NewReader(input))
	if err == nil {
		t.Fatal("Decode() error = nil, want a multiple document error")
	}

	if !strings.Contains(err.Error(), "multiple YAML documents are not allowed") {
		t.Errorf("Decode() error = %q, want a multiple document error", err)
	}
}

func TestDecodeRejectsYAMLReferences(t *testing.T) {
	tests := map[string]struct {
		input   string
		message string
	}{
		"alias": {
			input: `
version: &version 1
server:
  listen: *version
`,
			message: "YAML aliases are not allowed",
		},
		"merge key": {
			input: `
version: 1
server:
  <<:
    listen: 127.0.0.1:8080
`,
			message: "YAML merge keys are not allowed",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Decode(strings.NewReader(test.input))
			if err == nil {
				t.Fatalf("Decode() error = nil, want %q", test.message)
			}

			if !strings.Contains(err.Error(), test.message) {
				t.Errorf("Decode() error = %q, want %q", err, test.message)
			}
		})
	}
}

func TestDecodeRejectsInvalidYAML(t *testing.T) {
	_, err := config.Decode(strings.NewReader("version: ["))
	if err == nil {
		t.Fatal("Decode() error = nil, want a YAML error")
	}

	if !strings.Contains(err.Error(), "decode configuration:") {
		t.Errorf("Decode() error = %q, want a wrapped YAML error", err)
	}
}

func TestDecodeRejectsInvalidConfiguration(t *testing.T) {
	_, err := config.Decode(strings.NewReader("version: 2"))
	if err == nil {
		t.Fatal("Decode() error = nil, want a validation error")
	}

	if !strings.Contains(err.Error(), "validate configuration:") {
		t.Errorf("Decode() error = %q, want a validation error", err)
	}
}
