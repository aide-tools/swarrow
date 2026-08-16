package githuboidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const testAudience = "https://deploy.example.net"

func TestVerifierVerify(t *testing.T) {
	t.Parallel()

	key := generateKey(t)
	startedAt, now := testTimes()
	verifier := staticVerifier(&key.PublicKey, testAudience, now, startedAt)
	claims := validRawClaims(startedAt, now)

	verified, err := verifier.Verify(context.Background(), signToken(t, key, claims))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	wantIssuedAt := time.Unix(claims["iat"].(int64), 0)
	wantNotBefore := time.Unix(claims["nbf"].(int64), 0)
	wantExpiresAt := time.Unix(claims["exp"].(int64), 0)
	if verified.TokenID != "token-123" {
		t.Errorf("TokenID = %q, want %q", verified.TokenID, "token-123")
	}
	if verified.Subject != "repo:example/example-web:environment:production" {
		t.Errorf("Subject = %q", verified.Subject)
	}
	if verified.RepositoryID != "123456789" {
		t.Errorf("RepositoryID = %q", verified.RepositoryID)
	}
	if verified.Repository != "example/example-web" {
		t.Errorf("Repository = %q", verified.Repository)
	}
	if verified.WorkflowRef != "example/example-web/.github/workflows/deploy.yml@refs/heads/main" {
		t.Errorf("WorkflowRef = %q", verified.WorkflowRef)
	}
	if verified.Environment != "production" {
		t.Errorf("Environment = %q", verified.Environment)
	}
	if verified.JobWorkflowRefPresent {
		t.Error("JobWorkflowRefPresent = true, want false")
	}
	if !verified.IssuedAt.Equal(wantIssuedAt) {
		t.Errorf("IssuedAt = %v, want %v", verified.IssuedAt, wantIssuedAt)
	}
	if !verified.NotBefore.Equal(wantNotBefore) {
		t.Errorf("NotBefore = %v, want %v", verified.NotBefore, wantNotBefore)
	}
	if !verified.ExpiresAt.Equal(wantExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", verified.ExpiresAt, wantExpiresAt)
	}
	if !verified.ValidUntil.Equal(wantExpiresAt.Add(ClockSkew)) {
		t.Errorf("ValidUntil = %v, want %v", verified.ValidUntil, wantExpiresAt.Add(ClockSkew))
	}
}

func TestVerifierCapturesReusableWorkflowClaim(t *testing.T) {
	t.Parallel()

	key := generateKey(t)
	startedAt, now := testTimes()
	verifier := staticVerifier(&key.PublicKey, testAudience, now, startedAt)
	claims := validRawClaims(startedAt, now)
	claims["job_workflow_ref"] = "example/shared/.github/workflows/deploy.yml@refs/heads/main"

	verified, err := verifier.Verify(context.Background(), signToken(t, key, claims))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !verified.JobWorkflowRefPresent {
		t.Error("JobWorkflowRefPresent = false, want true")
	}
	if verified.JobWorkflowRef != claims["job_workflow_ref"] {
		t.Errorf("JobWorkflowRef = %q, want %q", verified.JobWorkflowRef, claims["job_workflow_ref"])
	}
}

func TestVerifierAllowsClockSkewBoundaries(t *testing.T) {
	t.Parallel()

	key := generateKey(t)
	startedAt, now := testTimes()
	verifier := staticVerifier(&key.PublicKey, testAudience, now, startedAt)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "expiry allowance",
			mutate: func(claims map[string]any) {
				claims["exp"] = now.Add(-ClockSkew + time.Second).Unix()
			},
		},
		{
			name: "not before allowance",
			mutate: func(claims map[string]any) {
				claims["nbf"] = now.Add(ClockSkew).Unix()
			},
		},
		{
			name: "issued at allowance",
			mutate: func(claims map[string]any) {
				claims["iat"] = now.Add(ClockSkew).Unix()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			claims := validRawClaims(startedAt, now)
			test.mutate(claims)
			if _, err := verifier.Verify(context.Background(), signToken(t, key, claims)); err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
		})
	}
}

