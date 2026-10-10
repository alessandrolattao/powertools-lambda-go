package powertools_test

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"

	"github.com/alessandrolattao/powertools-lambda-go"
)

func TestServiceName(t *testing.T) {
	t.Setenv("POWERTOOLS_SERVICE_NAME", "")
	assert.Equal(t, "service_undefined", powertools.ServiceName())
	t.Setenv("POWERTOOLS_SERVICE_NAME", "payment")
	assert.Equal(t, "payment", powertools.ServiceName())
}

func TestColdStart(t *testing.T) {
	t.Setenv("AWS_LAMBDA_INITIALIZATION_TYPE", "on-demand")
	first := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{AwsRequestID: "root-1"})
	second := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{AwsRequestID: "root-2"})
	assert.True(t, powertools.ColdStart(first))
	assert.False(t, powertools.ColdStart(second))
}
