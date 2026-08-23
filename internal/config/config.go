// Package config decodes and validates Swarrow's local deployment policy.
package config

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is Swarrow's complete file-based configuration.
type Config struct {
	Version     int          `yaml:"version"`
	Server      Server       `yaml:"server"`
	GitHub      GitHub       `yaml:"github"`
	Deployments []Deployment `yaml:"deployments"`
}

// Server configures the Swarrow HTTP server.
type Server struct {
	Listen         string        `yaml:"listen"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// GitHub configures GitHub Actions token verification.
type GitHub struct {
	Audience string `yaml:"audience"`
}

// Deployment grants one workflow identity access to one deployment target.
type Deployment struct {
	Name     string   `yaml:"name"`
	Identity Identity `yaml:"identity"`
	Target   Target   `yaml:"target"`
}

// Identity identifies one authorised GitHub Actions workflow.
type Identity struct {
	RepositoryID string `yaml:"repository_id"`
	Repository   string `yaml:"repository"`
	WorkflowRef  string `yaml:"workflow_ref"`
	Environment  string `yaml:"environment"`
}

// Target identifies the existing Swarm service and permitted image repository.
type Target struct {
	Service string `yaml:"service"`
	Image   string `yaml:"image"`
}

// Decode reads and validates exactly one YAML configuration document.
func Decode(reader io.Reader) (Config, error) {
	contents, err := io.ReadAll(reader)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}

	documentDecoder := yaml.NewDecoder(bytes.NewReader(contents))
	var document yaml.Node
	if err := documentDecoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}

	var extra any
	if err := documentDecoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("decode trailing configuration: %w", err)
		}

		return Config{}, fmt.Errorf("decode configuration: multiple YAML documents are not allowed")
	}

	if err := rejectYAMLReferences(&document); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)

	var configuration Config
	if err := decoder.Decode(&configuration); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}

	if err := Validate(configuration); err != nil {
		return Config{}, err
	}

	return configuration, nil
}

func rejectYAMLReferences(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("line %d: YAML aliases are not allowed", node.Line)
	}

	if node.ShortTag() == "!!merge" {
		return fmt.Errorf("line %d: YAML merge keys are not allowed", node.Line)
	}

	for _, child := range node.Content {
		if err := rejectYAMLReferences(child); err != nil {
			return err
		}
	}

	return nil
}
