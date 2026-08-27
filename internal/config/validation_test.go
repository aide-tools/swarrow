package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/config"
)

func TestValidate(t *testing.T) {
	configuration := validConfiguration()

	if err := config.Validate(configuration); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateReportsInvalidFields(t *testing.T) {
	configuration := config.Config{
		Version: 2,
		Server: config.Server{
			Listen: "8080",
		},
		Deployments: []config.Deployment{
			{
				Identity: config.Identity{
					RepositoryID: "repository-id",
					Repository:   " example/example-web",
				},
				Target: config.Target{
					Image: "ghcr.io/example/example-web:latest",
				},
			},
		},
	}

	err := config.Validate(configuration)
	if err == nil {
		t.Fatal("Validate() error = nil, want validation errors")
	}

	for _, expected := range []string{
		"version: must be 1",
		"server.listen: must be a host and port",
		"server.request_timeout: must be a positive duration",
		"github.audience: must not be empty",
		"deployments[0].name: must not be empty",
		"deployments[0].identity.repository_id: must be a positive decimal GitHub repository ID",
		"deployments[0].identity.repository: must not have leading or trailing whitespace",
		"deployments[0].identity.workflow_ref: must not be empty",
		"deployments[0].identity.environment: must not be empty",
		"deployments[0].target.service: must not be empty",
		"deployments[0].target.image: must not include a tag",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("Validate() error = %q, want %q", err, expected)
		}
	}
}

func TestValidateRequiresDeployment(t *testing.T) {
	configuration := validConfiguration()
	configuration.Deployments = nil

	err := config.Validate(configuration)
	if err == nil {
		t.Fatal("Validate() error = nil, want a deployment error")
	}

	if !strings.Contains(err.Error(), "deployments: must contain at least one deployment") {
		t.Errorf("Validate() error = %q, want a deployment error", err)
	}
}

func TestValidateRejectsWhitespaceInOptionalJobWorkflowRefs(t *testing.T) {
	tests := map[string]func(*config.Config){
		"global default": func(configuration *config.Config) {
			configuration.GitHub.JobWorkflowRef = " example/swarrow-deploy/.github/workflows/deploy.yml@refs/tags/v1"
		},
		"deployment override": func(configuration *config.Config) {
			configuration.Deployments[0].Identity.JobWorkflowRef = "example/other/.github/workflows/deploy.yml@refs/tags/v1 "
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			configuration := validConfiguration()
			mutate(&configuration)

			err := config.Validate(configuration)
			if err == nil {
				t.Fatal("Validate() error = nil, want a whitespace error")
			}

			if !strings.Contains(err.Error(), "job_workflow_ref: must not have leading or trailing whitespace") {
				t.Errorf("Validate() error = %q, want a job workflow ref whitespace error", err)
			}
		})
	}
}

func TestValidateRejectsInvalidListenPorts(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:http"} {
		t.Run(address, func(t *testing.T) {
			configuration := validConfiguration()
			configuration.Server.Listen = address

			err := config.Validate(configuration)
			if err == nil {
				t.Fatal("Validate() error = nil, want a port error")
			}

			if !strings.Contains(err.Error(), "server.listen: port must be an integer between 1 and 65535") {
				t.Errorf("Validate() error = %q, want a port error", err)
			}
		})
	}
}

func TestValidateRejectsNonPositiveRequestTimeouts(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		configuration := validConfiguration()
		configuration.Server.RequestTimeout = timeout

		err := config.Validate(configuration)
		if err == nil {
			t.Fatal("Validate() error = nil, want a request timeout error")
		}

		if !strings.Contains(err.Error(), "server.request_timeout: must be a positive duration") {
			t.Errorf("Validate() error = %q, want a request timeout error", err)
		}
	}
}

func TestValidateRejectsNonCanonicalRepositoryIDs(t *testing.T) {
	for _, repositoryID := range []string{"0", "0123", "-1", "123.0"} {
		t.Run(repositoryID, func(t *testing.T) {
			configuration := validConfiguration()
			configuration.Deployments[0].Identity.RepositoryID = repositoryID

			err := config.Validate(configuration)
			if err == nil {
				t.Fatal("Validate() error = nil, want a repository ID error")
			}

			if !strings.Contains(err.Error(), "must be a positive decimal GitHub repository ID") {
				t.Errorf("Validate() error = %q, want a repository ID error", err)
			}
		})
	}
}

func TestValidateRejectsDuplicateDeploymentNames(t *testing.T) {
	configuration := validConfiguration()
	duplicate := configuration.Deployments[0]
	duplicate.Target.Service = "example_worker"
	configuration.Deployments = append(configuration.Deployments, duplicate)

	err := config.Validate(configuration)
	if err == nil {
		t.Fatal("Validate() error = nil, want a duplicate name error")
	}

	if !strings.Contains(err.Error(), "deployments[1].name: duplicates deployments[0].name") {
		t.Errorf("Validate() error = %q, want a duplicate name error", err)
	}
}

func TestValidateRejectsDuplicateServiceTargets(t *testing.T) {
	configuration := validConfiguration()
	duplicate := configuration.Deployments[0]
	duplicate.Name = "example-worker"
	configuration.Deployments = append(configuration.Deployments, duplicate)

	err := config.Validate(configuration)
	if err == nil {
		t.Fatal("Validate() error = nil, want a duplicate service error")
	}

	if !strings.Contains(err.Error(), "deployments[1].target.service: duplicates deployments[0].target.service") {
		t.Errorf("Validate() error = %q, want a duplicate service error", err)
	}
}

func TestValidateAllowsRepeatedIdentities(t *testing.T) {
	configuration := validConfiguration()
	repeatedIdentity := configuration.Deployments[0]
	repeatedIdentity.Name = "example-worker"
	repeatedIdentity.Target.Service = "example_worker"
	configuration.Deployments = append(configuration.Deployments, repeatedIdentity)

	if err := config.Validate(configuration); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsImageTagsAndDigests(t *testing.T) {
	for name, image := range map[string]string{
		"tag":    "ghcr.io/example/example-web:latest",
		"digest": "ghcr.io/example/example-web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		t.Run(name, func(t *testing.T) {
			configuration := validConfiguration()
			configuration.Deployments[0].Target.Image = image

			err := config.Validate(configuration)
			if err == nil {
				t.Fatal("Validate() error = nil, want an image repository error")
			}

			if !strings.Contains(err.Error(), "must not include") {
				t.Errorf("Validate() error = %q, want an image repository error", err)
			}
		})
	}
}

func TestValidateRejectsInvalidImageRepositories(t *testing.T) {
	for _, image := range []string{
		"https://ghcr.io/example/example-web",
		"ghcr.io/example/",
		"ghcr.io/Example/example-web",
	} {
		t.Run(image, func(t *testing.T) {
			configuration := validConfiguration()
			configuration.Deployments[0].Target.Image = image

			err := config.Validate(configuration)
			if err == nil {
				t.Fatal("Validate() error = nil, want an image repository error")
			}

			if !strings.Contains(err.Error(), "must be a valid container image repository") {
				t.Errorf("Validate() error = %q, want an image repository error", err)
			}
		})
	}
}

func TestValidateAllowsRegistryPort(t *testing.T) {
	configuration := validConfiguration()
	configuration.Deployments[0].Target.Image = "registry.example.net:5000/example/example-web"

	if err := config.Validate(configuration); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func validConfiguration() config.Config {
	return config.Config{
		Version: 1,
		Server: config.Server{
			Listen:         "127.0.0.1:8080",
			RequestTimeout: 5 * time.Minute,
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
}
