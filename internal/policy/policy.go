// Package policy authorises authenticated workflow identities against local deployment policy.
package policy

import (
	"errors"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/githuboidc"
)

// ErrDenied is returned when a workflow cannot use the requested deployment.
var ErrDenied = errors.New("deployment authorisation denied")

// Deployment is the operator-controlled capability granted to a workflow.
type Deployment struct {
	Name    string
	Service string
	Image   string
}

// Policy is an immutable lookup of validated deployment policy.
type Policy struct {
	deployments map[string]config.Deployment
}

// New creates an immutable authorisation policy from validated configuration.
func New(configuration config.Config) *Policy {
	deployments := make(map[string]config.Deployment, len(configuration.Deployments))
	for _, deployment := range configuration.Deployments {
		deployments[deployment.Name] = deployment
	}

	return &Policy{deployments: deployments}
}

// Authorise returns the fixed capability when every identity constraint matches.
func (policy *Policy) Authorise(name string, claims githuboidc.Claims) (Deployment, error) {
	deployment, exists := policy.deployments[name]
	if !exists || !matches(deployment.Identity, claims) {
		return Deployment{}, ErrDenied
	}

	return Deployment{
		Name:    deployment.Name,
		Service: deployment.Target.Service,
		Image:   deployment.Target.Image,
	}, nil
}

func matches(identity config.Identity, claims githuboidc.Claims) bool {
	return matchesDirectWorkflow(claims) &&
		claims.RepositoryID == identity.RepositoryID &&
		claims.WorkflowRef == identity.WorkflowRef &&
		claims.Environment == identity.Environment
}

func matchesDirectWorkflow(claims githuboidc.Claims) bool {
	return !claims.JobWorkflowRefPresent || claims.JobWorkflowRef == claims.WorkflowRef
}
