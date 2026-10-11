package integration

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	appconfigtypes "github.com/aws/aws-sdk-go-v2/service/appconfig/types"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	pappconfig "github.com/alessandrolattao/powertools-lambda-go/parameters/appconfig"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/dynamodb"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/secrets"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/ssm"
)

// paramsUnique returns a name no other test or run uses.
func paramsUnique(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(rand.IntN(1000))
}

func TestSSMProvider(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := ssm.FromConfig(suite.aws)
	root := "/" + paramsUnique("pt")

	_, err := p.Set(ctx, root+"/db/host", "db.local")
	require.NoError(t, err)
	_, err = p.Set(ctx, root+"/db/hosts", []string{"a", "b"})
	require.NoError(t, err)
	_, err = p.Set(ctx, root+"/db/password", "s3cret", ssm.WithParameterType(ssmtypes.ParameterTypeSecureString))
	require.NoError(t, err)
	_, err = p.Set(ctx, root+"/config.json", `{"retries":3}`)
	require.NoError(t, err)

	_, err = p.Set(ctx, root+"/db/host", "other")
	require.ErrorIs(t, err, parameters.ErrSetParameter, "an existing parameter needs WithOverwrite")
	res, err := p.Set(ctx, root+"/db/host", "db.internal", ssm.WithOverwrite(true))
	require.NoError(t, err)
	assert.Equal(t, int64(2), res.Version)

	host, err := p.Get(ctx, root+"/db/host")
	require.NoError(t, err)
	assert.Equal(t, "db.internal", host)

	hosts, err := p.Get(ctx, root+"/db/hosts")
	require.NoError(t, err)
	assert.Equal(t, "a,b", hosts)

	password, err := p.Get(ctx, root+"/db/password", ssm.WithDecrypt(true))
	require.NoError(t, err)
	assert.Equal(t, "s3cret", password)

	type config struct {
		Retries int `json:"retries"`
	}
	cfg, err := parameters.Get[config](ctx, p, root+"/config.json", parameters.WithTransform(parameters.TransformAuto))
	require.NoError(t, err)
	assert.Equal(t, 3, cfg.Retries)

	direct, err := p.GetMultiple(ctx, root)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"config.json": `{"retries":3}`}, direct)

	all, err := p.GetMultiple(ctx, root, ssm.WithRecursive(true), ssm.WithDecrypt(true))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"config.json": `{"retries":3}`,
		"db/host":     "db.internal",
		"db/hosts":    "a,b",
		"db/password": "s3cret",
	}, all)

	_, err = p.Get(ctx, root+"/missing")
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	require.ErrorAs(t, err, new(*ssmtypes.ParameterNotFound))
}

func TestSSMGetByName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := ssm.FromConfig(suite.aws)
	root := "/" + paramsUnique("pt")
	params := make(map[string][]parameters.Option)
	for i := range 12 {
		name := fmt.Sprintf("%s/p%02d", root, i)
		_, err := p.Set(ctx, name, strconv.Itoa(i))
		require.NoError(t, err)
		params[name] = nil
	}
	_, err := p.Set(ctx, root+"/secret", "s3cret", ssm.WithParameterType(ssmtypes.ParameterTypeSecureString))
	require.NoError(t, err)
	params[root+"/secret"] = []parameters.Option{ssm.WithDecrypt(true)}
	params[root+"/doc"] = []parameters.Option{parameters.WithTransform(parameters.TransformJSON)}
	_, err = p.Set(ctx, root+"/doc", `{"a":1}`)
	require.NoError(t, err)

	values, failed, err := ssm.GetByName[any](ctx, p, params)
	require.NoError(t, err)
	assert.Nil(t, failed)
	assert.Len(t, values, 14, "twelve in two batches, the secret alone, the document decoded")
	assert.Equal(t, "s3cret", values[root+"/secret"])
	assert.Equal(t, map[string]any{"a": float64(1)}, values[root+"/doc"])
	assert.Equal(t, "11", values[root+"/p11"])

	params[root+"/missing"] = nil
	_, _, err = p.GetByName(ctx, params, parameters.WithForceFetch(true))
	require.ErrorIs(t, err, parameters.ErrNotFound)

	texts, failed, err := p.GetByName(ctx, params, ssm.WithRaiseOnError(false), parameters.WithForceFetch(true))
	require.NoError(t, err)
	assert.Len(t, texts, 13)
	require.Len(t, failed, 2)
	require.ErrorIs(t, failed[root+"/missing"], parameters.ErrNotFound)
	require.ErrorIs(t, failed[root+"/doc"], parameters.ErrTransform, "a JSON object is not a string")
}

