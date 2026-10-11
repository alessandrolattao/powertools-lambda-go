// Package codepipeline reads and writes the artifacts of a CodePipeline job,
// the S3 half of Powertools Python's CodePipelineJobEvent (setup_s3_client,
// get_artifact, put_artifact).
//
// A CodePipeline Lambda invoke action receives temporary credentials scoped
// to the pipeline's artifact store. FromConfig builds an S3 client with
// them; New takes any client:
//
//	func handle(ctx context.Context, event eventsource.CodePipelineJobEvent) error {
//		artifacts := codepipeline.FromConfig(cfg, event)
//		template, err := artifacts.GetFile(ctx, "SourceArtifact", "template.yaml")
//		...
//		return artifacts.Put(ctx, "BuildArtifact", bytes.NewReader(out), "application/zip")
//	}
//
// It lives in its own package so that importing eventsource does not bring
// in the S3 client.
package codepipeline

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// Errors of the artifact helpers.
var (
	// ErrArtifactNotFound reports a name the job has no artifact for.
	ErrArtifactNotFound = errors.New("artifact not found")
	// ErrFileNotFound reports a file the artifact archive does not hold.
	ErrFileNotFound = errors.New("file not found in the artifact")
)

// S3API is the part of the S3 client the helpers use; *s3.Client satisfies
// it.
type S3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Artifacts reads the input artifacts and writes the output artifacts of a
// job. It is safe for concurrent use when the client is.
type Artifacts struct {
	client S3API
	job    eventsource.CodePipelineJobEvent
}

// New returns the artifacts of job, accessed with client.
func New(client S3API, job eventsource.CodePipelineJobEvent) *Artifacts {
	return &Artifacts{client: client, job: job}
}

// NewS3Client returns an S3 client authenticated with the job's temporary
// artifact credentials. cfg gives the region and everything else; optFns
// customize the client further.
func NewS3Client(cfg aws.Config, job eventsource.CodePipelineJobEvent, optFns ...func(*s3.Options)) *s3.Client {
	creds := job.CodePipelineJob.Data.ArtifactCredentials
	provider := credentials.NewStaticCredentialsProvider(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	opts := append([]func(*s3.Options){func(o *s3.Options) { o.Credentials = provider }}, optFns...)
	return s3.NewFromConfig(cfg, opts...)
}

// FromConfig returns the artifacts of job, accessed with a client built by
// NewS3Client.
func FromConfig(cfg aws.Config, job eventsource.CodePipelineJobEvent, optFns ...func(*s3.Options)) *Artifacts {
	return New(NewS3Client(cfg, job, optFns...), job)
}

// Get returns the content of the input artifact called name.
func (a *Artifacts) Get(ctx context.Context, name string) ([]byte, error) {
	body, err := a.open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }() //nolint:errcheck // the body is fully read; a close error changes nothing
	content, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read artifact %q: %w", name, err)
	}
	return content, nil
}

// GetFile returns one file of the input artifact called name, which is a
// zip archive, as CodePipeline stores source and build outputs. The archive
// is spooled to a temporary file, as Powertools Python does, so that a large
// artifact does not have to fit in memory.
func (a *Artifacts) GetFile(ctx context.Context, name, filename string) ([]byte, error) {
	body, err := a.open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }() //nolint:errcheck // the body is fully read; a close error changes nothing

	spool, err := os.CreateTemp("", "codepipeline-artifact-*.zip")
	if err != nil {
		return nil, fmt.Errorf("spool artifact %q: %w", name, err)
	}
	defer func() {
		_ = spool.Close()           //nolint:errcheck // the file is removed right after
		_ = os.Remove(spool.Name()) //nolint:errcheck // a leftover temporary file is harmless
	}()
	size, err := io.Copy(spool, body)
	if err != nil {
		return nil, fmt.Errorf("spool artifact %q: %w", name, err)
	}
	archive, err := zip.NewReader(spool, size)
	if err != nil {
		return nil, fmt.Errorf("open artifact %q as a zip archive: %w", name, err)
	}
	file, err := archive.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("%w: %q in %q: %w", ErrFileNotFound, filename, name, err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // a read-only archive member has nothing to report on close
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %q in artifact %q: %w", filename, name, err)
	}
	return content, nil
}

func (a *Artifacts) open(ctx context.Context, name string) (io.ReadCloser, error) {
	artifact, ok := a.job.FindInputArtifact(name)
	if !ok {
		return nil, fmt.Errorf("%w: input %q", ErrArtifactNotFound, name)
	}
	location := artifact.Location.S3Location
	out, err := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(location.BucketName),
		Key:    aws.String(location.ObjectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("get artifact %q: %w", name, err)
	}
	return out.Body, nil
}

// Put writes body as the output artifact called name. When the pipeline
// encrypts its artifact store with a KMS key, the object is encrypted with
// it; an S3 Bucket Key is always enabled, as Powertools Python does.
func (a *Artifacts) Put(ctx context.Context, name string, body io.Reader, contentType string) error {
	artifact, ok := a.job.FindOutputArtifact(name)
	if !ok {
		return fmt.Errorf("%w: output %q", ErrArtifactNotFound, name)
	}
	location := artifact.Location.S3Location
	input := &s3.PutObjectInput{
		Bucket:           aws.String(location.BucketName),
		Key:              aws.String(location.ObjectKey),
		Body:             body,
		ContentType:      aws.String(contentType),
		BucketKeyEnabled: aws.Bool(true),
	}
	if key := a.job.CodePipelineJob.Data.EncryptionKey; key != nil {
		input.ServerSideEncryption = types.ServerSideEncryption(key.Type)
		if key.Type == "KMS" {
			input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		}
		input.SSEKMSKeyId = aws.String(key.ID)
	}
	if _, err := a.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("put artifact %q: %w", name, err)
	}
	return nil
}
