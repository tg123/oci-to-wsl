package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	"github.com/google/go-containerregistry/pkg/authn"
)

// aadScope is the scope used when requesting an AAD token to exchange for an
// ACR refresh token. ACR requires a token scoped to the ARM resource.
const aadScope = "https://management.azure.com/.default"

// envDisableACRAAD, when set to a value parseable as true by strconv.ParseBool
// (e.g. "1", "t", "true", "True", "TRUE"), disables AAD-based ACR
// authentication entirely. Pulls against *.azurecr.io/.cn/.us hosts then go
// through the normal docker keychain like any other registry, letting
// operators supply a username/password (or token) via `docker login` /
// $DOCKER_CONFIG instead of acquiring an AAD token.
const envDisableACRAAD = "OCI_TO_WSL_NO_ACR_AAD"

// isACRAADDisabled reports whether the user has opted out of AAD-based ACR
// authentication via the OCI_TO_WSL_NO_ACR_AAD environment variable. The
// value is parsed with strconv.ParseBool, so any of 1/t/T/TRUE/true/True
// (and their false-y counterparts) are accepted.
func isACRAADDisabled() bool {
	v, err := strconv.ParseBool(os.Getenv(envDisableACRAAD))
	if err != nil {
		return false
	}
	return v
}

// acrAuthenticator implements authn.Authenticator for Azure Container Registry.
type acrAuthenticator struct {
	accessToken string
}

// Authorization returns the bearer token used to authenticate to ACR.
// ACR uses a fixed sentinel username with the access token as the password.
func (a *acrAuthenticator) Authorization() (*authn.AuthConfig, error) {
	return &authn.AuthConfig{
		Username: "00000000-0000-0000-0000-000000000000",
		Password: a.accessToken,
	}, nil
}

// NewACRAuthenticator builds an authn.Authenticator for the given ACR endpoint
// using the official Azure SDK for Go.
//
// Authentication flow:
//  1. azidentity.AzureCLICredential — reuses an existing `az login` session
//     (silent, no prompting). The CLI token's tid claim is logged so operators
//     can see which tenant the token was issued from.
//  2. If the AAD-token-to-ACR-refresh-token exchange returns 401, the most
//     likely cause is that the CLI's tenant doesn't match the registry's
//     tenant (e.g. `az login` was for a different AAD tenant than the one
//     that owns the registry). The error is detected, a tenant-mismatch
//     diagnostic is logged, and the flow retries with
//     azidentity.InteractiveBrowserCredential so the operator can pick the
//     correct tenant in the browser.
//  3. If the CLI is unavailable (not installed, no active subscription, etc.),
//     it is skipped entirely and the interactive browser flow runs first.
//
// ChainedTokenCredential is deliberately NOT used here because it only falls
// through to the next source when GetToken returns an `azidentity.credentialUnavailableError`
// — a successful GetToken that produces a wrong-tenant token (the common case
// when an operator is multi-tenant) does not trigger fallthrough, so a 401 at
// the ACR exchange step is the right trigger.
func NewACRAuthenticator(registry string) (authn.Authenticator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Try the Azure CLI first when available so users who have already run
	// `az login` don't need to do anything interactive.
	if cli := newAzureCLICredentialOrNil(); cli != nil {
		auth, err := tryACRAuth(ctx, registry, cli, "azure-cli")
		if err == nil {
			return auth, nil
		}
		var ax *acrExchangeError
		switch {
		case errors.As(err, &ax) && isACRTokenRejection(err):
			// ACR rejected the CLI's AAD token (most likely a tenant
			// mismatch). Fall through to interactive browser sign-in so
			// the operator can pick the correct tenant.
			slog.Info("az CLI token rejected by ACR (likely tenant mismatch), switching to interactive browser sign-in",
				"registry", registry,
				"hint", "sign in with an account that has access to the registry's AAD tenant",
			)
		case errors.As(err, &ax):
			// Exchange call reached ACR but failed for a non-auth reason
			// (transport failure, registry not found, etc.). Opening a
			// browser won't help — surface the original error.
			return nil, err
		default:
			// AAD token acquisition itself failed (CLI not installed,
			// user not logged in, no active subscription, etc.). Treat
			// as a soft failure and fall through to the interactive
			// browser flow so the operator can complete sign-in.
			slog.Info("az CLI could not acquire an AAD token, switching to interactive browser sign-in",
				"registry", registry,
				"error", err,
			)
		}
	}

	browser, err := azidentity.NewInteractiveBrowserCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("creating interactive browser credential: %w", err)
	}
	return tryACRAuth(ctx, registry, browser, "interactive-browser")
}

