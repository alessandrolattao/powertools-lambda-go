package integration

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	ciptypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	floci "github.com/floci-io/testcontainers-floci-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	"github.com/alessandrolattao/powertools-lambda-go/auth"
	"github.com/alessandrolattao/powertools-lambda-go/oauth2"
)

// cognito is a Cognito user pool on Floci, reached through a TLS proxy so
// that its issuer is an HTTPS URL, as the verifier requires.
type cognito struct {
	issuer    string
	tokenURL  string
	trusting  *http.Client
	client    *cip.Client
	poolID    string
	service   string // the client-credentials app client
	secret    string
	app       string // the app client users sign in with
	scope     string
	resources string
}

// newCognito starts a Floci of its own: the shared one names its issuer
// http://localhost:4566, which no verifier trusts. This one is told the
// proxy's HTTPS URL.
func newCognito(t *testing.T) *cognito {
	t.Helper()
	ctx := t.Context()
	var target atomic.Pointer[url.URL]
	proxy := httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target.Load())
			r.Out.Host = r.Out.URL.Host
		},
		ErrorLog: log.New(io.Discard, "", 0),
	})
	proxy.Config.ErrorLog = log.New(io.Discard, "", 0)
	proxy.StartTLS()
	t.Cleanup(proxy.Close)

	emulator, err := floci.Run(ctx, flociImage, testcontainers.WithEnv(map[string]string{"FLOCI_BASE_URL": proxy.URL}))
	testcontainers.CleanupContainer(t, emulator)
	require.NoError(t, err)
	endpoint, err := url.Parse(emulator.GetEndpoint())
	require.NoError(t, err)
	target.Store(endpoint)

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(emulator.GetRegion()),
		config.WithBaseEndpoint(emulator.GetEndpoint()),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(emulator.GetAccessKey(), emulator.GetSecretKey(), "")),
	)
	require.NoError(t, err)
	c := &cognito{client: cip.NewFromConfig(cfg), trusting: proxy.Client(), tokenURL: proxy.URL + "/cognito-idp/oauth2/token"}
	c.setUp(t, proxy.URL)
	return c
}

func (c *cognito) setUp(t *testing.T, base string) {
	t.Helper()
	ctx := t.Context()
	pool, err := c.client.CreateUserPool(ctx, &cip.CreateUserPoolInput{PoolName: aws.String("orders")})
	require.NoError(t, err)
	c.poolID = aws.ToString(pool.UserPool.Id)
	c.issuer = base + "/" + c.poolID

	c.resources = "https://orders.example.com"
	_, err = c.client.CreateResourceServer(ctx, &cip.CreateResourceServerInput{
		UserPoolId: aws.String(c.poolID), Identifier: aws.String(c.resources), Name: aws.String("orders"),
		Scopes: []ciptypes.ResourceServerScopeType{{ScopeName: aws.String("read"), ScopeDescription: aws.String("read")}},
	})
	require.NoError(t, err)
	c.scope = c.resources + "/read"

	service, err := c.client.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: aws.String(c.poolID), ClientName: aws.String("inventory-service"), GenerateSecret: true,
		AllowedOAuthFlows: []ciptypes.OAuthFlowType{ciptypes.OAuthFlowTypeClientCredentials}, AllowedOAuthFlowsUserPoolClient: true,
		AllowedOAuthScopes: []string{c.scope},
	})
	require.NoError(t, err)
	c.service, c.secret = aws.ToString(service.UserPoolClient.ClientId), aws.ToString(service.UserPoolClient.ClientSecret)

	app, err := c.client.CreateUserPoolClient(ctx, &cip.CreateUserPoolClientInput{
		UserPoolId: aws.String(c.poolID), ClientName: aws.String("orders-app"),
		ExplicitAuthFlows: []ciptypes.ExplicitAuthFlowsType{ciptypes.ExplicitAuthFlowsTypeAllowUserPasswordAuth},
	})
	require.NoError(t, err)
	c.app = aws.ToString(app.UserPoolClient.ClientId)

	_, err = c.client.AdminCreateUser(ctx, &cip.AdminCreateUserInput{UserPoolId: aws.String(c.poolID), Username: aws.String("ada")})
	require.NoError(t, err)
	_, err = c.client.AdminSetUserPassword(ctx, &cip.AdminSetUserPasswordInput{
		UserPoolId: aws.String(c.poolID), Username: aws.String("ada"), Password: aws.String("Passw0rd!x"), Permanent: true,
	})
	require.NoError(t, err)
}

// signIn returns the ID and access tokens of the user.
func (c *cognito) signIn(t *testing.T) (idToken, accessToken string) {
	t.Helper()
	out, err := c.client.InitiateAuth(t.Context(), &cip.InitiateAuthInput{
		ClientId: aws.String(c.app), AuthFlow: ciptypes.AuthFlowTypeUserPasswordAuth,
		AuthParameters: map[string]string{"USERNAME": "ada", "PASSWORD": "Passw0rd!x"},
	})
	require.NoError(t, err)
	return aws.ToString(out.AuthenticationResult.IdToken), aws.ToString(out.AuthenticationResult.AccessToken)
}

