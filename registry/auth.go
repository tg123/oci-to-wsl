package registry

import (
	"context"
	"fmt"
	"log/slog"
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
// Authentication flow (delegated to the Azure SDK):
//  1. azidentity.AzureCLICredential – reuses an existing `az login` session.
//  2. azidentity.InteractiveBrowserCredential – opens the default browser
//     (no device-code copy/paste needed) when the CLI credential is unavailable.
//
// The resulting AAD token is exchanged for an ACR refresh token and then a
// scoped access token via azcontainerregistry.AuthenticationClient. ACR infers
// the tenant from the AAD token's tid claim, so no explicit tenant is required
// (cross-tenant guest access works automatically).
func NewACRAuthenticator(registry string) (authn.Authenticator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	slog.Debug("ACR auth: building Azure credential chain", "registry", registry)
	cred, err := newAzureCredential()
	if err != nil {
		return nil, fmt.Errorf("building Azure credential: %w", err)
	}

	slog.Debug("ACR auth: acquiring AAD token", "scope", aadScope)
	tokStart := time.Now()
	aadToken, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{aadScope}})
	if err != nil {
		return nil, fmt.Errorf("acquiring AAD token: %w", err)
	}
	slog.Debug("ACR auth: AAD token acquired",
		"duration", time.Since(tokStart),
		"expires_on", aadToken.ExpiresOn,
	)

	authClient, err := azcontainerregistry.NewAuthenticationClient("https://"+registry, nil)
	if err != nil {
		return nil, fmt.Errorf("creating ACR auth client: %w", err)
	}

	slog.Debug("ACR auth: exchanging AAD token for ACR refresh token", "registry", registry)
	exchangeStart := time.Now()
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
		return nil, fmt.Errorf("exchanging AAD token for ACR refresh token: %w", err)
	}
	if refreshResp.RefreshToken == nil {
		return nil, fmt.Errorf("ACR refresh token response was empty")
	}
	slog.Debug("ACR auth: ACR refresh token obtained", "duration", time.Since(exchangeStart))

	const scope = "repository:*:pull"
	slog.Debug("ACR auth: exchanging refresh token for access token", "registry", registry, "scope", scope)
	accessStart := time.Now()
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
	slog.Debug("ACR auth: ACR access token obtained", "duration", time.Since(accessStart))

	return &acrAuthenticator{accessToken: *accessResp.AccessToken}, nil
}

// newAzureCredential builds a credential chain that first tries the Azure CLI
// (so users who have already run `az login` skip any prompting) and falls back
// to an interactive browser login.
func newAzureCredential() (azcore.TokenCredential, error) {
	var sources []azcore.TokenCredential

	if cli, err := azidentity.NewAzureCLICredential(nil); err == nil {
		slog.Debug("ACR auth: Azure CLI credential available")
		sources = append(sources, cli)
	} else {
		slog.Debug("ACR auth: Azure CLI credential unavailable", "error", err)
	}

	browser, err := azidentity.NewInteractiveBrowserCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("creating interactive browser credential: %w", err)
	}
	slog.Debug("ACR auth: interactive browser credential added to chain")
	sources = append(sources, browser)

	return azidentity.NewChainedTokenCredential(sources, nil)
}
