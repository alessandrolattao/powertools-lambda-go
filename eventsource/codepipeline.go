package eventsource

import (
	"encoding/json/v2"
	"fmt"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

// CodePipelineJobEvent is events.CodePipelineJobEvent (a CodePipeline Lambda
// invoke action) whose job data has the artifact encryption key and the
// expiration of the artifact credentials. The subpackage
// eventsource/codepipeline reads and writes the job's artifacts.
type CodePipelineJobEvent struct {
	CodePipelineJob CodePipelineJob `json:"CodePipeline.job"`
}

// CodePipelineJob is events.CodePipelineJob with the complete job data.
type CodePipelineJob struct {
	events.CodePipelineJob

	Data CodePipelineData `json:"data"`
}

// CodePipelineData is events.CodePipelineData with the encryption key of the
// artifact store and the complete artifact credentials.
type CodePipelineData struct {
	events.CodePipelineData

	ArtifactCredentials CodePipelineArtifactCredentials `json:"artifactCredentials"`
	// EncryptionKey is nil when the artifact store uses the default key.
	EncryptionKey *CodePipelineEncryptionKey `json:"encryptionKey,omitempty"`
}

// CodePipelineEncryptionKey is the key the artifact store encrypts with.
type CodePipelineEncryptionKey struct {
	ID string `json:"id"`
	// Type is KMS.
	Type string `json:"type"`
}

// CodePipelineArtifactCredentials is events.CodePipelineArtifactCredentials
// with their expiration. The secret members never reach logs: the type
// formats and logs them as [SENSITIVE], as Powertools Python prints them.
type CodePipelineArtifactCredentials struct {
	events.CodePipelineArtifactCredentials

	// ExpirationTime is in milliseconds since the Unix epoch.
	ExpirationTime int64 `json:"expirationTime,omitempty"`
}

// sensitive replaces secrets in logs and formatted output.
const sensitive = "[SENSITIVE]"

// String hides the secret access key and the session token.
func (c CodePipelineArtifactCredentials) String() string {
	return fmt.Sprintf("{AccessKeyID:%s SecretAccessKey:%s SessionToken:%s ExpirationTime:%d}",
		c.AccessKeyID, sensitive, sensitive, c.ExpirationTime)
}

// GoString hides the secrets from %#v too.
func (c CodePipelineArtifactCredentials) GoString() string {
	return "eventsource.CodePipelineArtifactCredentials" + c.String()
}

// LogValue hides the secrets from log/slog.
func (c CodePipelineArtifactCredentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("accessKeyId", c.AccessKeyID),
		slog.String("secretAccessKey", sensitive),
		slog.String("sessionToken", sensitive),
		slog.Int64("expirationTime", c.ExpirationTime),
	)
}

// UserParameters returns the user parameters of the action configuration.
func (e CodePipelineJobEvent) UserParameters() string {
	return e.CodePipelineJob.Data.ActionConfiguration.Configuration.UserParameters
}

// DecodeUserParameters decodes the user parameters, JSON, into a T. Missing
// parameters give the zero T, as Powertools Python gives an empty dict.
func (e CodePipelineJobEvent) DecodeUserParameters[T any]() (T, error) {
	var out T
	params := e.UserParameters()
	if params == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(params), &out); err != nil {
		return out, fmt.Errorf("decode the user parameters into %T: %w", out, err)
	}
	return out, nil
}

// InputBucketName returns the bucket of the first input artifact, or "".
func (e CodePipelineJobEvent) InputBucketName() string {
	inputs := e.CodePipelineJob.Data.InputArtifacts
	if len(inputs) == 0 {
		return ""
	}
	return inputs[0].Location.S3Location.BucketName
}

// InputObjectKey returns the URL-decoded object key of the first input
// artifact, or "".
func (e CodePipelineJobEvent) InputObjectKey() string {
	inputs := e.CodePipelineJob.Data.InputArtifacts
	if len(inputs) == 0 {
		return ""
	}
	return UnquoteS3Key(inputs[0].Location.S3Location.ObjectKey)
}

// FindInputArtifact returns the input artifact called name.
func (e CodePipelineJobEvent) FindInputArtifact(name string) (events.CodePipelineInputArtifact, bool) {
	for _, artifact := range e.CodePipelineJob.Data.InputArtifacts {
		if artifact.Name == name {
			return artifact, true
		}
	}
	return events.CodePipelineInputArtifact{}, false
}

// FindOutputArtifact returns the output artifact called name.
func (e CodePipelineJobEvent) FindOutputArtifact(name string) (events.CodePipelineOutputArtifact, bool) {
	for _, artifact := range e.CodePipelineJob.Data.OutPutArtifacts {
		if artifact.Name == name {
			return artifact, true
		}
	}
	return events.CodePipelineOutputArtifact{}, false
}
