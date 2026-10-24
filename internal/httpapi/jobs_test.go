package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/config"
	"github.com/aide-tools/swarrow/internal/deploy"
	"github.com/aide-tools/swarrow/internal/githuboidc"
	"github.com/aide-tools/swarrow/internal/httpapi"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/swarm"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

func TestDigestRequestReachesConfiguredJob(t *testing.T) {
	for _, mode := range []string{"job", "service", ""} {
		t.Run(mode, func(t *testing.T) {
			configuration := config.Config{
				Version: 1, Server: config.Server{Listen: "127.0.0.1:8080", RequestTimeout: time.Second},
				GitHub: config.GitHub{Audience: "https://deploy.example.net"},
				Deployments: []config.Deployment{{Name: "example-web",
					Identity: config.Identity{RepositoryID: "123", WorkflowRef: "workflow", Environment: "production"},
					Target:   config.Target{Mode: mode, Service: "migration", Image: "ghcr.io/example/app"},
				}},
			}
			one := uint64(1)
			client := &jobDocker{service: swarmtypes.Service{
				ID: "migration", Meta: swarmtypes.Meta{Version: swarmtypes.Version{Index: 42}},
				Spec: swarmtypes.ServiceSpec{
					Mode: swarmtypes.ServiceMode{ReplicatedJob: &swarmtypes.ReplicatedJob{MaxConcurrent: &one, TotalCompletions: &one}},
					TaskTemplate: swarmtypes.TaskSpec{
						ContainerSpec: &swarmtypes.ContainerSpec{Image: "ghcr.io/example/app@sha256:" + strings.Repeat("a", 64)},
						RestartPolicy: &swarmtypes.RestartPolicy{Condition: swarmtypes.RestartPolicyConditionNone},
					},
				},
				JobStatus: &swarmtypes.JobStatus{JobIteration: swarmtypes.Version{Index: 7}},
			}}
			jobs, err := swarm.NewJobs(client, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			observer, err := swarm.NewObserver(client, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			coordinator, err := deploy.New(ctx, configuration, policy.New(configuration), swarm.NewUpdater(client), observer, jobs)
			if err != nil {
				t.Fatal(err)
			}
			verifier := &fakeVerifier{verify: func(context.Context, string) (githuboidc.Claims, error) {
				return githuboidc.Claims{TokenID: "token", RepositoryID: "123", WorkflowRef: "workflow", Environment: "production", ValidUntil: time.Now().Add(time.Hour)}, nil
			}}
			handler, err := httpapi.New(verifier, coordinator, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`))
			if mode == "job" {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"conclusion":"completed"`) || client.updates != 1 {
					t.Fatalf("response=%d %s updates=%d", response.Code, response.Body.String(), client.updates)
				}
				if strings.Contains(response.Body.String(), "JobIteration") || strings.Contains(response.Body.String(), "InspectedVersion") {
					t.Fatal("internal retry state leaked")
				}
			} else if response.Code != http.StatusBadGateway || client.updates != 0 {
				t.Fatalf("ordinary target accepted job: %d updates=%d", response.Code, client.updates)
			}
		})
	}
}

type jobDocker struct {
	service swarmtypes.Service
	updates int
}

func (client *jobDocker) ServiceInspect(context.Context, string, clienttypes.ServiceInspectOptions) (clienttypes.ServiceInspectResult, error) {
	return clienttypes.ServiceInspectResult{Service: client.service}, nil
}
func (client *jobDocker) ServiceUpdate(_ context.Context, _ string, options clienttypes.ServiceUpdateOptions) (clienttypes.ServiceUpdateResult, error) {
	client.updates++
	previous := client.service.Spec
	client.service.PreviousSpec = &previous
	client.service.Spec = options.Spec
	client.service.Version.Index++
	client.service.JobStatus = &swarmtypes.JobStatus{JobIteration: swarmtypes.Version{Index: 10}}
	return clienttypes.ServiceUpdateResult{}, nil
}
func (client *jobDocker) TaskList(context.Context, clienttypes.TaskListOptions) (clienttypes.TaskListResult, error) {
	return clienttypes.TaskListResult{Items: []swarmtypes.Task{{
		ServiceID: client.service.ID, JobIteration: &client.service.JobStatus.JobIteration,
		Status: swarmtypes.TaskStatus{State: swarmtypes.TaskStateComplete, ContainerStatus: &swarmtypes.ContainerStatus{ExitCode: 0}},
	}}}, nil
}
