package swarm_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/swarm"
	cerrdefs "github.com/containerd/errdefs"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

const (
	repository   = "ghcr.io/example/example-web"
	imageDigest  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	desiredImage = repository + "@" + imageDigest
)

func TestApplyChangesOnlyTheContainerImage(t *testing.T) {
	service := populatedService()
	original := cloneService(t, service)
	client := &fakeClient{service: service}

	result, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if result.Action != swarm.ActionUpdated {
		t.Errorf("Apply() action = %q, want %q", result.Action, swarm.ActionUpdated)
	}
	if result.ServiceID != service.ID {
		t.Errorf("Apply() service ID = %q, want %q", result.ServiceID, service.ID)
	}
	if result.Image != desiredImage {
		t.Errorf("Apply() image = %q, want %q", result.Image, desiredImage)
	}
	if result.InspectedVersion != service.Version.Index {
		t.Errorf("Apply() version = %d, want %d", result.InspectedVersion, service.Version.Index)
	}
	if !reflect.DeepEqual(result.Warnings, []string{"warning"}) {
		t.Errorf("Apply() warnings = %#v, want update warnings", result.Warnings)
	}

	if client.updatedServiceID != service.ID {
		t.Errorf("ServiceUpdate() service ID = %q, want %q", client.updatedServiceID, service.ID)
	}
	if client.updateOptions.Version != service.Version {
		t.Errorf("ServiceUpdate() version = %#v, want %#v", client.updateOptions.Version, service.Version)
	}
	if client.updateOptions.RegistryAuthFrom != swarmtypes.RegistryAuthFromSpec {
		t.Errorf("ServiceUpdate() registry auth source = %q, want %q", client.updateOptions.RegistryAuthFrom, swarmtypes.RegistryAuthFromSpec)
	}
	if client.updateOptions.QueryRegistry {
		t.Error("ServiceUpdate() queried the registry")
	}

	expected := original.Spec
	expectedContainer := *expected.TaskTemplate.ContainerSpec
	expectedContainer.Image = desiredImage
	expected.TaskTemplate.ContainerSpec = &expectedContainer
	if !reflect.DeepEqual(client.updateOptions.Spec, expected) {
		t.Errorf("ServiceUpdate() spec = %#v, want only image changed from %#v", client.updateOptions.Spec, original.Spec)
	}
	if !reflect.DeepEqual(client.service, original) {
		t.Error("Apply() mutated the inspected service")
	}
}

func TestApplyReturnsNoChangeForTheSameDigest(t *testing.T) {
	service := populatedService()
	service.Spec.TaskTemplate.ContainerSpec.Image = repository + ":release@" + imageDigest
	client := &fakeClient{service: service}

	result, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if result.Action != swarm.ActionNoChange {
		t.Errorf("Apply() action = %q, want %q", result.Action, swarm.ActionNoChange)
	}
	if client.updateCalls != 0 {
		t.Errorf("ServiceUpdate() calls = %d, want 0", client.updateCalls)
	}
}

func TestInspectReportsTheCurrentTargetWithoutUpdating(t *testing.T) {
	service := populatedService()
	service.Spec.TaskTemplate.ContainerSpec.Image = desiredImage
	client := &fakeClient{service: service}

	inspection, err := swarm.NewUpdater(client).Inspect(context.Background(), "example_web", repository, imageDigest)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !inspection.Desired {
		t.Error("Inspect() desired = false, want true")
	}
	if inspection.ServiceID != service.ID || inspection.Image != desiredImage || inspection.Version != service.Version.Index {
		t.Errorf("Inspect() = %#v, want service identity, desired image and version", inspection)
	}
	if client.updateCalls != 0 {
		t.Errorf("ServiceUpdate() calls = %d, want 0", client.updateCalls)
	}
}

