// Package httpapi exposes Swarrow's narrow deployment HTTP interface.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/aide-tools/swarrow/internal/deploy"
	"github.com/aide-tools/swarrow/internal/githuboidc"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/replay"
	"github.com/aide-tools/swarrow/internal/swarm"
	"github.com/opencontainers/go-digest"
)

const maxDeploymentBodyBytes = 1024

var ErrInvalidConfiguration = errors.New("invalid HTTP API configuration")

type tokenVerifier interface {
	Verify(context.Context, string) (githuboidc.Claims, error)
}

type deployer interface {
	Deploy(context.Context, deploy.Request) (deploy.Outcome, error)
}

// New creates the complete Swarrow HTTP handler.
func New(verifier tokenVerifier, deploymentService deployer, requestTimeout time.Duration) (http.Handler, error) {
	if verifier == nil || deploymentService == nil || requestTimeout <= 0 {
		return nil, ErrInvalidConfiguration
	}

	api := &handler{
		verifier:       verifier,
		deployer:       deploymentService,
		requestTimeout: requestTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", api.health)
	mux.HandleFunc("/v1/deployments/{deployment}", api.deployment)
	mux.HandleFunc("/", api.notFound)
	return securityHeaders(mux), nil
}

type handler struct {
	verifier       tokenVerifier
	deployer       deployer
	requestTimeout time.Duration
}

func (handler *handler) health(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	if request.Method == http.MethodHead {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		return
	}

	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (handler *handler) deployment(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), handler.requestTimeout)
	defer cancel()
	body := request.Body
	stopBodyClose := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stopBodyClose()

	rawToken, err := bearerToken(request.Header.Values("Authorization"))
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Valid Bearer authentication is required")
		return
	}

	claims, err := handler.verifier.Verify(ctx, rawToken)
	if err != nil {
		if writeContextError(writer, ctx) {
			return
		}
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Valid Bearer authentication is required")
		return
	}

	deploymentRequest, err := decodeDeploymentRequest(writer, request)
	if err != nil {
		if writeContextError(writer, ctx) {
			return
		}
		writeError(writer, http.StatusBadRequest, "invalid_request", "Request body must contain one canonical sha256 digest")
		return
	}

	outcome, err := handler.deployer.Deploy(ctx, deploy.Request{
		Deployment: request.PathValue("deployment"),
		Digest:     deploymentRequest.Digest,
		Claims:     claims,
	})
	if err != nil {
		writeDeployError(writer, outcome, err)
		return
	}

	writeOutcome(writer, outcome)
}

func (handler *handler) notFound(writer http.ResponseWriter, _ *http.Request) {
	writeError(writer, http.StatusNotFound, "not_found", "Endpoint not found")
}

type deploymentRequest struct {
	Digest string `json:"digest"`
}

func decodeDeploymentRequest(writer http.ResponseWriter, request *http.Request) (deploymentRequest, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return deploymentRequest{}, errors.New("content type must be application/json")
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxDeploymentBodyBytes)
	decoder := json.NewDecoder(request.Body)

	var decoded deploymentRequest
	opening, err := decoder.Token()
	if err != nil {
		return deploymentRequest{}, fmt.Errorf("decode request: %w", err)
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return deploymentRequest{}, errors.New("request must be a JSON object")
	}

	seenDigest := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return deploymentRequest{}, fmt.Errorf("decode request field: %w", err)
		}
		if key != "digest" || seenDigest {
			return deploymentRequest{}, errors.New("request must contain only one digest field")
		}
		if err := decoder.Decode(&decoded.Digest); err != nil {
			return deploymentRequest{}, fmt.Errorf("decode digest: %w", err)
		}
		seenDigest = true
	}
	if _, err := decoder.Token(); err != nil {
		return deploymentRequest{}, fmt.Errorf("close request object: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return deploymentRequest{}, fmt.Errorf("decode trailing request: %w", err)
		}
		return deploymentRequest{}, errors.New("multiple JSON values are not allowed")
	}

	parsed, err := digest.Parse(decoded.Digest)
	if err != nil || parsed.Algorithm() != digest.SHA256 || parsed.String() != decoded.Digest {
		return deploymentRequest{}, errors.New("digest must be a canonical sha256 digest")
	}

	return decoded, nil
}

