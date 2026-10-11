package integration

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // G501: S3 checks SSE-C keys with MD5
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
	"uuid"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/streaming"
)

// s3Bucket creates a bucket of its own for the test, and returns a client
// of the emulator.
func s3Bucket(t *testing.T) (client *s3.Client, bucket string) {
	t.Helper()
	client = s3.NewFromConfig(suite.aws, func(o *s3.Options) { o.UsePathStyle = true })
	bucket = "streaming-" + uuid.New().String()
	_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	return client, bucket
}

func putObject(t *testing.T, client *s3.Client, bucket, key string, data []byte) *s3.PutObjectOutput {
	t.Helper()
	out, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)
	return out
}

// sample is data whose every position is recognizable.
func sample(size int) []byte {
	var buf bytes.Buffer
	for i := 0; buf.Len() < size; i++ {
		fmt.Fprintf(&buf, "%08d\n", i)
	}
	return buf.Bytes()[:size]
}

func TestStreamingReadAndSeek(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	data := sample(3 << 20)
	putObject(t, client, bucket, "big.txt", data)

	obj := streaming.NewS3Object(t.Context(), client, bucket, "big.txt")
	defer func() { require.NoError(t, obj.Close()) }()

	head := make([]byte, 18)
	_, err := io.ReadFull(obj, head)
	require.NoError(t, err)
	assert.Equal(t, "00000000\n00000001\n", string(head))

	size, err := obj.Size()
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)

	pos, err := obj.Seek(-9, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)-9), pos)
	tail, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, data[len(data)-9:], tail)

	_, err = obj.Seek(1<<20, io.SeekStart)
	require.NoError(t, err)
	var copied bytes.Buffer
	n, err := io.Copy(&copied, obj)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)-1<<20), n)
	assert.Equal(t, data[1<<20:], copied.Bytes())

	at := make([]byte, 27)
	_, err = obj.ReadAt(at, 9*1000)
	require.NoError(t, err)
	assert.Equal(t, "00001000\n00001001\n00001002\n", string(at))

	_, err = obj.Seek(int64(len(data))+10, io.SeekStart)
	require.NoError(t, err)
	n2, err := obj.Read(make([]byte, 1))
	assert.Zero(t, n2)
	require.ErrorIs(t, err, io.EOF)
}

func TestStreamingEmptyObject(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	putObject(t, client, bucket, "empty", nil)

	obj := streaming.NewS3Object(t.Context(), client, bucket, "empty")
	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Empty(t, data)
	require.NoError(t, obj.Close())
}

func TestStreamingGzipCSV(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	var csv strings.Builder
	csv.WriteString("id,name,total\n")
	for i := range 5000 {
		fmt.Fprintf(&csv, "%d,customer %d,%d.50\n", i, i, i)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, err := io.WriteString(w, csv.String())
	require.NoError(t, err)
	require.NoError(t, w.Close())
	putObject(t, client, bucket, "orders.csv.gz", gz.Bytes())

	obj := streaming.NewS3Object(t.Context(), client, bucket, "orders.csv.gz")
	r, err := obj.Transform(streaming.Gzip())
	require.NoError(t, err)
	count := 0
	for row, err := range streaming.CSVDicts(r) {
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("customer %d", count), row["name"])
		count++
	}
	assert.Equal(t, 5000, count)
	require.NoError(t, r.Close())
}

func TestStreamingZip(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for i := range 50 {
		member, err := zw.Create(fmt.Sprintf("logs/%02d.log", i))
		require.NoError(t, err)
		_, err = member.Write(sample(10_000 + i))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	putObject(t, client, bucket, "logs.zip", archive.Bytes())

	obj := streaming.NewS3Object(t.Context(), client, bucket, "logs.zip")
	defer func() { require.NoError(t, obj.Close()) }()
	zr, err := obj.Zip()
	require.NoError(t, err)
	require.Len(t, zr.File, 50)

	member, err := zr.Open("logs/42.log")
	require.NoError(t, err)
	content, err := io.ReadAll(member)
	require.NoError(t, err)
	require.NoError(t, member.Close())
	assert.Equal(t, sample(10_042), content)
}

func TestStreamingDetectsAnOverwrite(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	putObject(t, client, bucket, "data.txt", sample(1<<20))

	obj := streaming.NewS3Object(t.Context(), client, bucket, "data.txt")
	defer func() { require.NoError(t, obj.Close()) }()
	_, err := io.ReadFull(obj, make([]byte, 100))
	require.NoError(t, err)

	putObject(t, client, bucket, "data.txt", bytes.Repeat([]byte("changed\n"), 1<<17))

	_, err = obj.Seek(512<<10, io.SeekStart)
	require.NoError(t, err)
	_, err = obj.Read(make([]byte, 100))
	require.ErrorIs(t, err, streaming.ErrObjectChanged)
}

func TestStreamingVersionID(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	_, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	require.NoError(t, err)
	first := putObject(t, client, bucket, "doc.txt", []byte("first version"))
	putObject(t, client, bucket, "doc.txt", []byte("second version"))
	require.NotEmpty(t, aws.ToString(first.VersionId))

	obj := streaming.NewS3Object(t.Context(), client, bucket, "doc.txt", streaming.WithVersionID(aws.ToString(first.VersionId)))
	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, "first version", string(data))
	require.NoError(t, obj.Close())

	latest := streaming.NewS3Object(t.Context(), client, bucket, "doc.txt")
	data, err = io.ReadAll(latest)
	require.NoError(t, err)
	assert.Equal(t, "second version", string(data))
	require.NoError(t, latest.Close())
}

func TestStreamingSSECustomerKey(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	key := bytes.Repeat([]byte{7}, 32)
	sum := md5.Sum(key) //nolint:gosec // G401: S3 checks SSE-C keys with MD5
	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:               aws.String(bucket),
		Key:                  aws.String("secret.txt"),
		Body:                 strings.NewReader("top secret contents"),
		SSECustomerAlgorithm: aws.String("AES256"),
		SSECustomerKey:       aws.String(base64.StdEncoding.EncodeToString(key)),
		SSECustomerKeyMD5:    aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	require.NoError(t, err)

	obj := streaming.NewS3Object(t.Context(), client, bucket, "secret.txt", streaming.WithSSECustomerKey(key))
	defer func() { require.NoError(t, obj.Close()) }()
	_, err = obj.Seek(-8, io.SeekEnd)
	require.NoError(t, err, "the HEAD request carries the key too")
	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, "contents", string(data))

	withoutKey := streaming.NewS3Object(t.Context(), client, bucket, "secret.txt")
	_, err = withoutKey.Read(make([]byte, 1))
	require.Error(t, err)
}

func TestStreamingMissingObject(t *testing.T) {
	t.Parallel()
	client, bucket := s3Bucket(t)
	obj := streaming.NewS3Object(t.Context(), client, bucket, "missing")
	_, err := obj.Read(make([]byte, 1))
	var noSuchKey *types.NoSuchKey
	require.ErrorAs(t, err, &noSuchKey)
	require.NotErrorIs(t, err, streaming.ErrObjectChanged)
}