func TestApplyRejectsInvalidImagesBeforeInspecting(t *testing.T) {
	tests := map[string]struct {
		repository string
		digest     string
	}{
		"tagged repository": {repository: repository + ":latest", digest: imageDigest},
		"missing digest":    {repository: repository, digest: ""},
		"different algorithm": {
			repository: repository,
			digest:     "sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{service: populatedService()}

			_, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", test.repository, test.digest)
			if !errors.Is(err, swarm.ErrInvalidImage) {
				t.Fatalf("Apply() error = %v, want ErrInvalidImage", err)
			}
			if client.inspectCalls != 0 {
				t.Errorf("ServiceInspect() calls = %d, want 0", client.inspectCalls)
			}
		})
	}
}

func TestApplyRejectsNonContainerService(t *testing.T) {
	service := populatedService()
	service.Spec.TaskTemplate.ContainerSpec = nil
	client := &fakeClient{service: service}

	_, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
	if !errors.Is(err, swarm.ErrUnsupportedService) {
		t.Fatalf("Apply() error = %v, want ErrUnsupportedService", err)
	}
	if client.updateCalls != 0 {
		t.Errorf("ServiceUpdate() calls = %d, want 0", client.updateCalls)
	}
}

func TestApplyReportsUpdateErrorsAsIndeterminate(t *testing.T) {
	updateError := errors.New("connection closed")
	client := &fakeClient{service: populatedService(), updateError: updateError}

	result, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
	if !errors.Is(err, updateError) {
		t.Fatalf("Apply() error = %v, want update error", err)
	}
	if result.Action != swarm.ActionIndeterminate {
		t.Errorf("Apply() action = %q, want %q", result.Action, swarm.ActionIndeterminate)
	}
}

func TestApplyReportsDefinitiveUpdateRejections(t *testing.T) {
	tests := map[string]error{
		"invalid service specification": cerrdefs.ErrInvalidArgument,
		"service no longer exists":      cerrdefs.ErrNotFound,
	}

	for name, updateError := range tests {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{service: populatedService(), updateError: updateError}

			result, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
			if !errors.Is(err, updateError) {
				t.Fatalf("Apply() error = %v, want update error", err)
			}
			if result.Action != swarm.ActionRejected {
				t.Errorf("Apply() action = %q, want %q", result.Action, swarm.ActionRejected)
			}
		})
	}
}

func TestApplyReportsConcurrentChangesAsConflicts(t *testing.T) {
	client := &fakeClient{
		service:     populatedService(),
		updateError: cerrdefs.ErrConflict,
	}

	result, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
	if !errors.Is(err, swarm.ErrConflict) {
		t.Fatalf("Apply() error = %v, want ErrConflict", err)
	}
	if result.Action != "" {
		t.Errorf("Apply() action = %q, want empty", result.Action)
	}
}

func TestRetryAppliesOnlyAtTheOriginallyInspectedVersion(t *testing.T) {
	tests := map[string]struct {
		image          string
		version        uint64
		action         swarm.Action
		wantConflict   bool
		wantUpdateCall bool
	}{
		"original submission was accepted": {
			image:   desiredImage,
			version: 43,
			action:  swarm.ActionNoChange,
		},
		"original submission was not accepted": {
			image:          repository + ":previous",
			version:        42,
			action:         swarm.ActionUpdated,
			wantUpdateCall: true,
		},
		"another update advanced the service": {
			image:        repository + ":other",
			version:      43,
			wantConflict: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service := populatedService()
			service.Version.Index = test.version
			service.Spec.TaskTemplate.ContainerSpec.Image = test.image
			client := &fakeClient{service: service}

			result, err := swarm.NewUpdater(client).Retry(context.Background(), "example_web", repository, imageDigest, 42)
			if test.wantConflict {
				if !errors.Is(err, swarm.ErrConflict) {
					t.Fatalf("Retry() error = %v, want ErrConflict", err)
				}
			} else if err != nil {
				t.Fatalf("Retry() error = %v", err)
			}
			if result.Action != test.action {
				t.Errorf("Retry() action = %q, want %q", result.Action, test.action)
			}
			if got := client.updateCalls == 1; got != test.wantUpdateCall {
				t.Errorf("Retry() update call = %v, want %v", got, test.wantUpdateCall)
			}
		})
	}
}

