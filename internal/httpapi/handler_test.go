package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aide-tools/swarrow/internal/deploy"
	"github.com/aide-tools/swarrow/internal/githuboidc"
	"github.com/aide-tools/swarrow/internal/httpapi"
	"github.com/aide-tools/swarrow/internal/policy"
	"github.com/aide-tools/swarrow/internal/replay"
	"github.com/aide-tools/swarrow/internal/swarm"
)

const canonicalDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHealth(t *testing.T) {
	handler := newHandler(t, &fakeVerifier{}, &fakeDeployer{}, time.Minute)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("health response = (%d, %q), want 200 status", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("health security headers = %#v", response.Header())
	}
}

func TestHealthSupportsHeadWithoutABody(t *testing.T) {
	handler := newHandler(t, &fakeVerifier{}, &fakeDeployer{}, time.Minute)
	request := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Errorf("health response = (%d, %q), want empty 200 response", response.Code, response.Body.String())
	}
}

func TestDeploymentAuthenticatesAndForwardsTheBoundedRequest(t *testing.T) {
	claims := githuboidc.Claims{TokenID: "token-id"}
	verifier := &fakeVerifier{verify: func(ctx context.Context, token string) (githuboidc.Claims, error) {
		if token != "signed-token" {
			t.Errorf("Verify() token = %q, want signed-token", token)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 50*time.Second {
			t.Errorf("Verify() deadline = %v, want request timeout", deadline)
		}
		return claims, nil
	}}
	deployer := &fakeDeployer{deploy: func(_ context.Context, request deploy.Request) (deploy.Outcome, error) {
		if request.Deployment != "example-web" || request.Digest != canonicalDigest || request.Claims != claims {
			t.Errorf("Deploy() request = %#v, want path, digest and verified claims", request)
		}
		return completedOutcome(), nil
	}}
	handler := newHandler(t, verifier, deployer, time.Minute)
	request := deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("deployment status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "service-id") || strings.Contains(response.Body.String(), "42") {
		t.Errorf("deployment body exposes internal Docker identity: %s", response.Body.String())
	}
	for _, expected := range []string{`"deployment":"example-web"`, `"action":"updated"`, `"conclusion":"completed"`, `"running":2`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("deployment body %q does not contain %q", response.Body.String(), expected)
		}
	}
}

func TestDeploymentRequiresOneBearerToken(t *testing.T) {
	tests := map[string][]string{
		"missing":          nil,
		"empty":            {"Bearer "},
		"wrong scheme":     {"Basic token"},
		"leading space":    {" Bearer token"},
		"multiple spaces":  {"Bearer token extra"},
		"multiple headers": {"Bearer one", "Bearer two"},
	}

	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			verifier := &fakeVerifier{}
			handler := newHandler(t, verifier, &fakeDeployer{}, time.Minute)
			request := deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`)
			request.Header.Del("Authorization")
			for _, value := range values {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", response.Code)
			}
			if verifier.calls != 0 {
				t.Errorf("Verify() calls = %d, want 0", verifier.calls)
			}
		})
	}
}

func TestDeploymentRejectsInvalidBodiesBeforeCoordination(t *testing.T) {
	tests := map[string]struct {
		contentType string
		body        string
	}{
		"missing content type": {body: `{"digest":"` + canonicalDigest + `"}`},
		"wrong content type":   {contentType: "text/plain", body: `{"digest":"` + canonicalDigest + `"}`},
		"not an object":        {contentType: "application/json", body: `[]`},
		"missing digest":       {contentType: "application/json", body: `{}`},
		"unknown field":        {contentType: "application/json", body: `{"digest":"` + canonicalDigest + `","service":"other"}`},
		"duplicate digest":     {contentType: "application/json", body: `{"digest":"` + canonicalDigest + `","digest":"` + canonicalDigest + `"}`},
		"trailing value":       {contentType: "application/json", body: `{"digest":"` + canonicalDigest + `"} {}`},
		"tag":                  {contentType: "application/json", body: `{"digest":"latest"}`},
		"different algorithm":  {contentType: "application/json", body: `{"digest":"sha512:0123456789abcdef"}`},
		"oversized":            {contentType: "application/json", body: `{"digest":"` + canonicalDigest + `"}` + strings.Repeat(" ", 1024)},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			deployer := &fakeDeployer{}
			handler := newHandler(t, &fakeVerifier{verify: verified}, deployer, time.Minute)
			request := deploymentRequest(http.MethodPost, test.body)
			if test.contentType == "" {
				request.Header.Del("Content-Type")
			} else {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
			if deployer.calls != 0 {
				t.Errorf("Deploy() calls = %d, want 0", deployer.calls)
			}
		})
	}
}

func TestDeploymentDoesNotExposeRejectedTokens(t *testing.T) {
	verifier := &fakeVerifier{verify: func(context.Context, string) (githuboidc.Claims, error) {
		return githuboidc.Claims{}, errors.New("token signed-token is expired")
	}}
	handler := newHandler(t, verifier, &fakeDeployer{}, time.Minute)
	request := deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if strings.Contains(response.Body.String(), "signed-token") || strings.Contains(response.Body.String(), "expired") {
		t.Errorf("response exposes verifier detail: %s", response.Body.String())
	}
}

func TestDeploymentReportsRestartWarmupAsRetryable(t *testing.T) {
	deployer := &fakeDeployer{}
	verifier := &fakeVerifier{verify: func(context.Context, string) (githuboidc.Claims, error) {
		return githuboidc.Claims{}, retryableVerificationError{retryAfter: 1500 * time.Millisecond}
	}}
	handler := newHandler(t, verifier, deployer, time.Minute)
	request := deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if response.Header().Get("Retry-After") != "2" {
		t.Errorf("Retry-After = %q, want 2", response.Header().Get("Retry-After"))
	}
	if !strings.Contains(response.Body.String(), `"code":"authentication_warming_up"`) {
		t.Errorf("body = %q, want warm-up error", response.Body.String())
	}
	if deployer.calls != 0 {
		t.Errorf("Deploy() calls = %d, want 0", deployer.calls)
	}
}

func TestDeploymentWritesStructuredAuditEventsWithoutCredentials(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	claims := githuboidc.Claims{
		TokenID:      "secret-token-id",
		RepositoryID: "123456789",
		WorkflowRef:  "example/example/.github/workflows/deploy.yml@refs/heads/main",
		Environment:  "production",
	}
	handler, err := httpapi.New(
		&fakeVerifier{verify: func(context.Context, string) (githuboidc.Claims, error) { return claims, nil }},
		&fakeDeployer{deploy: returns(completedOutcome(), nil)},
		time.Minute,
		logger,
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`))

	logEntry := output.String()
	for _, expected := range []string{
		`"event":"deployment_request"`,
		`"deployment":"example-web"`,
		`"status":200`,
		`"authenticated":true`,
		`"repository_id":"123456789"`,
		`"environment":"production"`,
		`"digest":"` + canonicalDigest + `"`,
		`"action":"updated"`,
		`"conclusion":"completed"`,
	} {
		if !strings.Contains(logEntry, expected) {
			t.Errorf("audit event %q does not contain %q", logEntry, expected)
		}
	}
	for _, forbidden := range []string{"signed-token", "secret-token-id", "service-id"} {
		if strings.Contains(logEntry, forbidden) {
			t.Errorf("audit event exposes %q: %s", forbidden, logEntry)
		}
	}
}

