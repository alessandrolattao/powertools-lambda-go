package eventsource

import (
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// UnquoteS3Key decodes an object key as S3 event notifications encode it,
// the way Python's urllib.parse.unquote_plus does: "+" is a space and %XX
// escapes are decoded, while a malformed escape is kept as it is instead of
// failing (url.QueryUnescape fails).
func UnquoteS3Key(key string) string {
	if !strings.ContainsAny(key, "+%") {
		return strings.ToValidUTF8(key, "\uFFFD")
	}
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(key) && isHex(key[i+1]) && isHex(key[i+2]):
			b.WriteByte(unhex(key[i+1])<<4 | unhex(key[i+2]))
			i += 2
		default:
			b.WriteByte(c)
		}
	}
	// Python decodes the bytes as UTF-8, replacing what is not.
	return strings.ToValidUTF8(b.String(), "\uFFFD")
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// S3BucketFromARN returns the bucket name of an S3 bucket ARN
// (arn:aws:s3:::bucket), or the argument itself when it is not an ARN.
func S3BucketFromARN(arn string) string {
	if _, bucket, ok := strings.Cut(arn, ":::"); ok {
		return bucket
	}
	return arn
}

// S3BatchTask is a task of an S3 Batch Operations event, schema 1.0 or 2.0.
type S3BatchTask interface {
	events.S3BatchJobTask | events.S3BatchJobTaskV2
}

// S3BatchTaskBucket returns the bucket of a task: S3Bucket in schema 2.0,
// the name in S3BucketARN in schema 1.0.
func S3BatchTaskBucket[T S3BatchTask](task T) string {
	switch t := any(task).(type) {
	case events.S3BatchJobTask:
		return S3BucketFromARN(t.S3BucketARN)
	case events.S3BatchJobTaskV2:
		return t.S3Bucket
	default:
		return ""
	}
}

// S3BatchTaskKey returns the object key of a task, URL-decoded as
// UnquoteS3Key does.
func S3BatchTaskKey[T S3BatchTask](task T) string {
	switch t := any(task).(type) {
	case events.S3BatchJobTask:
		return UnquoteS3Key(t.S3Key)
	case events.S3BatchJobTaskV2:
		return UnquoteS3Key(t.S3Key)
	default:
		return ""
	}
}

// The result codes of an S3 Batch Operations task.
const (
	S3BatchSucceeded        = "Succeeded"
	S3BatchTemporaryFailure = "TemporaryFailure"
	S3BatchPermanentFailure = "PermanentFailure"
)

// NewS3BatchJobResponse returns the response to an S3 Batch Operations
// invocation, with treatMissingKeysAs Succeeded as Powertools Python
// defaults it. S3 Batch Operations sends one task per invocation, so results
// should hold exactly one result; Validate checks it.
func NewS3BatchJobResponse(invocationSchemaVersion, invocationID string, results ...events.S3BatchJobResult) events.S3BatchJobResponse {
	return events.S3BatchJobResponse{
		InvocationSchemaVersion: invocationSchemaVersion,
		TreatMissingKeysAs:      S3BatchSucceeded,
		InvocationID:            invocationID,
		Results:                 results,
	}
}

// ValidateS3BatchJobResponse reports what S3 Batch Operations would reject:
// a number of results other than one, or an unknown result code.
func ValidateS3BatchJobResponse(r events.S3BatchJobResponse) error {
	if len(r.Results) == 0 {
		return ErrEmptyResponse
	}
	if len(r.Results) > 1 {
		return fmt.Errorf("%w: S3 Batch Operations expects exactly one result, got %d", ErrInvalidResult, len(r.Results))
	}
	for _, code := range []string{r.TreatMissingKeysAs, r.Results[0].ResultCode} {
		switch code {
		case S3BatchSucceeded, S3BatchTemporaryFailure, S3BatchPermanentFailure:
		default:
			return fmt.Errorf("%w: result code %q", ErrInvalidResult, code)
		}
	}
	return nil
}

// S3ObjectLambdaEvent is events.S3ObjectLambdaEvent whose user identity has
// the IAM user name.
type S3ObjectLambdaEvent struct {
	events.S3ObjectLambdaEvent

	UserIdentity S3ObjectLambdaUserIdentity `json:"userIdentity"`
}

// S3ObjectLambdaUserIdentity is events.S3ObjectLambdaUserIdentity with the
// IAM user name.
type S3ObjectLambdaUserIdentity struct {
	events.S3ObjectLambdaUserIdentity

	UserName string `json:"userName,omitempty"`
}