func TestApplyReturnsInspectErrorsWithoutUpdating(t *testing.T) {
	inspectError := errors.New("service not found")
	client := &fakeClient{inspectError: inspectError}

	result, err := swarm.NewUpdater(client).Apply(context.Background(), "example_web", repository, imageDigest)
	if !errors.Is(err, inspectError) {
		t.Fatalf("Apply() error = %v, want inspect error", err)
	}
	if result.Action != "" {
		t.Errorf("Apply() action = %q, want empty", result.Action)
	}
	if client.updateCalls != 0 {
		t.Errorf("ServiceUpdate() calls = %d, want 0", client.updateCalls)
	}
}

type fakeClient struct {
	service          swarmtypes.Service
	inspectError     error
	updateError      error
	inspectCalls     int
	updateCalls      int
	updatedServiceID string
	updateOptions    clienttypes.ServiceUpdateOptions
}

func (client *fakeClient) ServiceInspect(_ context.Context, _ string, _ clienttypes.ServiceInspectOptions) (clienttypes.ServiceInspectResult, error) {
	client.inspectCalls++
	return clienttypes.ServiceInspectResult{Service: client.service}, client.inspectError
}

func (client *fakeClient) ServiceUpdate(_ context.Context, serviceID string, options clienttypes.ServiceUpdateOptions) (clienttypes.ServiceUpdateResult, error) {
	client.updateCalls++
	client.updatedServiceID = serviceID
	client.updateOptions = options
	return clienttypes.ServiceUpdateResult{Warnings: []string{"warning"}}, client.updateError
}

func populatedService() swarmtypes.Service {
	replicas := uint64(3)
	return swarmtypes.Service{
		ID: "service-id",
		Meta: swarmtypes.Meta{
			Version: swarmtypes.Version{Index: 42},
		},
		Spec: swarmtypes.ServiceSpec{
			Annotations: swarmtypes.Annotations{
				Name:   "example_web",
				Labels: map[string]string{"com.example.owner": "infrastructure"},
			},
			TaskTemplate: swarmtypes.TaskSpec{
				ContainerSpec: &swarmtypes.ContainerSpec{
					Image:   repository + ":previous",
					Command: []string{"/app/server"},
					Env:     []string{"APP_ENV=production"},
				},
				Placement: &swarmtypes.Placement{Constraints: []string{"node.role==worker"}},
			},
			Mode: swarmtypes.ServiceMode{Replicated: &swarmtypes.ReplicatedService{Replicas: &replicas}},
			UpdateConfig: &swarmtypes.UpdateConfig{
				Parallelism: 1,
				Delay:       2 * time.Second,
			},
			EndpointSpec: &swarmtypes.EndpointSpec{Mode: swarmtypes.ResolutionModeVIP},
		},
	}
}

func cloneService(t *testing.T, service swarmtypes.Service) swarmtypes.Service {
	t.Helper()

	clone := service
	clone.Spec.Annotations.Labels = cloneStringMap(service.Spec.Annotations.Labels)
	container := *service.Spec.TaskTemplate.ContainerSpec
	container.Command = append([]string(nil), container.Command...)
	container.Env = append([]string(nil), container.Env...)
	clone.Spec.TaskTemplate.ContainerSpec = &container
	placement := *service.Spec.TaskTemplate.Placement
	placement.Constraints = append([]string(nil), placement.Constraints...)
	clone.Spec.TaskTemplate.Placement = &placement
	updateConfig := *service.Spec.UpdateConfig
	clone.Spec.UpdateConfig = &updateConfig
	endpointSpec := *service.Spec.EndpointSpec
	clone.Spec.EndpointSpec = &endpointSpec
	return clone
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