func TestAuthWithCognito(t *testing.T) {
	t.Parallel()
	c := newCognito(t)

	t.Run("an ID token verifies through discovery", func(t *testing.T) {
		t.Parallel()
		idToken, _ := c.signIn(t)
		verifier, err := auth.NewJWTVerifier(c.issuer, []string{c.app}, []auth.Algorithm{auth.RS256},
			auth.WithHTTPClient(c.trusting), auth.WithRequiredClaims("sub"))
		require.NoError(t, err)
		require.NoError(t, verifier.Prefetch(t.Context()))

		claims, err := verifier.Verify(t.Context(), idToken)
		require.NoError(t, err)
		assert.Equal(t, c.issuer, claims.Issuer())
		assert.NotEmpty(t, claims.Subject())
		username, _ := claims.GetString("cognito:username")
		assert.Equal(t, "ada", username)

		protect, err := auth.Require(verifier)
		require.NoError(t, err)
		handler := protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, _ := auth.ClaimsFromContext(r.Context())
			w.Header().Set("X-Subject", seen.Subject())
		}))
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/orders", http.NoBody)
		r.Header.Set("Authorization", "Bearer "+idToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, claims.Subject(), w.Header().Get("X-Subject"))
	})

	t.Run("the access token profile refuses an ID token", func(t *testing.T) {
		t.Parallel()
		idToken, _ := c.signIn(t)
		verifier, err := auth.NewJWTVerifier(c.issuer, []string{c.app}, []auth.Algorithm{auth.RS256},
			auth.WithHTTPClient(c.trusting), auth.WithExpectedClaims(map[string]string{"token_use": "access"}))
		require.NoError(t, err)
		_, err = verifier.Verify(t.Context(), idToken)
		require.ErrorIs(t, err, auth.ErrInvalidClaims)
	})

	t.Run("access tokens without resource binding are refused", func(t *testing.T) {
		t.Parallel()
		_, accessToken := c.signIn(t)
		verifier, err := auth.NewJWTVerifier(c.issuer, []string{c.resources}, []auth.Algorithm{auth.RS256}, auth.WithHTTPClient(c.trusting))
		require.NoError(t, err)
		// The signature verifies; the missing aud claim does not.
		_, err = verifier.Verify(t.Context(), accessToken)
		require.ErrorIs(t, err, auth.ErrInvalidClaims)
	})

	t.Run("a token of another pool is refused", func(t *testing.T) {
		t.Parallel()
		idToken, _ := c.signIn(t)
		verifier, err := auth.NewJWTVerifier(c.issuer+"x", []string{c.app}, []auth.Algorithm{auth.RS256}, auth.WithHTTPClient(c.trusting))
		require.NoError(t, err)
		_, err = verifier.Verify(t.Context(), idToken)
		require.ErrorIs(t, err, auth.ErrJWKSFetch, "the other pool has no OpenID configuration")
	})

	for _, method := range []oauth2.AuthMethod{oauth2.ClientSecretBasic, oauth2.ClientSecretPost} {
		t.Run("client credentials with "+string(method), func(t *testing.T) {
			t.Parallel()
			secrets := secretsmanager.NewFromConfig(suite.aws)
			name := "oauth2/" + unique(t)
			_, err := secrets.CreateSecret(t.Context(), &secretsmanager.CreateSecretInput{Name: aws.String(name), SecretString: aws.String(c.secret)})
			require.NoError(t, err)
			var lookups atomic.Int32
			source := func(ctx context.Context) (string, error) {
				lookups.Add(1)
				out, lookupErr := secrets.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name)})
				if lookupErr != nil {
					return "", lookupErr
				}
				return aws.ToString(out.SecretString), nil
			}

			client, err := oauth2.New(c.tokenURL, c.service, source,
				oauth2.WithAuthMethod(method), oauth2.WithScopes(c.scope), oauth2.WithHTTPClient(c.trusting))
			require.NoError(t, err)
			first, err := client.Token(t.Context())
			require.NoError(t, err)
			second, err := client.Token(t.Context())
			require.NoError(t, err)
			assert.Equal(t, first, second, "the token is cached")
			assert.Equal(t, int32(1), lookups.Load())

			// Cognito's client-credentials token is an access token of the
			// pool, signed with its keys.
			verifier, err := auth.NewJWTVerifier(c.issuer, []string{c.resources}, []auth.Algorithm{auth.RS256}, auth.WithHTTPClient(c.trusting))
			require.NoError(t, err)
			_, err = verifier.Verify(t.Context(), first)
			require.ErrorIs(t, err, auth.ErrInvalidClaims, "verified, then refused for its missing audience")
		})
	}

	t.Run("a wrong secret is not retried", func(t *testing.T) {
		t.Parallel()
		client, err := oauth2.New(c.tokenURL, c.service, oauth2.StaticSecret("wrong"), oauth2.WithHTTPClient(c.trusting))
		require.NoError(t, err)
		_, err = client.Token(t.Context())
		require.ErrorIs(t, err, oauth2.ErrTokenExchange)
		exchangeErr, ok := errors.AsType[*oauth2.Error](err)
		require.True(t, ok)
		assert.False(t, exchangeErr.Retryable)
	})
}