func TestDeploymentMapsConclusions(t *testing.T) {
	tests := map[swarm.Conclusion]int{
		swarm.ConclusionCompleted:  http.StatusOK,
		swarm.ConclusionInProgress: http.StatusAccepted,
		swarm.ConclusionSuperseded: http.StatusConflict,
		swarm.ConclusionFailed:     http.StatusBadGateway,
		swarm.ConclusionRolledBack: http.StatusBadGateway,
	}
	for conclusion, status := range tests {
		t.Run(string(conclusion), func(t *testing.T) {
			outcome := completedOutcome()
			outcome.Conclusion = conclusion
			handler := newHandler(t, &fakeVerifier{verify: verified}, &fakeDeployer{deploy: returns(outcome, nil)}, time.Minute)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`))

			if response.Code != status {
				t.Errorf("status = %d, want %d", response.Code, status)
			}
		})
	}
}

func TestDeploymentMapsOperationalErrors(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"denied":          {err: policy.ErrDenied, status: http.StatusForbidden, code: "forbidden"},
		"reused token":    {err: replay.ErrReused, status: http.StatusForbidden, code: "forbidden"},
		"replay capacity": {err: replay.ErrCapacity, status: http.StatusTooManyRequests, code: "capacity_exhausted"},
		"queue capacity":  {err: deploy.ErrQueueFull, status: http.StatusTooManyRequests, code: "capacity_exhausted"},
		"conflict":        {err: swarm.ErrConflict, status: http.StatusConflict, code: "service_conflict"},
		"timeout":         {err: context.DeadlineExceeded, status: http.StatusGatewayTimeout, code: "request_timeout"},
		"cancelled":       {err: context.Canceled, status: http.StatusRequestTimeout, code: "request_cancelled"},
		"expired binding": {err: replay.ErrInvalidBinding, status: http.StatusUnauthorized, code: "unauthorized"},
		"docker failure":  {err: errors.New("daemon unavailable"), status: http.StatusBadGateway, code: "deployment_error"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			outcome := completedOutcome()
			outcome.Action = deploy.ActionNotApplied
			outcome.Conclusion = ""
			handler := newHandler(t, &fakeVerifier{verify: verified}, &fakeDeployer{deploy: returns(outcome, test.err)}, time.Minute)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`))

			if response.Code != test.status {
				t.Errorf("status = %d, want %d", response.Code, test.status)
			}
			if !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Errorf("body = %q, want generic %q error", response.Body.String(), test.code)
			}
			if name == "docker failure" && strings.Contains(response.Body.String(), test.err.Error()) {
				t.Errorf("body exposes internal deployment error: %q", response.Body.String())
			}
		})
	}
}