func bearerToken(values []string) (string, error) {
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
		return "", errors.New("authorization header must appear exactly once")
	}

	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return "", errors.New("authorization header must contain one Bearer token")
	}

	return token, nil
}

type response struct {
	Deployment string           `json:"deployment,omitempty"`
	Action     deploy.Action    `json:"action,omitempty"`
	Conclusion swarm.Conclusion `json:"conclusion,omitempty"`
	Image      string           `json:"image,omitempty"`
	Message    string           `json:"message,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
	Tasks      *taskSummary     `json:"tasks,omitempty"`
	Error      *errorResponse   `json:"error,omitempty"`
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type taskSummary struct {
	Running  int           `json:"running"`
	Pending  int           `json:"pending"`
	Failed   int           `json:"failed"`
	Failures []taskFailure `json:"failures,omitempty"`
}

type taskFailure struct {
	ID    string `json:"id"`
	Slot  int    `json:"slot"`
	State string `json:"state"`
	Error string `json:"error"`
}

func writeOutcome(writer http.ResponseWriter, outcome deploy.Outcome) {
	status := http.StatusOK
	switch outcome.Conclusion {
	case swarm.ConclusionCompleted:
	case swarm.ConclusionInProgress:
		status = http.StatusAccepted
	case swarm.ConclusionSuperseded:
		status = http.StatusConflict
	case swarm.ConclusionFailed, swarm.ConclusionRolledBack:
		status = http.StatusBadGateway
	default:
		writeError(writer, http.StatusInternalServerError, "internal_error", "Deployment produced no recognised conclusion")
		return
	}

	writeJSON(writer, status, outcomeResponse(outcome))
}

func writeDeployError(writer http.ResponseWriter, outcome deploy.Outcome, err error) {
	status, code, message := http.StatusBadGateway, "deployment_error", "Deployment could not be completed"
	switch {
	case errors.Is(err, policy.ErrDenied), errors.Is(err, replay.ErrReused):
		status, code, message = http.StatusForbidden, "forbidden", "Deployment authorisation denied"
	case errors.Is(err, replay.ErrCapacity), errors.Is(err, deploy.ErrQueueFull):
		status, code, message = http.StatusTooManyRequests, "capacity_exhausted", "Deployment capacity is exhausted"
	case errors.Is(err, swarm.ErrConflict):
		status, code, message = http.StatusConflict, "service_conflict", "The service changed concurrently"
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, "request_timeout", "Deployment request timed out"
	case errors.Is(err, context.Canceled):
		status, code, message = http.StatusRequestTimeout, "request_cancelled", "Deployment request was cancelled"
	case errors.Is(err, replay.ErrInvalidBinding):
		status, code, message = http.StatusUnauthorized, "unauthorized", "The deployment token is no longer valid"
	}

	response := outcomeResponse(outcome)
	response.Error = &errorResponse{Code: code, Message: message}
	writeJSON(writer, status, response)
}

func outcomeResponse(outcome deploy.Outcome) response {
	failures := make([]taskFailure, 0, len(outcome.Tasks.Failures))
	for _, failure := range outcome.Tasks.Failures {
		failures = append(failures, taskFailure{
			ID:    failure.ID,
			Slot:  failure.Slot,
			State: string(failure.State),
			Error: failure.Error,
		})
	}

	return response{
		Deployment: outcome.Deployment,
		Action:     outcome.Action,
		Conclusion: outcome.Conclusion,
		Image:      outcome.Image,
		Message:    outcome.Message,
		Warnings:   outcome.Warnings,
		Tasks: &taskSummary{
			Running:  outcome.Tasks.Running,
			Pending:  outcome.Tasks.Pending,
			Failed:   outcome.Tasks.Failed,
			Failures: failures,
		},
	}
}

func writeError(writer http.ResponseWriter, status int, code string, message string) {
	writeJSON(writer, status, response{Error: &errorResponse{Code: code, Message: message}})
}

func writeContextError(writer http.ResponseWriter, ctx context.Context) bool {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		writeError(writer, http.StatusGatewayTimeout, "request_timeout", "Deployment request timed out")
		return true
	case context.Canceled:
		writeError(writer, http.StatusRequestTimeout, "request_cancelled", "Deployment request was cancelled")
		return true
	default:
		return false
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}