func TestVerifierRejectsInvalidClaims(t *testing.T) {
	t.Parallel()

	key := generateKey(t)
	startedAt, now := testTimes()
	verifier := staticVerifier(&key.PublicKey, testAudience, now, startedAt)

	tests := []struct {
		name      string
		mutate    func(map[string]any)
		wantError string
	}{
		{
			name: "wrong issuer",
			mutate: func(claims map[string]any) {
				claims["iss"] = "https://attacker.example"
			},
			wantError: "different provider",
		},
		{
			name: "wrong audience",
			mutate: func(claims map[string]any) {
				claims["aud"] = "https://other.example"
			},
			wantError: "expected audience",
		},
		{
			name: "additional audience",
			mutate: func(claims map[string]any) {
				claims["aud"] = []string{testAudience, "https://other.example"}
			},
			wantError: "audience must exactly match",
		},
		{
			name: "expired",
			mutate: func(claims map[string]any) {
				claims["exp"] = now.Add(-ClockSkew).Unix()
			},
			wantError: "token has expired",
		},
		{
			name: "not valid yet",
			mutate: func(claims map[string]any) {
				claims["nbf"] = now.Add(ClockSkew + time.Second).Unix()
			},
			wantError: "token is not valid yet",
		},
		{
			name: "issued too far in future",
			mutate: func(claims map[string]any) {
				claims["iat"] = now.Add(ClockSkew + time.Second).Unix()
			},
			wantError: "iat is too far in the future",
		},
		{
			name: "issued before restart cutoff",
			mutate: func(claims map[string]any) {
				claims["iat"] = verifier.restartCutoff.Add(-time.Second).Unix()
			},
			wantError: "iat is before the current process restart cutoff",
		},
		{
			name: "expiry before issued at",
			mutate: func(claims map[string]any) {
				claims["exp"] = claims["iat"]
			},
			wantError: "exp must be after iat",
		},
		{
			name: "expiry before not before",
			mutate: func(claims map[string]any) {
				claims["nbf"] = claims["exp"]
			},
			wantError: "exp must be after nbf",
		},
		{
			name: "malformed expiry",
			mutate: func(claims map[string]any) {
				claims["exp"] = "soon"
			},
			wantError: "failed to unmarshal claims",
		},
		{
			name: "null reusable workflow",
			mutate: func(claims map[string]any) {
				claims["job_workflow_ref"] = nil
			},
			wantError: "decode claims",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			claims := validRawClaims(startedAt, now)
			test.mutate(claims)
			_, err := verifier.Verify(context.Background(), signToken(t, key, claims))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Verify() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestVerifierRejectsMissingRequiredClaims(t *testing.T) {
	t.Parallel()

	key := generateKey(t)
	startedAt, now := testTimes()
	verifier := staticVerifier(&key.PublicKey, testAudience, now, startedAt)

	for _, claim := range []string{"sub", "exp", "iat", "nbf", "jti", "repository_id", "workflow_ref", "environment"} {
		t.Run(claim, func(t *testing.T) {
			t.Parallel()

			claims := validRawClaims(startedAt, now)
			delete(claims, claim)
			_, err := verifier.Verify(context.Background(), signToken(t, key, claims))
			if err == nil || !strings.Contains(err.Error(), claim+" claim") {
				t.Fatalf("Verify() error = %v, want missing %s claim", err, claim)
			}
		})
	}
}

func TestVerifierRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	trustedKey := generateKey(t)
	untrustedKey := generateKey(t)
	startedAt, now := testTimes()
	verifier := staticVerifier(&trustedKey.PublicKey, testAudience, now, startedAt)

	_, err := verifier.Verify(context.Background(), signToken(t, untrustedKey, validRawClaims(startedAt, now)))
	if err == nil || !strings.Contains(err.Error(), "verify signature") {
		t.Fatalf("Verify() error = %v, want invalid signature", err)
	}
}

func TestVerifierRejectsEmptyToken(t *testing.T) {
	t.Parallel()

	startedAt, now := testTimes()
	verifier := newVerifier(nil, testAudience, func() time.Time { return now }, startedAt)

	_, err := verifier.Verify(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "token must not be empty") {
		t.Fatalf("Verify() error = %v, want empty token error", err)
	}
}

func TestNewRequiresAudience(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "audience must not be empty") {
		t.Fatalf("New() error = %v, want audience error", err)
	}
}

func TestNewUsesGitHubIssuerDiscovery(t *testing.T) {
	t.Parallel()

	requested := ""
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requested = request.URL.String()
			return nil, errors.New("discovery unavailable")
		}),
	}

	_, err := New(context.Background(), Options{Audience: testAudience, HTTPClient: client})
	if err == nil || !strings.Contains(err.Error(), "discover GitHub OIDC provider") {
		t.Fatalf("New() error = %v, want discovery error", err)
	}
	wantURL := Issuer + "/.well-known/openid-configuration"
	if requested != wantURL {
		t.Errorf("discovery URL = %q, want %q", requested, wantURL)
	}
}

func staticVerifier(publicKey *rsa.PublicKey, audience string, now time.Time, startedAt time.Time) *Verifier {
	underlying := oidc.NewVerifier(Issuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{publicKey}}, &oidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: []string{"RS256"},
		SkipExpiryCheck:      true,
	})

	return newVerifier(underlying, audience, func() time.Time { return now }, startedAt)
}

func testTimes() (time.Time, time.Time) {
	startedAt := time.Date(2026, time.August, 16, 12, 0, 0, 250_000_000, time.UTC)
	return startedAt, startedAt.Add(2 * time.Minute)
}

func validRawClaims(startedAt time.Time, now time.Time) map[string]any {
	restartCutoff := startedAt.Truncate(time.Second).Add(time.Second + ClockSkew)
	return map[string]any{
		"iss":           Issuer,
		"aud":           testAudience,
		"sub":           "repo:example/example-web:environment:production",
		"exp":           now.Add(5 * time.Minute).Unix(),
		"iat":           restartCutoff.Unix(),
		"nbf":           restartCutoff.Unix(),
		"jti":           "token-123",
		"repository_id": "123456789",
		"repository":    "example/example-web",
		"workflow_ref":  "example/example-web/.github/workflows/deploy.yml@refs/heads/main",
		"environment":   "production",
	}
}

func generateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()

	header, err := json.Marshal(map[string]string{
		"alg": "RS256",
		"kid": "test-key",
		"typ": "JWT",
	})
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}

	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := encodedHeader + "." + encodedPayload
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