func TestDeploymentMapsAuthenticationTimeout(t *testing.T) {
	verifier := &fakeVerifier{verify: func(ctx context.Context, _ string) (githuboidc.Claims, error) {
		<-ctx.Done()
		return githuboidc.Claims{}, ctx.Err()
	}}
	handler := newHandler(t, verifier, &fakeDeployer{}, time.Millisecond)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, deploymentRequest(http.MethodPost, `{"digest":"`+canonicalDigest+`"}`))

	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), `"code":"request_timeout"`) {
		t.Errorf("response = (%d, %q), want timeout", response.Code, response.Body.String())
	}
}

func TestDeploymentTimeoutStopsReadingTheRequestBody(t *testing.T) {
	body := &blockingBody{reading: make(chan struct{}), closed: make(chan struct{})}
	handler := newHandler(t, &fakeVerifier{verify: verified}, &fakeDeployer{}, 10*time.Millisecond)
	request := deploymentRequest(http.MethodPost, "")
	request.Body = body
	response := httptest.NewRecorder()
	served := make(chan struct{})

	go func() {
		handler.ServeHTTP(response, request)
		close(served)
	}()

	select {
	case <-body.reading:
	case <-time.After(time.Second):
		t.Fatal("handler did not start reading the request body")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop reading after the request timeout")
	}

	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), `"code":"request_timeout"`) {
		t.Errorf("response = (%d, %q), want timeout", response.Code, response.Body.String())
	}
}

func TestRoutesRejectUnsupportedMethodsAndPaths(t *testing.T) {
	handler := newHandler(t, &fakeVerifier{}, &fakeDeployer{}, time.Minute)
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodPost, path: "/healthz", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/v1/deployments/example-web", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/unknown", status: http.StatusNotFound},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.status {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.status)
		}
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	if _, err := httpapi.New(nil, &fakeDeployer{}, time.Minute, slog.Default()); !errors.Is(err, httpapi.ErrInvalidConfiguration) {
		t.Errorf("New() error = %v, want ErrInvalidConfiguration", err)
	}
}

func newHandler(t *testing.T, verifier *fakeVerifier, deployer *fakeDeployer, timeout time.Duration) http.Handler {
	t.Helper()
	handler, err := httpapi.New(verifier, deployer, timeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return handler
}

func deploymentRequest(method string, body string) *http.Request {
	request := httptest.NewRequest(method, "/v1/deployments/example-web", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer signed-token")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func verified(context.Context, string) (githuboidc.Claims, error) {
	return githuboidc.Claims{TokenID: "token-id"}, nil
}

func returns(outcome deploy.Outcome, err error) func(context.Context, deploy.Request) (deploy.Outcome, error) {
	return func(context.Context, deploy.Request) (deploy.Outcome, error) { return outcome, err }
}

func completedOutcome() deploy.Outcome {
	return deploy.Outcome{
		Deployment:       "example-web",
		Action:           deploy.ActionUpdated,
		Conclusion:       swarm.ConclusionCompleted,
		ServiceID:        "service-id",
		Image:            "ghcr.io/example/example-web@" + canonicalDigest,
		InspectedVersion: 42,
		Message:          "completed",
		Tasks:            swarm.TaskSummary{Running: 2},
	}
}

type fakeVerifier struct {
	calls  int
	verify func(context.Context, string) (githuboidc.Claims, error)
}

type retryableVerificationError struct {
	retryAfter time.Duration
}

func (err retryableVerificationError) Error() string {
	return "restart warm-up"
}

func (err retryableVerificationError) RetryAfter() time.Duration {
	return err.retryAfter
}

func (verifier *fakeVerifier) Verify(ctx context.Context, token string) (githuboidc.Claims, error) {
	verifier.calls++
	if verifier.verify == nil {
		return githuboidc.Claims{}, errors.New("unexpected Verify call")
	}
	return verifier.verify(ctx, token)
}

type fakeDeployer struct {
	calls  int
	deploy func(context.Context, deploy.Request) (deploy.Outcome, error)
}

type blockingBody struct {
	reading     chan struct{}
	closed      chan struct{}
	readingOnce sync.Once
	closeOnce   sync.Once
}

func (body *blockingBody) Read([]byte) (int, error) {
	body.readingOnce.Do(func() { close(body.reading) })
	<-body.closed
	return 0, errors.New("request body closed")
}

func (body *blockingBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}

func (deployer *fakeDeployer) Deploy(ctx context.Context, request deploy.Request) (deploy.Outcome, error) {
	deployer.calls++
	if deployer.deploy == nil {
		return deploy.Outcome{}, errors.New("unexpected Deploy call")
	}
	return deployer.deploy(ctx, request)
}
