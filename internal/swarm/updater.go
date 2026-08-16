// Package swarm applies constrained image updates to Docker Swarm services.
package swarm

import (
	"context"
	"errors"
	"fmt"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"

	cerrdefs "github.com/containerd/errdefs"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	clienttypes "github.com/moby/moby/client"
)

var (
	// ErrInvalidImage identifies an invalid repository or immutable digest.
	ErrInvalidImage = errors.New("invalid image")
	// ErrConflict identifies a service changed after Swarrow inspected it.
	ErrConflict = errors.New("service changed concurrently")
	// ErrUnsupportedService identifies a service that does not run containers.
	ErrUnsupportedService = errors.New("unsupported service")
)

// Action records whether an image update was required and established.
type Action string

const (
	ActionNoChange      Action = "no_change"
	ActionUpdated       Action = "updated"
	ActionRejected      Action = "rejected"
	ActionIndeterminate Action = "indeterminate"
)

// Result describes the image mutation attempted against one inspected service version.
type Result struct {
	Action           Action
	ServiceID        string
	Image            string
	InspectedVersion uint64
	Warnings         []string
}

// Inspection reports whether a service currently targets one immutable image.
type Inspection struct {
	ServiceID string
	Image     string
	Desired   bool
	Version   uint64
}

type serviceClient interface {
	ServiceInspect(context.Context, string, clienttypes.ServiceInspectOptions) (clienttypes.ServiceInspectResult, error)
	ServiceUpdate(context.Context, string, clienttypes.ServiceUpdateOptions) (clienttypes.ServiceUpdateResult, error)
}

// Updater changes only the container image of an existing Swarm service.
type Updater struct {
	client serviceClient
}

// NewUpdater creates an image-only service updater.
func NewUpdater(client serviceClient) *Updater {
	return &Updater{client: client}
}

// Apply sets an existing service to repository at the supplied immutable digest.
func (updater *Updater) Apply(ctx context.Context, serviceName string, repository string, imageDigest string) (Result, error) {
	return updater.apply(ctx, serviceName, repository, imageDigest, nil)
}

// Inspect checks the current service image without submitting an update.
func (updater *Updater) Inspect(ctx context.Context, serviceName string, repository string, imageDigest string) (Inspection, error) {
	target, err := updater.inspect(ctx, serviceName, repository, imageDigest)
	if err != nil {
		return Inspection{}, err
	}

	return Inspection{
		ServiceID: target.service.ID,
		Image:     target.image,
		Desired:   sameImage(target.container.Image, target.image),
		Version:   target.service.Version.Index,
	}, nil
}

// Retry re-inspects an indeterminate update and changes the image only when the
// service still has the version observed before the original submission.
func (updater *Updater) Retry(ctx context.Context, serviceName string, repository string, imageDigest string, inspectedVersion uint64) (Result, error) {
	return updater.apply(ctx, serviceName, repository, imageDigest, &inspectedVersion)
}

func (updater *Updater) apply(ctx context.Context, serviceName string, repository string, imageDigest string, expectedVersion *uint64) (Result, error) {
	target, err := updater.inspect(ctx, serviceName, repository, imageDigest)
	if err != nil {
		return Result{}, err
	}

	service := target.service
	result := Result{
		ServiceID:        service.ID,
		Image:            target.image,
		InspectedVersion: service.Version.Index,
	}

	if sameImage(target.container.Image, target.image) {
		result.Action = ActionNoChange
		return result, nil
	}
	if expectedVersion != nil && service.Version.Index != *expectedVersion {
		return result, fmt.Errorf("%w: service %q advanced from version %d to %d", ErrConflict, serviceName, *expectedVersion, service.Version.Index)
	}

	spec := service.Spec
	containerCopy := *target.container
	containerCopy.Image = target.image
	spec.TaskTemplate.ContainerSpec = &containerCopy

	updated, err := updater.client.ServiceUpdate(ctx, service.ID, clienttypes.ServiceUpdateOptions{
		Version:          service.Version,
		Spec:             spec,
		RegistryAuthFrom: swarmtypes.RegistryAuthFromSpec,
		QueryRegistry:    false,
	})
	if err != nil {
		if cerrdefs.IsConflict(err) {
			return result, fmt.Errorf("%w: update service %q: %v", ErrConflict, serviceName, err)
		}
		if isDefinitiveRejection(err) {
			result.Action = ActionRejected
			return result, fmt.Errorf("update service %q: %w", serviceName, err)
		}

		result.Action = ActionIndeterminate
		return result, fmt.Errorf("update service %q: %w", serviceName, err)
	}

	result.Action = ActionUpdated
	result.Warnings = append([]string(nil), updated.Warnings...)
	return result, nil
}

func isDefinitiveRejection(err error) bool {
	return cerrdefs.IsInvalidArgument(err) ||
		cerrdefs.IsUnauthorized(err) ||
		cerrdefs.IsPermissionDenied(err) ||
		cerrdefs.IsNotFound(err) ||
		cerrdefs.IsNotModified(err) ||
		cerrdefs.IsFailedPrecondition(err) ||
		cerrdefs.IsResourceExhausted(err) ||
		cerrdefs.IsNotImplemented(err)
}

type inspectedTarget struct {
	service   swarmtypes.Service
	container *swarmtypes.ContainerSpec
	image     string
}

func (updater *Updater) inspect(ctx context.Context, serviceName string, repository string, imageDigest string) (inspectedTarget, error) {
	image, err := buildImageReference(repository, imageDigest)
	if err != nil {
		return inspectedTarget{}, err
	}

	inspected, err := updater.client.ServiceInspect(ctx, serviceName, clienttypes.ServiceInspectOptions{})
	if err != nil {
		return inspectedTarget{}, fmt.Errorf("inspect service %q: %w", serviceName, err)
	}

	service := inspected.Service
	container := service.Spec.TaskTemplate.ContainerSpec
	if container == nil {
		return inspectedTarget{}, fmt.Errorf("%w: service %q does not use container tasks", ErrUnsupportedService, serviceName)
	}

	return inspectedTarget{service: service, container: container, image: image}, nil
}

func buildImageReference(repository string, imageDigest string) (string, error) {
	named, err := reference.ParseNormalizedNamed(repository)
	if err != nil || !reference.IsNameOnly(named) {
		return "", fmt.Errorf("%w: repository must be a name without a tag or digest", ErrInvalidImage)
	}

	parsedDigest, err := digest.Parse(imageDigest)
	if err != nil || parsedDigest.Algorithm() != digest.SHA256 || parsedDigest.String() != imageDigest {
		return "", fmt.Errorf("%w: digest must be a canonical sha256 digest", ErrInvalidImage)
	}

	digested, err := reference.WithDigest(reference.TrimNamed(named), parsedDigest)
	if err != nil {
		return "", fmt.Errorf("%w: combine repository and digest: %v", ErrInvalidImage, err)
	}

	return digested.String(), nil
}

func sameImage(current string, desired string) bool {
	currentNamed, err := reference.ParseNormalizedNamed(current)
	if err != nil {
		return false
	}

	desiredNamed, err := reference.ParseNormalizedNamed(desired)
	if err != nil {
		return false
	}

	currentDigested, currentHasDigest := currentNamed.(reference.Digested)
	desiredDigested, desiredHasDigest := desiredNamed.(reference.Digested)
	if !currentHasDigest || !desiredHasDigest {
		return false
	}

	return reference.TrimNamed(currentNamed).String() == reference.TrimNamed(desiredNamed).String() &&
		currentDigested.Digest() == desiredDigested.Digest()
}
