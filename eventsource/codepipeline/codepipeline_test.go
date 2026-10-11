package codepipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
)

// fakeS3 keeps objects in memory and records the last put.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	lastPut *s3.PutObjectInput
	err     error
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)] = body
	f.lastPut = in
	return &s3.PutObjectOutput{}, nil
}

func job(encryptionKey *eventsource.CodePipelineEncryptionKey) eventsource.CodePipelineJobEvent {
	location := func(key string) events.CodePipelineInputLocation {
		return events.CodePipelineInputLocation{
			LocationType: "S3",
			S3Location:   events.CodePipelineS3Location{BucketName: "artifacts", ObjectKey: key},
		}
	}
	var event eventsource.CodePipelineJobEvent
	event.CodePipelineJob.ID = "job-1"
	event.CodePipelineJob.Data.InputArtifacts = []events.CodePipelineInputArtifact{
		{Name: "Source", Location: location("source.zip")},
		{Name: "Plain", Location: location("plain.txt")},
	}
	event.CodePipelineJob.Data.OutPutArtifacts = []events.CodePipelineOutputArtifact{
		{Name: "Output", Location: location("output.zip")},
	}
	event.CodePipelineJob.Data.EncryptionKey = encryptionKey
	return event
}

func zipped(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestGet(t *testing.T) {
	t.Parallel()
	client := &fakeS3{objects: map[string][]byte{
		"artifacts/plain.txt":  []byte("hello"),
		"artifacts/source.zip": zipped(t, map[string]string{"template.yaml": "Resources: {}"}),
	}}
	artifacts := New(client, job(nil))

	got, err := artifacts.Get(t.Context(), "Plain")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))

	file, err := artifacts.GetFile(t.Context(), "Source", "template.yaml")
	require.NoError(t, err)
	assert.Equal(t, "Resources: {}", string(file))

	_, err = artifacts.GetFile(t.Context(), "Source", "missing.yaml")
	require.ErrorIs(t, err, ErrFileNotFound)
	_, err = artifacts.GetFile(t.Context(), "Plain", "x")
	require.Error(t, err, "a plain object is not a zip archive")
	_, err = artifacts.Get(t.Context(), "Missing")
	require.ErrorIs(t, err, ErrArtifactNotFound)
	_, err = artifacts.GetFile(t.Context(), "Missing", "x")
	require.ErrorIs(t, err, ErrArtifactNotFound)
}

func TestPut(t *testing.T) {
	t.Parallel()

	client := &fakeS3{objects: map[string][]byte{}}
	require.NoError(t, New(client, job(nil)).Put(t.Context(), "Output", strings.NewReader("built"), "application/zip"))
	assert.Equal(t, "built", string(client.objects["artifacts/output.zip"]))
	assert.Equal(t, "application/zip", aws.ToString(client.lastPut.ContentType))
	assert.True(t, aws.ToBool(client.lastPut.BucketKeyEnabled))
	assert.Empty(t, client.lastPut.ServerSideEncryption)
	assert.Nil(t, client.lastPut.SSEKMSKeyId)

	kms := &fakeS3{objects: map[string][]byte{}}
	key := &eventsource.CodePipelineEncryptionKey{ID: "arn:aws:kms:us-east-1:1:key/k", Type: "KMS"}
	require.NoError(t, New(kms, job(key)).Put(t.Context(), "Output", strings.NewReader("built"), "application/zip"))
	assert.Equal(t, types.ServerSideEncryptionAwsKms, kms.lastPut.ServerSideEncryption)
	assert.Equal(t, key.ID, aws.ToString(kms.lastPut.SSEKMSKeyId))

	err := New(kms, job(nil)).Put(t.Context(), "Missing", strings.NewReader(""), "text/plain")
	require.ErrorIs(t, err, ErrArtifactNotFound)
}

func TestClientErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	artifacts := New(&fakeS3{objects: map[string][]byte{}, err: boom}, job(nil))
	_, err := artifacts.Get(t.Context(), "Plain")
	require.ErrorIs(t, err, boom)
	_, err = artifacts.GetFile(t.Context(), "Source", "x")
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, artifacts.Put(t.Context(), "Output", strings.NewReader(""), "text/plain"), boom)
}

func TestNewS3Client(t *testing.T) {
	t.Parallel()
	event := job(nil)
	event.CodePipelineJob.Data.ArtifactCredentials.AccessKeyID = "AKID"
	event.CodePipelineJob.Data.ArtifactCredentials.SecretAccessKey = "SECRET"
	event.CodePipelineJob.Data.ArtifactCredentials.SessionToken = "TOKEN"

	client := NewS3Client(aws.Config{Region: "us-east-1"}, event, func(o *s3.Options) { o.UsePathStyle = true })
	creds, err := client.Options().Credentials.Retrieve(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "AKID", creds.AccessKeyID)
	assert.Equal(t, "SECRET", creds.SecretAccessKey)
	assert.Equal(t, "TOKEN", creds.SessionToken)
	assert.True(t, client.Options().UsePathStyle)
	assert.NotNil(t, FromConfig(aws.Config{Region: "us-east-1"}, event))
}
