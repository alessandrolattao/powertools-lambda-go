package streaming_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/streaming"
)

// cutS3 serves an object of size bytes whose ranged bodies break after
// keep bytes, as a connection cut mid-body does.
type cutS3 struct {
	size, keep int64
}

func (c cutS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	var start int64
	if in.Range != nil {
		_, _ = fmt.Sscanf(aws.ToString(in.Range), "bytes=%d-", &start) //nolint:errcheck // the test always sends this shape
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("x", int(c.keep))), errReader{})),
		ContentRange:  aws.String(fmt.Sprintf("bytes %d-%d/%d", start, c.size-1, c.size)),
		ContentLength: aws.Int64(c.size - start),
		ETag:          aws.String(`"etag"`),
	}, nil
}

func (c cutS3) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(c.size), ETag: aws.String(`"etag"`)}, nil
}

// errReader is what net/http hands back when a body shorter than its
// Content-Length ends.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadAtReportsATruncatedBody(t *testing.T) {
	t.Parallel()
	obj := streaming.NewS3Object(t.Context(), cutS3{size: 100, keep: 10}, "bucket", "key")
	t.Cleanup(func() { assert.NoError(t, obj.Close()) })

	buf := make([]byte, 50)
	n, err := obj.ReadAt(buf, 0)
	assert.Equal(t, 10, n)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF, "a cut connection is not the end of the object")
	require.NotErrorIs(t, err, io.EOF)
}
