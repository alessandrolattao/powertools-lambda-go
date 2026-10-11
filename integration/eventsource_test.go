package integration

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
	"github.com/alessandrolattao/powertools-lambda-go/eventsource/codepipeline"
)

func pathStyle(o *s3.Options) { o.UsePathStyle = true }

// TestCodePipelineArtifacts reads and writes the artifacts of a CodePipeline
// job in a real S3 implementation, with the job's credentials.
func TestCodePipelineArtifacts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := s3.NewFromConfig(suite.aws, pathStyle)
	bucket := fmt.Sprintf("codepipeline-artifacts-%d", time.Now().UnixNano())
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create("template.yaml")
	require.NoError(t, err)
	_, err = f.Write([]byte("Resources: {}\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("pipeline/Source/abc.zip"), Body: bytes.NewReader(archive.Bytes()),
	})
	require.NoError(t, err)

	creds, err := suite.aws.Credentials.Retrieve(ctx)
	require.NoError(t, err)
	location := func(key string) events.CodePipelineInputLocation {
		return events.CodePipelineInputLocation{
			LocationType: "S3",
			S3Location:   events.CodePipelineS3Location{BucketName: bucket, ObjectKey: key},
		}
	}
	var job eventsource.CodePipelineJobEvent
	job.CodePipelineJob.ID = "job-1"
	job.CodePipelineJob.Data.InputArtifacts = []events.CodePipelineInputArtifact{
		{Name: "SourceArtifact", Location: location("pipeline/Source/abc.zip")},
	}
	job.CodePipelineJob.Data.OutPutArtifacts = []events.CodePipelineOutputArtifact{
		{Name: "BuildArtifact", Location: location("pipeline/Build/def.zip")},
	}
	job.CodePipelineJob.Data.ArtifactCredentials.AccessKeyID = creds.AccessKeyID
	job.CodePipelineJob.Data.ArtifactCredentials.SecretAccessKey = creds.SecretAccessKey
	job.CodePipelineJob.Data.ArtifactCredentials.SessionToken = creds.SessionToken

	artifacts := codepipeline.FromConfig(suite.aws, job, pathStyle)

	whole, err := artifacts.Get(ctx, "SourceArtifact")
	require.NoError(t, err)
	assert.Equal(t, archive.Bytes(), whole)

	template, err := artifacts.GetFile(ctx, "SourceArtifact", "template.yaml")
	require.NoError(t, err)
	assert.Equal(t, "Resources: {}\n", string(template))

	require.NoError(t, artifacts.Put(ctx, "BuildArtifact", strings.NewReader("built"), "text/plain"))
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("pipeline/Build/def.zip")})
	require.NoError(t, err)
	written, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	require.NoError(t, out.Body.Close())
	assert.Equal(t, "built", string(written))
	assert.Equal(t, "text/plain", aws.ToString(out.ContentType))

	_, err = artifacts.Get(ctx, "Missing")
	require.ErrorIs(t, err, codepipeline.ErrArtifactNotFound)
}
