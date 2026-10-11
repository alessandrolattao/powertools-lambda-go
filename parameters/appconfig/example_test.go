package appconfig_test

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"

	"github.com/alessandrolattao/powertools-lambda-go/parameters"
	"github.com/alessandrolattao/powertools-lambda-go/parameters/appconfig"
)

// oneConfiguration is an AppConfigData client that always serves the same
// configuration.
type oneConfiguration string

func (oneConfiguration) StartConfigurationSession(context.Context, *appconfigdata.StartConfigurationSessionInput, ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error) {
	return &appconfigdata.StartConfigurationSessionOutput{InitialConfigurationToken: aws.String("token")}, nil
}

func (c oneConfiguration) GetLatestConfiguration(context.Context, *appconfigdata.GetLatestConfigurationInput, ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error) {
	return &appconfigdata.GetLatestConfigurationOutput{Configuration: []byte(c), NextPollConfigurationToken: aws.String("token")}, nil
}

func ExampleProvider() {
	type config struct {
		LogLevel string `json:"log_level"`
	}
	// In a function: appconfig.FromConfig(cfg, "my-app", "prod"), created
	// once in main.
	provider := appconfig.New(oneConfiguration(`{"log_level":"DEBUG"}`), "my-app", "prod")

	cfg, err := parameters.Get[config](context.Background(), provider, "my-profile", parameters.WithTransform(parameters.TransformJSON))
	if err != nil {
		panic(err)
	}
	fmt.Println(cfg.LogLevel)
	// Output: DEBUG
}
