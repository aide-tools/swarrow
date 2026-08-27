package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/distribution/reference"
)

const supportedVersion = 1

// Validate checks the complete configuration before it can be used as policy.
func Validate(configuration Config) error {
	var problems []string

	if configuration.Version != supportedVersion {
		problems = append(problems, fmt.Sprintf("version: must be %d", supportedVersion))
	}

	problems = append(problems, validateListenAddress(configuration.Server.Listen)...)
	if configuration.Server.RequestTimeout <= 0 {
		problems = append(problems, "server.request_timeout: must be a positive duration")
	}
	problems = append(problems, validateRequired("github.audience", configuration.GitHub.Audience)...)
	problems = append(problems, validateOptional("github.job_workflow_ref", configuration.GitHub.JobWorkflowRef)...)

	if len(configuration.Deployments) == 0 {
		problems = append(problems, "deployments: must contain at least one deployment")
	}

	deploymentNames := make(map[string]int, len(configuration.Deployments))
	serviceTargets := make(map[string]int, len(configuration.Deployments))

	for index, deployment := range configuration.Deployments {
		path := fmt.Sprintf("deployments[%d]", index)

		problems = append(problems, validateRequired(path+".name", deployment.Name)...)
		problems = append(problems, validateRepositoryID(path+".identity.repository_id", deployment.Identity.RepositoryID)...)
		problems = append(problems, validateOptional(path+".identity.repository", deployment.Identity.Repository)...)
		problems = append(problems, validateRequired(path+".identity.workflow_ref", deployment.Identity.WorkflowRef)...)
		problems = append(problems, validateOptional(path+".identity.job_workflow_ref", deployment.Identity.JobWorkflowRef)...)
		problems = append(problems, validateRequired(path+".identity.environment", deployment.Identity.Environment)...)
		problems = append(problems, validateRequired(path+".target.service", deployment.Target.Service)...)
		problems = append(problems, validateImageRepository(path+".target.image", deployment.Target.Image)...)

		if previous, exists := deploymentNames[deployment.Name]; exists && deployment.Name != "" {
			problems = append(problems, fmt.Sprintf("%s.name: duplicates deployments[%d].name", path, previous))
		} else {
			deploymentNames[deployment.Name] = index
		}

		if previous, exists := serviceTargets[deployment.Target.Service]; exists && deployment.Target.Service != "" {
			problems = append(problems, fmt.Sprintf("%s.target.service: duplicates deployments[%d].target.service", path, previous))
		} else {
			serviceTargets[deployment.Target.Service] = index
		}
	}

	if len(problems) > 0 {
		return validationError(problems)
	}

	return nil
}

func validateListenAddress(address string) []string {
	if problems := validateRequired("server.listen", address); len(problems) > 0 {
		return problems
	}

	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return []string{"server.listen: must be a host and port"}
	}

	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return []string{"server.listen: port must be an integer between 1 and 65535"}
	}

	return nil
}

func validateRepositoryID(path string, repositoryID string) []string {
	if problems := validateRequired(path, repositoryID); len(problems) > 0 {
		return problems
	}

	parsed, err := strconv.ParseUint(repositoryID, 10, 64)
	if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != repositoryID {
		return []string{path + ": must be a positive decimal GitHub repository ID"}
	}

	return nil
}

func validateImageRepository(path string, image string) []string {
	if problems := validateRequired(path, image); len(problems) > 0 {
		return problems
	}

	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return []string{path + ": must be a valid container image repository"}
	}

	if !reference.IsNameOnly(named) {
		return []string{path + ": must not include a tag or digest"}
	}

	return nil
}

func validateRequired(path string, value string) []string {
	if value == "" {
		return []string{path + ": must not be empty"}
	}

	if strings.TrimSpace(value) != value {
		return []string{path + ": must not have leading or trailing whitespace"}
	}

	return nil
}

func validateOptional(path string, value string) []string {
	if value != "" && strings.TrimSpace(value) != value {
		return []string{path + ": must not have leading or trailing whitespace"}
	}

	return nil
}

type validationError []string

func (problems validationError) Error() string {
	return "validate configuration:\n- " + strings.Join(problems, "\n- ")
}