func TestSecretsProvider(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := secrets.FromConfig(suite.aws)
	name := paramsUnique("pt-db")

	created, err := p.Set(ctx, name, map[string]any{"user": "admin", "password": "first"})
	require.NoError(t, err)
	assert.Equal(t, name, created.Name)
	_, err = p.Set(ctx, name, `{"user":"admin","password":"second"}`)
	require.NoError(t, err, "an existing secret gets a new version")

	type credentials struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	current, err := parameters.Get[credentials](ctx, p, name, parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, credentials{User: "admin", Password: "second"}, current)

	binary := paramsUnique("pt-cert")
	_, err = p.Set(ctx, binary, []byte{0xde, 0xad, 0xbe, 0xef})
	require.NoError(t, err)
	cert, err := parameters.Get[[]byte](ctx, p, binary)
	require.NoError(t, err)
	assert.Equal(t, []byte{0xde, 0xad, 0xbe, 0xef}, cert)

	// A secret whose name starts like another one: Python's name filter
	// would return both.
	longer := name + "-password"
	_, err = p.Set(ctx, longer, "x")
	require.NoError(t, err)
	missing := paramsUnique("pt-missing")

	values, failed, err := p.GetByName(ctx, []string{name, binary, missing})
	require.NoError(t, err)
	assert.Len(t, values, 2)
	assert.NotContains(t, values, longer)
	assert.Equal(t, "\xde\xad\xbe\xef", values[binary])
	require.Contains(t, failed, missing)
	var apiErr smithy.APIError
	require.ErrorAs(t, failed[missing], &apiErr)
	assert.Equal(t, "ResourceNotFoundException", apiErr.ErrorCode())

	_, err = p.Get(ctx, missing)
	require.ErrorIs(t, err, parameters.ErrGetParameter)
}

func TestDynamoDBProvider(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := awsdynamodb.NewFromConfig(suite.aws)
	single := paramsCreateTable(t, client, false)
	multiple := paramsCreateTable(t, client, true)

	put := func(table string, item map[string]dynamodbtypes.AttributeValue) {
		_, err := client.PutItem(ctx, &awsdynamodb.PutItemInput{TableName: aws.String(table), Item: item})
		require.NoError(t, err)
	}
	s := func(v string) dynamodbtypes.AttributeValue { return &dynamodbtypes.AttributeValueMemberS{Value: v} }
	put(single, map[string]dynamodbtypes.AttributeValue{"id": s("my-param"), "value": s("my-value")})
	put(single, map[string]dynamodbtypes.AttributeValue{"id": s("doc"), "value": &dynamodbtypes.AttributeValueMemberM{Value: map[string]dynamodbtypes.AttributeValue{
		"limit": &dynamodbtypes.AttributeValueMemberN{Value: "10"},
	}}})
	for _, sk := range []string{"a", "b", "c.json"} {
		value := "value-" + sk
		if strings.HasSuffix(sk, ".json") {
			value = `{"c":true}`
		}
		put(multiple, map[string]dynamodbtypes.AttributeValue{"id": s("my-hash-key"), "sk": s(sk), "value": s(value)})
	}

	p := dynamodb.FromConfig(suite.aws, single)
	v, err := p.Get(ctx, "my-param", dynamodb.WithConsistentRead(true))
	require.NoError(t, err)
	assert.Equal(t, "my-value", v)
	doc, err := parameters.Get[map[string]int](ctx, p, "doc", parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"limit": 10}, doc)
	_, err = p.Get(ctx, "missing")
	require.ErrorIs(t, err, parameters.ErrNotFound)

	values, err := parameters.GetMultiple[any](ctx, dynamodb.FromConfig(suite.aws, multiple), "my-hash-key", parameters.WithTransform(parameters.TransformAuto))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": "value-a", "b": "value-b", "c.json": map[string]any{"c": true}}, values)
}

