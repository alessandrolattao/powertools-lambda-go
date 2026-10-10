package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/require"
)

func TestEmulatorAnswers(t *testing.T) {
	t.Parallel()
	out, err := sts.NewFromConfig(suite.aws).GetCallerIdentity(t.Context(), &sts.GetCallerIdentityInput{})
	require.NoError(t, err)
	require.NotEmpty(t, out.Account)
	require.NotEmpty(t, suite.valkeyURL)
}
