package policy

import (
	"errors"
	"testing"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/githuboidc"
)

func TestPolicyAuthorise(t *testing.T) {
	t.Parallel()

	policy := New(testConfiguration())
	claims := testClaims()

	deployment, err := policy.Authorise("example-web", claims)
	if err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
	want := (Deployment{
		Name:    "example-web",
		Service: "example_web",
		Image:   "ghcr.io/example/example-web",
	})
	if deployment != want {
		t.Errorf("Authorise() = %#v, want %#v", deployment, want)
	}
}

func TestPolicyIgnoresReadableRepositoryName(t *testing.T) {
	t.Parallel()

	policy := New(testConfiguration())
	claims := testClaims()
	claims.Repository = "renamed/example-web"

	if _, err := policy.Authorise("example-web", claims); err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
}

func TestPolicyAllowsMatchingJobWorkflowRef(t *testing.T) {
	t.Parallel()

	claims := testClaims()
	claims.JobWorkflowRef = claims.WorkflowRef
	claims.JobWorkflowRefPresent = true

	if _, err := New(testConfiguration()).Authorise("example-web", claims); err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
}

func TestPolicyAllowsConfiguredReusableWorkflow(t *testing.T) {
	t.Parallel()

	configuration := testConfiguration()
	configuration.GitHub.JobWorkflowRef = "example/swarrow-deploy/.github/workflows/deploy.yml@refs/tags/v1"
	claims := testClaims()
	claims.JobWorkflowRef = configuration.GitHub.JobWorkflowRef
	claims.JobWorkflowRefPresent = true

	if _, err := New(configuration).Authorise("example-web", claims); err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
}

func TestPolicyDeploymentJobWorkflowRefOverridesDefault(t *testing.T) {
	t.Parallel()

	configuration := testConfiguration()
	configuration.GitHub.JobWorkflowRef = "example/swarrow-deploy/.github/workflows/deploy.yml@refs/tags/v1"
	configuration.Deployments[0].Identity.JobWorkflowRef = "example/other-deploy/.github/workflows/deploy.yml@refs/tags/v2"
	claims := testClaims()
	claims.JobWorkflowRef = configuration.Deployments[0].Identity.JobWorkflowRef
	claims.JobWorkflowRefPresent = true

	if _, err := New(configuration).Authorise("example-web", claims); err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
}

func TestPolicyConfiguredReusableWorkflowRequiresExactPresentClaim(t *testing.T) {
	t.Parallel()

	configuration := testConfiguration()
	configuration.GitHub.JobWorkflowRef = "example/swarrow-deploy/.github/workflows/deploy.yml@refs/tags/v1"

	tests := map[string]func(*githuboidc.Claims){
		"absent": func(claims *githuboidc.Claims) {},
		"different": func(claims *githuboidc.Claims) {
			claims.JobWorkflowRef = "example/other/.github/workflows/deploy.yml@refs/tags/v1"
			claims.JobWorkflowRefPresent = true
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			claims := testClaims()
			mutate(&claims)
			assertDenied(t, New(configuration), "example-web", claims)
		})
	}
}

func TestPolicyAllowsOneIdentitySeveralDeployments(t *testing.T) {
	t.Parallel()

	configuration := testConfiguration()
	second := configuration.Deployments[0]
	second.Name = "example-worker"
	second.Target.Service = "example_worker"
	configuration.Deployments = append(configuration.Deployments, second)
	policy := New(configuration)

	deployment, err := policy.Authorise("example-worker", testClaims())
	if err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
	if deployment.Name != "example-worker" || deployment.Service != "example_worker" {
		t.Errorf("Authorise() = %#v", deployment)
	}
}

func TestPolicyDeniesIdentityMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*githuboidc.Claims)
	}{
		{
			name: "repository ID",
			mutate: func(claims *githuboidc.Claims) {
				claims.RepositoryID = "987654321"
			},
		},
		{
			name: "workflow ref",
			mutate: func(claims *githuboidc.Claims) {
				claims.WorkflowRef = "example/example-web/.github/workflows/other.yml@refs/heads/main"
			},
		},
		{
			name: "environment",
			mutate: func(claims *githuboidc.Claims) {
				claims.Environment = "staging"
			},
		},
		{
			name: "different reusable workflow",
			mutate: func(claims *githuboidc.Claims) {
				claims.JobWorkflowRef = "example/shared/.github/workflows/deploy.yml@refs/heads/main"
				claims.JobWorkflowRefPresent = true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			claims := testClaims()
			test.mutate(&claims)
			assertDenied(t, New(testConfiguration()), "example-web", claims)
		})
	}
}

func TestPolicyDeniesUnknownDeployment(t *testing.T) {
	t.Parallel()

	assertDenied(t, New(testConfiguration()), "unknown", testClaims())
}

func TestPolicyCopiesConfiguration(t *testing.T) {
	t.Parallel()

	configuration := testConfiguration()
	policy := New(configuration)
	configuration.Deployments[0].Target.Service = "mutated"

	deployment, err := policy.Authorise("example-web", testClaims())
	if err != nil {
		t.Fatalf("Authorise() error = %v", err)
	}
	if deployment.Service != "example_web" {
		t.Errorf("Service = %q, want %q", deployment.Service, "example_web")
	}
}

func assertDenied(t *testing.T, policy *Policy, name string, claims githuboidc.Claims) {
	t.Helper()

	deployment, err := policy.Authorise(name, claims)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Authorise() error = %v, want ErrDenied", err)
	}
	if deployment != (Deployment{}) {
		t.Errorf("Authorise() = %#v, want zero deployment", deployment)
	}
}

func testConfiguration() config.Config {
	return config.Config{
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

func testClaims() githuboidc.Claims {
	return githuboidc.Claims{
		RepositoryID: "123456789",
		Repository:   "example/example-web",
		WorkflowRef:  "example/example-web/.github/workflows/deploy.yml@refs/heads/main",
		Environment:  "production",
	}
}