// paramsCreateTable creates a table with Python's schema: partition key id,
// and sort key sk when it holds values by path.
func paramsCreateTable(t *testing.T, client *awsdynamodb.Client, withSort bool) string {
	t.Helper()
	name := paramsUnique("pt-params")
	input := &awsdynamodb.CreateTableInput{
		TableName:            aws.String(name),
		BillingMode:          dynamodbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: dynamodbtypes.ScalarAttributeTypeS}},
		KeySchema:            []dynamodbtypes.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: dynamodbtypes.KeyTypeHash}},
	}
	if withSort {
		input.AttributeDefinitions = append(input.AttributeDefinitions, dynamodbtypes.AttributeDefinition{AttributeName: aws.String("sk"), AttributeType: dynamodbtypes.ScalarAttributeTypeS})
		input.KeySchema = append(input.KeySchema, dynamodbtypes.KeySchemaElement{AttributeName: aws.String("sk"), KeyType: dynamodbtypes.KeyTypeRange})
	}
	_, err := client.CreateTable(t.Context(), input)
	require.NoError(t, err)
	waiter := awsdynamodb.NewTableExistsWaiter(client)
	require.NoError(t, waiter.Wait(t.Context(), &awsdynamodb.DescribeTableInput{TableName: aws.String(name)}, time.Minute))
	return name
}

// paramsDeployAppConfig creates an AppConfig application, environment and
// freeform hosted profile holding content, deploys it, and returns their
// ids. AWS accepts names or ids; Floci 2.1.0 resolves ids only, its
// StartConfigurationSession answers "ResourceNotFoundException: Environment
// not found" to names.
func paramsDeployAppConfig(t *testing.T, content string) (application, environment, profile string) {
	t.Helper()
	ctx := t.Context()
	client := appconfig.NewFromConfig(suite.aws)
	app, err := client.CreateApplication(ctx, &appconfig.CreateApplicationInput{Name: aws.String(paramsUnique("pt-app"))})
	require.NoError(t, err, "CreateApplication")
	env, err := client.CreateEnvironment(ctx, &appconfig.CreateEnvironmentInput{ApplicationId: app.Id, Name: aws.String("prod")})
	require.NoError(t, err, "CreateEnvironment")
	prof, err := client.CreateConfigurationProfile(ctx, &appconfig.CreateConfigurationProfileInput{
		ApplicationId: app.Id,
		Name:          aws.String("features"),
		LocationUri:   aws.String("hosted"),
		Type:          aws.String("AWS.Freeform"),
	})
	require.NoError(t, err, "CreateConfigurationProfile")
	version, err := client.CreateHostedConfigurationVersion(ctx, &appconfig.CreateHostedConfigurationVersionInput{
		ApplicationId:          app.Id,
		ConfigurationProfileId: prof.Id,
		Content:                []byte(content),
		ContentType:            aws.String("application/json"),
	})
	require.NoError(t, err, "CreateHostedConfigurationVersion")
	strategy, err := client.CreateDeploymentStrategy(ctx, &appconfig.CreateDeploymentStrategyInput{
		Name:                        aws.String(paramsUnique("pt-now")),
		DeploymentDurationInMinutes: aws.Int32(0),
		FinalBakeTimeInMinutes:      0,
		GrowthFactor:                aws.Float32(100),
		ReplicateTo:                 appconfigtypes.ReplicateToNone,
	})
	require.NoError(t, err, "CreateDeploymentStrategy")
	_, err = client.StartDeployment(ctx, &appconfig.StartDeploymentInput{
		ApplicationId:          app.Id,
		EnvironmentId:          env.Id,
		ConfigurationProfileId: prof.Id,
		ConfigurationVersion:   aws.String(strconv.Itoa(int(version.VersionNumber))),
		DeploymentStrategyId:   strategy.Id,
	})
	require.NoError(t, err, "StartDeployment")
	return aws.ToString(app.Id), aws.ToString(env.Id), aws.ToString(prof.Id)
}

