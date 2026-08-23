// Package httpapi exposes Swarrow's narrow deployment HTTP interface.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
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
func New(verifier tokenVerifier, deploymentService deployer, requestTimeout time.Duration, logger *slog.Logger) (http.Handler, error) {
	if verifier == nil || deploymentService == nil || requestTimeout <= 0 || logger == nil {
		return nil, ErrInvalidConfiguration
	}

	api := &handler{
		verifier:       verifier,
		deployer:       deploymentService,
		requestTimeout: requestTimeout,
		logger:         logger,
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
	logger         *slog.Logger
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
	audit := deploymentAudit{
		method:     request.Method,
		deployment: request.PathValue("deployment"),
		startedAt:  time.Now(),
	}
	defer func() { handler.writeAudit(request.Context(), audit) }()

	if request.Method != http.MethodPost {
		audit.status = http.StatusMethodNotAllowed
		audit.errorCode = "method_not_allowed"
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
		audit.status = http.StatusUnauthorized
		audit.errorCode = "unauthorized"
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Valid Bearer authentication is required")
		return
	}

	claims, err := handler.verifier.Verify(ctx, rawToken)
	if err != nil {
		if status, code, written := writeContextError(writer, ctx); written {
			audit.status = status
			audit.errorCode = code
			return
		}
		var warmup retryableAuthenticationError
		if errors.As(err, &warmup) {
			writer.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(warmup.RetryAfter()), 10))
			audit.status = http.StatusServiceUnavailable
			audit.errorCode = "authentication_warming_up"
			writeError(writer, http.StatusServiceUnavailable, "authentication_warming_up", "Deployment authentication is warming up; obtain a new token before retrying")
			return
		}
		audit.status = http.StatusUnauthorized
		audit.errorCode = "unauthorized"
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Valid Bearer authentication is required")
		return
	}
	audit.authenticated = true
	audit.repositoryID = claims.RepositoryID
	audit.workflowRef = claims.WorkflowRef
	audit.environment = claims.Environment

	deploymentRequest, err := decodeDeploymentRequest(writer, request)
	if err != nil {
		if status, code, written := writeContextError(writer, ctx); written {
			audit.status = status
			audit.errorCode = code
			return
		}
		audit.status = http.StatusBadRequest
		audit.errorCode = "invalid_request"
		writeError(writer, http.StatusBadRequest, "invalid_request", "Request body must contain one canonical sha256 digest")
		return
	}
	audit.digest = deploymentRequest.Digest

	outcome, err := handler.deployer.Deploy(ctx, deploy.Request{
		Deployment: request.PathValue("deployment"),
		Digest:     deploymentRequest.Digest,
		Claims:     claims,
	})
	if err != nil {
		audit.action = outcome.Action
		audit.conclusion = outcome.Conclusion
		audit.status, audit.errorCode = writeDeployError(writer, outcome, err)
		return
	}

	audit.action = outcome.Action
	audit.conclusion = outcome.Conclusion
	audit.status, audit.errorCode = writeOutcome(writer, outcome)
}

type retryableAuthenticationError interface {
	error
	RetryAfter() time.Duration
}

func retryAfterSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 1
	}
	return int64((duration + time.Second - 1) / time.Second)
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

func writeOutcome(writer http.ResponseWriter, outcome deploy.Outcome) (int, string) {
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
		return http.StatusInternalServerError, "internal_error"
	}

	writeJSON(writer, status, outcomeResponse(outcome))
	return status, ""
}

func writeDeployError(writer http.ResponseWriter, outcome deploy.Outcome, err error) (int, string) {
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
	return status, code
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

func writeContextError(writer http.ResponseWriter, ctx context.Context) (int, string, bool) {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		writeError(writer, http.StatusGatewayTimeout, "request_timeout", "Deployment request timed out")
		return http.StatusGatewayTimeout, "request_timeout", true
	case context.Canceled:
		writeError(writer, http.StatusRequestTimeout, "request_cancelled", "Deployment request was cancelled")
		return http.StatusRequestTimeout, "request_cancelled", true
	default:
		return 0, "", false
	}
}

type deploymentAudit struct {
	method        string
	deployment    string
	status        int
	errorCode     string
	authenticated bool
	repositoryID  string
	workflowRef   string
	environment   string
	digest        string
	action        deploy.Action
	conclusion    swarm.Conclusion
	startedAt     time.Time
}

func (handler *handler) writeAudit(ctx context.Context, audit deploymentAudit) {
	attributes := []any{
		"event", "deployment_request",
		"method", audit.method,
		"deployment", audit.deployment,
		"status", audit.status,
		"authenticated", audit.authenticated,
		"duration_ms", time.Since(audit.startedAt).Milliseconds(),
	}
	if audit.errorCode != "" {
		attributes = append(attributes, "error_code", audit.errorCode)
	}
	if audit.authenticated {
		attributes = append(attributes,
			"repository_id", audit.repositoryID,
			"workflow_ref", audit.workflowRef,
			"environment", audit.environment,
		)
	}
	if audit.digest != "" {
		attributes = append(attributes, "digest", audit.digest)
	}
	if audit.action != "" {
		attributes = append(attributes, "action", audit.action)
	}
	if audit.conclusion != "" {
		attributes = append(attributes, "conclusion", audit.conclusion)
	}

	handler.logger.InfoContext(ctx, "deployment request", attributes...)
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
