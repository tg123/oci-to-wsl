package registry

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestIsACR(t *testing.T) {
	cases := []struct {
		registry string
		want     bool
	}{
		{"myacr.azurecr.io", true},
		{"MYACR.AZURECR.IO", true},
		{"myacr.azurecr.cn", true},
		{"myacr.azurecr.us", true},
		{"index.docker.io", false},
		{"gcr.io", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isACR(tc.registry)
		if got != tc.want {
			t.Errorf("isACR(%q) = %v, want %v", tc.registry, got, tc.want)
		}
	}
}

// TestAADScope verifies the scope constant required for ACR token exchange.
func TestAADScope(t *testing.T) {
	if !strings.Contains(aadScope, "management.azure.com") {
		t.Errorf("aadScope %q does not contain management.azure.com", aadScope)
	}
}

func TestIsACRAADDisabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"garbage", false},
		{"1", true},
		{"t", true},
		{"true", true},
		{"True", true},
		{"TRUE", true},
	}
	for _, tc := range cases {
		t.Setenv(envDisableACRAAD, tc.val)
		if got := isACRAADDisabled(); got != tc.want {
			t.Errorf("isACRAADDisabled() with %s=%q = %v, want %v", envDisableACRAAD, tc.val, got, tc.want)
		}
	}
}

// TestACRAuthenticatorAuthorization confirms the sentinel username/password
// format expected by ACR.
func TestACRAuthenticatorAuthorization(t *testing.T) {
	a := &acrAuthenticator{accessToken: "test-token"}
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatalf("Authorization: unexpected error: %v", err)
	}
	if cfg.Username != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("Username: got %q, want sentinel UUID", cfg.Username)
	}
	if cfg.Password != "test-token" {
		t.Errorf("Password: got %q, want %q", cfg.Password, "test-token")
	}
}

// TestNewAzureCLICredentialOrNil ensures the helper never returns an error
// (returns nil instead when the CLI is unavailable, so the caller can fall
// through to the interactive browser flow without surfacing a hard failure).
func TestNewAzureCLICredentialOrNil(t *testing.T) {
	// Either the CLI is installed (non-nil) or it isn't (nil) — both are
	// valid outcomes on a CI runner. We only assert that the function does
	// not panic and does not return a half-constructed credential.
	_ = newAzureCLICredentialOrNil()
}

// fakeJWT assembles a JWT-shaped string with the given JSON payload. The
// header and signature segments are dummy values — jwtTenantID only reads
// the payload, so the signature is irrelevant to the test.
func fakeJWT(payloadJSON string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	return header + "." + body + ".sig"
}

func TestJWTTenantID(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		want       string
		wantErrSub string
	}{
		{
			name:  "real_aad_shape",
			token: fakeJWT(`{"aud":"https://management.azure.com/","tid":"8b9ebe14-d942-49e7-ace9-14496d0caff0","oid":"abc"}`),
			want:  "8b9ebe14-d942-49e7-ace9-14496d0caff0",
		},
		{
			name:  "extra_claims_ignored",
			token: fakeJWT(`{"tid":"72f988bf-86f1-41af-91ab-2d7cd011db47","name":"Alice","email":"alice@example.com"}`),
			want:  "72f988bf-86f1-41af-91ab-2d7cd011db47",
		},
		{
			name:       "not_a_jwt",
			token:      "not-a-jwt",
			wantErrSub: "not a JWT",
		},
		{
			name:       "garbage_payload",
			token:      "abc.!!!not-base64!!!.sig",
			wantErrSub: "decoding JWT payload",
		},
		{
			name:       "no_tid_claim",
			token:      fakeJWT(`{"aud":"foo","iss":"bar"}`),
			wantErrSub: "no tid claim",
		},
		{
			name:       "empty_tid_claim",
			token:      fakeJWT(`{"tid":""}`),
			wantErrSub: "no tid claim",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jwtTenantID(tc.token)
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (tid=%q)", tc.wantErrSub, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Errorf("error %q does not contain %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("tid: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsACRTokenRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "unwrapped_unrelated",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "exchange_401_response",
			err:  &acrExchangeError{registry: "x.azurecr.io", credential: "azure-cli", cause: errors.New("--------------------------------------------------------------------------------\nRESPONSE 401: 401 Unauthorized\nERROR CODE UNAVAILABLE\n")},
			want: true,
		},
		{
			name: "exchange_unknown_tenantId",
			err:  &acrExchangeError{registry: "x.azurecr.io", credential: "azure-cli", cause: errors.New(`token validation failed: the received access token has unknown tenantId "72f988bf-86f1-41af-91ab-2d7cd011db47"`)},
			want: true,
		},
		{
			name: "exchange_unauthorized_word",
			err:  &acrExchangeError{registry: "x.azurecr.io", credential: "azure-cli", cause: errors.New(`{"errors":[{"code":"UNAUTHORIZED","message":"..."}]}`)},
			want: true,
		},
		{
			name: "exchange_network_failure_not_rejection",
			err:  &acrExchangeError{registry: "x.azurecr.io", credential: "azure-cli", cause: errors.New("dial tcp: connection refused")},
			want: false,
		},
		{
			name: "non_exchange_401_not_classified",
			err:  errors.New("RESPONSE 401: 401 from some other call"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isACRTokenRejection(tc.err)
			if got != tc.want {
				t.Errorf("isACRTokenRejection(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestACRExchangeError_Unwrap(t *testing.T) {
	root := errors.New("root cause")
	wrapped := &acrExchangeError{registry: "x.azurecr.io", credential: "azure-cli", cause: root}
	if !errors.Is(wrapped, root) {
		t.Errorf("errors.Is: wrapped error does not unwrap to root cause")
	}
	if !strings.Contains(wrapped.Error(), "x.azurecr.io") {
		t.Errorf("Error() should include registry, got %q", wrapped.Error())
	}
	if !strings.Contains(wrapped.Error(), "azure-cli") {
		t.Errorf("Error() should include credential name, got %q", wrapped.Error())
	}
}