func TestAppConfigProvider(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	application, environment, profile := paramsDeployAppConfig(t, `{"log_level":"DEBUG","retries":3}`)
	p := pappconfig.FromConfig(suite.aws, application, environment)

	type config struct {
		LogLevel string `json:"log_level"`
		Retries  int    `json:"retries"`
	}
	cfg, err := parameters.Get[config](ctx, p, profile, parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, config{LogLevel: "DEBUG", Retries: 3}, cfg)

	// Read again past the cache: within the poll interval AppConfig asked
	// for, the last configuration is served without polling.
	again, err := p.Get(ctx, profile, parameters.WithForceFetch(true))
	require.NoError(t, err)
	assert.JSONEq(t, `{"log_level":"DEBUG","retries":3}`, again)

	_, err = p.Get(ctx, "no-such-profile")
	require.ErrorIs(t, err, parameters.ErrGetParameter)
}

// TestShortcuts uses the default providers, built from the environment as
// in a function: it points the SDK at the emulator through the standard
// variables, so it cannot run in parallel.
func TestShortcuts(t *testing.T) {
	creds, err := suite.aws.Credentials.Retrieve(t.Context())
	require.NoError(t, err)
	t.Setenv("AWS_ENDPOINT_URL", aws.ToString(suite.aws.BaseEndpoint))
	t.Setenv("AWS_REGION", suite.aws.Region)
	t.Setenv("AWS_ACCESS_KEY_ID", creds.AccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", creds.SecretAccessKey)
	t.Setenv("AWS_SESSION_TOKEN", "")
	parameters.ClearCaches()
	t.Cleanup(parameters.ClearCaches)
	ctx := t.Context()

	root := "/" + paramsUnique("pt")
	_, err = ssm.SetParameter(ctx, root+"/a", "1")
	require.NoError(t, err)
	_, err = ssm.SetParameter(ctx, root+"/deep/b", "2")
	require.NoError(t, err)
	a, err := ssm.GetParameter[string](ctx, root+"/a")
	require.NoError(t, err)
	assert.Equal(t, "1", a)
	all, err := ssm.GetParameters[string](ctx, root)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "deep/b": "2"}, all, "the shortcut is recursive, as Python's get_parameters")
	byName, _, err := ssm.GetParametersByName[string](ctx, map[string][]parameters.Option{root + "/a": nil, root + "/deep/b": nil})
	require.NoError(t, err)
	assert.Len(t, byName, 2)

	secret := paramsUnique("pt-secret")
	_, err = secrets.SetSecret(ctx, secret, "s3cret")
	require.NoError(t, err)
	value, err := secrets.GetSecret[string](ctx, secret)
	require.NoError(t, err)
	assert.Equal(t, "s3cret", value)
	values, _, err := secrets.GetSecretsByName[string](ctx, []string{secret})
	require.NoError(t, err)
	assert.Equal(t, "s3cret", values[secret])

	appA, envA, profA := paramsDeployAppConfig(t, `{"app":"a"}`)
	appB, envB, profB := paramsDeployAppConfig(t, `{"app":"b"}`)
	gotA, err := pappconfig.GetAppConfig[map[string]string](ctx, profA, envA, appA, parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	gotB, err := pappconfig.GetAppConfig[map[string]string](ctx, profB, envB, appB, parameters.WithTransform(parameters.TransformJSON))
	require.NoError(t, err)
	assert.Equal(t, "a", gotA["app"])
	assert.Equal(t, "b", gotB["app"], "Python would read application a again")
}

func TestUnreachableStoreIsAGetParameterError(t *testing.T) {
	t.Parallel()
	cfg := suite.aws.Copy()
	cfg.BaseEndpoint = aws.String("http://127.0.0.1:1")
	cfg.RetryMaxAttempts = 1
	_, err := ssm.FromConfig(cfg).Get(t.Context(), "/anything")
	require.ErrorIs(t, err, parameters.ErrGetParameter)
	require.NotErrorIs(t, err, parameters.ErrNotFound)
	_, err = secretsmanager.NewFromConfig(cfg).GetSecretValue(t.Context(), &secretsmanager.GetSecretValueInput{SecretId: aws.String("x")})
	require.Error(t, err)
}
