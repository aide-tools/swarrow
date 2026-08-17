// Package githuboidc authenticates identity tokens issued to GitHub Actions jobs.
package githuboidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// Issuer is GitHub.com's canonical Actions OpenID Connect issuer.
	Issuer = "https://token.actions.githubusercontent.com"

	// ClockSkew is the fixed allowance for differences between GitHub's clock and
	// the Swarrow host clock.
	ClockSkew = 30 * time.Second

	defaultHTTPTimeout = 10 * time.Second
)

// Options configures GitHub token verification.
type Options struct {
	Audience   string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Claims are authenticated facts about one GitHub Actions job.
type Claims struct {
	TokenID               string
	Subject               string
	RepositoryID          string
	Repository            string
	WorkflowRef           string
	Environment           string
	JobWorkflowRef        string
	JobWorkflowRefPresent bool
	IssuedAt              time.Time
	NotBefore             time.Time
	ExpiresAt             time.Time
	ValidUntil            time.Time
}

// Verifier authenticates GitHub Actions identity tokens for one audience.
type Verifier struct {
	tokenVerifier tokenVerifier
	audience      string
	now           func() time.Time
	restartCutoff time.Time
}

type tokenVerifier interface {
	Verify(context.Context, string) (*oidc.IDToken, error)
}

// New discovers GitHub's OpenID Connect metadata and constructs a verifier.
func New(ctx context.Context, options Options) (*Verifier, error) {
	if options.Audience == "" {
		return nil, errors.New("create GitHub OIDC verifier: audience must not be empty")
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}
	startedAt := now()

	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	providerContext := oidc.ClientContext(ctx, httpClient)
	provider, err := oidc.NewProvider(providerContext, Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover GitHub OIDC provider: %w", err)
	}

	underlying := provider.VerifierContext(providerContext, &oidc.Config{
		ClientID:             options.Audience,
		SupportedSigningAlgs: []string{"RS256"},
		// Swarrow validates exp, nbf and iat together with one explicit clock
		// allowance below. go-oidc otherwise applies a separate five-minute nbf
		// allowance and no expiry allowance.
		SkipExpiryCheck: true,
	})

	return newVerifier(underlying, options.Audience, now, startedAt), nil
}

func newVerifier(underlying tokenVerifier, audience string, now func() time.Time, startedAt time.Time) *Verifier {
	return &Verifier{
		tokenVerifier: underlying,
		audience:      audience,
		now:           now,
		restartCutoff: startedAt.Truncate(time.Second).Add(time.Second + ClockSkew),
	}
}

// Verify authenticates a token and returns its required GitHub Actions claims.
func (verifier *Verifier) Verify(ctx context.Context, rawToken string) (Claims, error) {
	if rawToken == "" {
		return Claims{}, errors.New("verify GitHub OIDC token: token must not be empty")
	}

	token, err := verifier.tokenVerifier.Verify(ctx, rawToken)
	if err != nil {
		return Claims{}, fmt.Errorf("verify GitHub OIDC token: %w", err)
	}

	if len(token.Audience) != 1 || token.Audience[0] != verifier.audience {
		return Claims{}, errors.New("verify GitHub OIDC token: audience must exactly match the configured audience")
	}

	var raw rawClaims
	if err := token.Claims(&raw); err != nil {
		return Claims{}, fmt.Errorf("verify GitHub OIDC token: decode claims: %w", err)
	}

	claims, err := raw.validate(verifier.now(), verifier.restartCutoff)
	if err != nil {
		return Claims{}, fmt.Errorf("verify GitHub OIDC token: %w", err)
	}

	return claims, nil
}

type rawClaims struct {
	Subject        *string        `json:"sub"`
	ExpiresAt      *int64         `json:"exp"`
	IssuedAt       *int64         `json:"iat"`
	NotBefore      *int64         `json:"nbf"`
	TokenID        *string        `json:"jti"`
	RepositoryID   *string        `json:"repository_id"`
	Repository     string         `json:"repository"`
	WorkflowRef    *string        `json:"workflow_ref"`
	Environment    *string        `json:"environment"`
	JobWorkflowRef optionalString `json:"job_workflow_ref"`
}

func (raw rawClaims) validate(now time.Time, restartCutoff time.Time) (Claims, error) {
	subject, err := requiredString("sub", raw.Subject)
	if err != nil {
		return Claims{}, err
	}
	tokenID, err := requiredString("jti", raw.TokenID)
	if err != nil {
		return Claims{}, err
	}
	repositoryID, err := requiredString("repository_id", raw.RepositoryID)
	if err != nil {
		return Claims{}, err
	}
	workflowRef, err := requiredString("workflow_ref", raw.WorkflowRef)
	if err != nil {
		return Claims{}, err
	}
	environment, err := requiredString("environment", raw.Environment)
	if err != nil {
		return Claims{}, err
	}

	expiresAt, err := requiredTime("exp", raw.ExpiresAt)
	if err != nil {
		return Claims{}, err
	}
	issuedAt, err := requiredTime("iat", raw.IssuedAt)
	if err != nil {
		return Claims{}, err
	}
	notBefore, err := requiredTime("nbf", raw.NotBefore)
	if err != nil {
		return Claims{}, err
	}

	validUntil := expiresAt.Add(ClockSkew)
	if !expiresAt.After(issuedAt) {
		return Claims{}, errors.New("exp must be after iat")
	}
	if !expiresAt.After(notBefore) {
		return Claims{}, errors.New("exp must be after nbf")
	}
	if !now.Before(validUntil) {
		return Claims{}, errors.New("token has expired")
	}
	if now.Add(ClockSkew).Before(notBefore) {
		return Claims{}, errors.New("token is not valid yet")
	}
	if now.Add(ClockSkew).Before(issuedAt) {
		return Claims{}, errors.New("iat is too far in the future")
	}
	if issuedAt.Before(restartCutoff) {
		return Claims{}, errors.New("iat is before the current process restart cutoff")
	}

	return Claims{
		TokenID:               tokenID,
		Subject:               subject,
		RepositoryID:          repositoryID,
		Repository:            raw.Repository,
		WorkflowRef:           workflowRef,
		Environment:           environment,
		JobWorkflowRef:        raw.JobWorkflowRef.value,
		JobWorkflowRefPresent: raw.JobWorkflowRef.present,
		IssuedAt:              issuedAt,
		NotBefore:             notBefore,
		ExpiresAt:             expiresAt,
		ValidUntil:            validUntil,
	}, nil
}

func requiredString(name string, value *string) (string, error) {
	if value == nil || *value == "" {
		return "", fmt.Errorf("%s claim must be a non-empty string", name)
	}

	return *value, nil
}

func requiredTime(name string, value *int64) (time.Time, error) {
	if value == nil {
		return time.Time{}, fmt.Errorf("%s claim must be an integer NumericDate", name)
	}

	return time.Unix(*value, 0), nil
}

type optionalString struct {
	value   string
	present bool
}

func (value *optionalString) UnmarshalJSON(data []byte) error {
	value.present = true
	if bytes.Equal(data, []byte("null")) {
		return errors.New("must be a string")
	}

	return json.Unmarshal(data, &value.value)
}