// tryACRAuth performs the full AAD-token -> ACR-refresh-token -> ACR-access-token
// exchange using a single credential source. credName is used purely for
// logging so operators can correlate which source produced a given token.
func tryACRAuth(ctx context.Context, registry string, cred azcore.TokenCredential, credName string) (authn.Authenticator, error) {
	slog.Debug("ACR auth: acquiring AAD token", "credential", credName, "scope", aadScope)
	tokStart := time.Now()
	aadToken, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{aadScope}})
	if err != nil {
		return nil, fmt.Errorf("acquiring AAD token via %s: %w", credName, err)
	}
	// Log the CLI token's tid up front so a subsequent tenant-mismatch error
	// is unambiguous about which tenant the CLI was logged into.
	if tid, terr := jwtTenantID(aadToken.Token); terr == nil {
		slog.Info("AAD token acquired",
			"credential", credName,
			"tenant_id", tid,
			"expires_on", aadToken.ExpiresOn,
		)
	} else {
		slog.Debug("AAD token acquired (tid claim unparseable)",
			"credential", credName,
			"expires_on", aadToken.ExpiresOn,
			"tid_parse_error", terr,
		)
	}

	authClient, err := azcontainerregistry.NewAuthenticationClient("https://"+registry, nil)
	if err != nil {
		return nil, fmt.Errorf("creating ACR auth client: %w", err)
	}

	slog.Debug("ACR auth: exchanging AAD token for ACR refresh token", "registry", registry, "duration_aad", time.Since(tokStart))
	exchangeOpts := &azcontainerregistry.AuthenticationClientExchangeAADAccessTokenForACRRefreshTokenOptions{
		AccessToken: &aadToken.Token,
	}
	refreshResp, err := authClient.ExchangeAADAccessTokenForACRRefreshToken(
		ctx,
		azcontainerregistry.PostContentSchemaGrantTypeAccessToken,
		registry,
		exchangeOpts,
	)
	if err != nil {
		// Wrap so callers can use isACRTokenRejection to distinguish a
		// tenant-mismatch from e.g. a network failure.
		return nil, &acrExchangeError{registry: registry, credential: credName, cause: err}
	}
	if refreshResp.RefreshToken == nil {
		return nil, fmt.Errorf("ACR refresh token response was empty")
	}

	const scope = "repository:*:pull"
	grant := azcontainerregistry.TokenGrantTypeRefreshToken
	accessResp, err := authClient.ExchangeACRRefreshTokenForACRAccessToken(
		ctx,
		registry,
		scope,
		*refreshResp.RefreshToken,
		&azcontainerregistry.AuthenticationClientExchangeACRRefreshTokenForACRAccessTokenOptions{
			GrantType: &grant,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("exchanging ACR refresh token for access token: %w", err)
	}
	if accessResp.AccessToken == nil {
		return nil, fmt.Errorf("ACR access token response was empty")
	}

	return &acrAuthenticator{accessToken: *accessResp.AccessToken}, nil
}

// newAzureCLICredentialOrNil returns an AzureCLICredential when the Azure CLI
// is installed and responsive, or nil otherwise. Returning nil rather than an
// error lets the caller cleanly fall through to the interactive browser flow
// without leaking the construction error up the stack as a hard failure.
func newAzureCLICredentialOrNil() azcore.TokenCredential {
	cli, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		slog.Debug("ACR auth: Azure CLI credential unavailable", "error", err)
		return nil
	}
	return cli
}

// acrExchangeError wraps an error from the ACR token-exchange endpoint so
// callers can identify it via errors.As without doing fragile string matching
// against the raw error message.
type acrExchangeError struct {
	registry   string
	credential string
	cause      error
}

func (e *acrExchangeError) Error() string {
	return fmt.Sprintf("exchanging AAD token (%s) for ACR refresh token at %s: %v", e.credential, e.registry, e.cause)
}

func (e *acrExchangeError) Unwrap() error { return e.cause }

// isACRTokenRejection reports whether err looks like ACR rejected the AAD
// token (HTTP 401), as opposed to a network failure, DNS error, or other
// transport-level problem. Detection works only on errors wrapped in
// *acrExchangeError (i.e. failures from the AAD-to-ACR-refresh-token
// exchange step); raw azcore.ResponseError values bubbled up from elsewhere
// are intentionally not classified here.
func isACRTokenRejection(err error) bool {
	if err == nil {
		return false
	}
	var ax *acrExchangeError
	if errors.As(err, &ax) {
		// Only the exchange step can produce a tenant-mismatch rejection;
		// network/registry-not-found errors come from the same call but
		// look different in the error message.
		msg := ax.cause.Error()
		return strings.Contains(msg, "RESPONSE 401") ||
			strings.Contains(msg, "UNAUTHORIZED") ||
			strings.Contains(msg, "unknown tenantId")
	}
	return false
}

// jwtTenantID extracts the tid claim from an AAD-issued JWT access token.
// AAD tokens are standard JWTs (header.payload.signature) where the payload
// is base64url-encoded JSON containing a "tid" string with the tenant GUID.
// The signature is NOT verified — we use this purely for diagnostic logging
// and tenant-comparison heuristics, not for authorization decisions, so a
// forged token would only be able to mislead a log line.
func jwtTenantID(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("not a JWT: expected 3 segments, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some JWTs use padded base64url; try that as a fallback so we
		// stay on the URL-safe alphabet (which standard base64 rejects
		// for the '-' and '_' characters used by base64url).
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", fmt.Errorf("decoding JWT payload: %w", err)
		}
	}
	var claims struct {
		TID string `json:"tid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parsing JWT claims: %w", err)
	}
	if claims.TID == "" {
		return "", fmt.Errorf("JWT has no tid claim")
	}
	return claims.TID, nil
}
