package streaming_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: S3 checks SSE-C keys with MD5
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/streaming"
)

const alphabet = "abcdefghijklmnopqrstuvwxyz"

func newObject(t *testing.T, data string, opts ...streaming.Option) (*streaming.S3Object, *memS3) {
	t.Helper()
	fake := newMemS3()
	fake.put("key", []byte(data))
	obj := streaming.NewS3Object(t.Context(), fake, "bucket", "key", opts...)
	t.Cleanup(func() { assert.NoError(t, obj.Close()) })
	return obj, fake
}

func TestReadIsLazyAndSequential(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)
	assert.Empty(t, fake.ranges(), "nothing is requested before the first read")
	assert.Equal(t, "bucket", obj.Bucket())
	assert.Equal(t, "key", obj.Key())
	assert.Empty(t, obj.VersionID())

	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, alphabet, string(data))
	assert.Equal(t, []string{"bytes=0-"}, fake.ranges(), "one request for a sequential read")
	assert.Empty(t, fake.heads)
	assert.Zero(t, fake.openBodies())

	n, err := obj.Read(make([]byte, 4))
	assert.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
	assert.Len(t, fake.ranges(), 1, "the size is known: no request at the end")

	n, err = obj.Read(nil)
	assert.Zero(t, n)
	require.NoError(t, err)
}

func TestSeek(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)

	buf := make([]byte, 3)
	_, err := io.ReadFull(obj, buf)
	require.NoError(t, err)
	assert.Equal(t, "abc", string(buf))

	pos, err := obj.Seek(3, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(3), pos)
	pos, err = obj.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(3), pos)
	_, err = io.ReadFull(obj, buf)
	require.NoError(t, err)
	assert.Equal(t, "def", string(buf))
	assert.Len(t, fake.ranges(), 1, "seeking where the reader already is keeps the stream")

	pos, err = obj.Seek(-3, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(23), pos)
	_, err = io.ReadFull(obj, buf)
	require.NoError(t, err)
	assert.Equal(t, "xyz", string(buf))

	pos, err = obj.Seek(-10, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(16), pos)
	_, err = io.ReadFull(obj, buf)
	require.NoError(t, err)
	assert.Equal(t, "qrs", string(buf))

	assert.Equal(t, []string{"bytes=0-", "bytes=23-", "bytes=16-"}, fake.ranges())
	assert.Empty(t, fake.heads, "the first response told the size")
	assert.Equal(t, 1, fake.openBodies(), "abandoned streams are closed")

	_, err = obj.Seek(-1, io.SeekStart)
	require.ErrorIs(t, err, fs.ErrInvalid)
	_, err = obj.Seek(0, 7)
	require.ErrorIs(t, err, fs.ErrInvalid)
	pos, err = obj.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(19), pos, "a failed seek leaves the position alone")
}

func TestSeekEndAsksForTheSize(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)
	pos, err := obj.Seek(-5, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(21), pos)
	require.Len(t, fake.heads, 1)

	rest, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, "vwxyz", string(rest))
	assert.Equal(t, fake.objects["key"].etag, aws.ToString(fake.gets[0].IfMatch), "the HEAD pinned the ETag")

	size, err := obj.Size()
	require.NoError(t, err)
	assert.Equal(t, int64(26), size)
	assert.Len(t, fake.heads, 1, "the size is cached")
}

func TestReadPastTheEnd(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)
	_, err := obj.Seek(100, io.SeekStart)
	require.NoError(t, err)
	n, err := obj.Read(make([]byte, 4))
	assert.Zero(t, n)
	require.ErrorIs(t, err, io.EOF, "S3 answers InvalidRange, a file reads nothing")
	assert.Equal(t, []string{"bytes=100-"}, fake.ranges())

	empty, _ := newObject(t, "")
	data, err := io.ReadAll(empty)
	require.NoError(t, err)
	assert.Empty(t, data)
}

func TestReadAt(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)

	buf := make([]byte, 4)
	n, err := obj.ReadAt(buf, 10)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "klmn", string(buf))
	n, err = obj.ReadAt(buf, 14)
	require.NoError(t, err)
	assert.Equal(t, "opqr", string(buf[:n]))
	assert.Equal(t, []string{"bytes=10-"}, fake.ranges(), "reads that follow each other share a stream")

	n, err = obj.ReadAt(buf, 24)
	assert.Equal(t, 2, n)
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, "yz", string(buf[:n]))

	n, err = obj.ReadAt(buf, 26)
	assert.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)

	_, err = obj.ReadAt(buf, -1)
	require.ErrorIs(t, err, fs.ErrInvalid)
	n, err = obj.ReadAt(nil, 3)
	assert.Zero(t, n)
	require.NoError(t, err)

	pos, err := obj.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Zero(t, pos, "ReadAt does not move the reading position")
	first := make([]byte, 2)
	_, err = io.ReadFull(obj, first)
	require.NoError(t, err)
	assert.Equal(t, "ab", string(first))
}

func TestReadAtConcurrently(t *testing.T) {
	t.Parallel()
	obj, _ := newObject(t, strings.Repeat(alphabet, 100))
	got := make([]string, 26)
	errs := make([]error, 26)
	var wg sync.WaitGroup
	for i := range 26 {
		wg.Go(func() {
			buf := make([]byte, 26)
			n, err := obj.ReadAt(buf, int64(i*26+i))
			got[i], errs[i] = string(buf[:n]), err
		})
	}
	wg.Wait()
	for i := range 26 {
		require.NoError(t, errs[i])
		assert.Equal(t, alphabet[i:]+alphabet[:i], got[i])
	}
}

func TestWriteTo(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)
	_, err := obj.Seek(20, io.SeekStart)
	require.NoError(t, err)

	var buf bytes.Buffer
	n, err := io.Copy(&buf, obj)
	require.NoError(t, err)
	assert.Equal(t, int64(6), n)
	assert.Equal(t, "uvwxyz", buf.String())
	assert.Equal(t, []string{"bytes=20-"}, fake.ranges())

	pos, err := obj.Seek(0, io.SeekCurrent)
	require.NoError(t, err)
	assert.Equal(t, int64(26), pos)
	n, err = obj.WriteTo(&buf)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestOverwriteIsDetected(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet)
	buf := make([]byte, 5)
	_, err := io.ReadFull(obj, buf)
	require.NoError(t, err)

	fake.put("key", []byte(strings.ToUpper(alphabet)))

	_, err = obj.Seek(10, io.SeekStart)
	require.NoError(t, err)
	_, err = obj.Read(buf)
	require.ErrorIs(t, err, streaming.ErrObjectChanged)
	apiErr, ok := errors.AsType[smithy.APIError](err)
	require.True(t, ok, "the S3 error is wrapped")
	assert.Equal(t, "PreconditionFailed", apiErr.ErrorCode())

	_, err = obj.ReadAt(buf, 0)
	require.ErrorIs(t, err, streaming.ErrObjectChanged)

	fresh := streaming.NewS3Object(t.Context(), fake, "bucket", "key")
	_, err = fresh.Size()
	require.NoError(t, err)
	fake.put("key", []byte("again"))
	_, err = fresh.Read(buf)
	require.ErrorIs(t, err, streaming.ErrObjectChanged, "the HEAD pins the ETag too")
}

func TestErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	fake := newMemS3()
	obj := streaming.NewS3Object(t.Context(), fake, "bucket", "missing")
	_, err := obj.Read(make([]byte, 1))
	require.Error(t, err)
	require.NotErrorIs(t, err, streaming.ErrObjectChanged)
	apiErr, ok := errors.AsType[smithy.APIError](err)
	require.True(t, ok)
	assert.Equal(t, "NoSuchKey", apiErr.ErrorCode())
	assert.Contains(t, err.Error(), "s3://bucket/missing")

	_, err = obj.Size()
	require.Error(t, err)
	_, err = obj.Seek(0, io.SeekEnd)
	require.Error(t, err)
	_, err = obj.ReadAt(make([]byte, 1), 0)
	require.Error(t, err)
	_, err = obj.WriteTo(io.Discard)
	require.Error(t, err)
}

func TestClose(t *testing.T) {
	t.Parallel()
	fake := newMemS3()
	fake.put("key", []byte(alphabet))
	obj := streaming.NewS3Object(t.Context(), fake, "bucket", "key")
	_, err := obj.Read(make([]byte, 2))
	require.NoError(t, err)
	_, err = obj.ReadAt(make([]byte, 2), 5)
	require.NoError(t, err)
	assert.Equal(t, 2, fake.openBodies())

	require.NoError(t, obj.Close())
	assert.Zero(t, fake.openBodies())
	require.NoError(t, obj.Close(), "closing twice does nothing")

	_, err = obj.Read(make([]byte, 1))
	require.ErrorIs(t, err, fs.ErrClosed)
	_, err = obj.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, fs.ErrClosed)
	_, err = obj.Seek(0, io.SeekStart)
	require.ErrorIs(t, err, fs.ErrClosed)
	_, err = obj.WriteTo(io.Discard)
	require.ErrorIs(t, err, fs.ErrClosed)
}

func TestRequestOptions(t *testing.T) {
	t.Parallel()
	fake := newMemS3()
	version := fake.put("key", []byte("first"))
	fake.put("key", []byte("second"))
	key := bytes.Repeat([]byte{7}, 32)
	sum := md5.Sum(key) //nolint:gosec // G401: S3 checks SSE-C keys with MD5
	calls := 0

	obj := streaming.NewS3Object(t.Context(), fake, "bucket", "key",
		streaming.WithVersionID(version),
		streaming.WithSSECustomerKey(key),
		streaming.WithRequestPayer(types.RequestPayerRequester),
		streaming.WithExpectedBucketOwner("123456789012"),
		streaming.WithGetObjectInput(func(in *s3.GetObjectInput) {
			in.ResponseContentType = aws.String("text/plain")
			in.Bucket = aws.String("ignored")
			in.Range = aws.String("bytes=1-1")
		}),
		streaming.WithHeadObjectInput(func(in *s3.HeadObjectInput) {
			in.ChecksumMode = types.ChecksumModeEnabled
			in.Key = aws.String("ignored")
		}),
		streaming.WithClientOptions(func(*s3.Options) { calls++ }),
	)
	defer func() { require.NoError(t, obj.Close()) }()
	assert.Equal(t, version, obj.VersionID())

	size, err := obj.Size()
	require.NoError(t, err)
	assert.Equal(t, int64(5), size, "the version asked for")
	data, err := io.ReadAll(obj)
	require.NoError(t, err)
	assert.Equal(t, "first", string(data))

	get := fake.gets[0]
	assert.Equal(t, "bucket", aws.ToString(get.Bucket), "Bucket is always the object's")
	assert.Equal(t, "bytes=0-", aws.ToString(get.Range), "Range is always the object's")
	assert.Equal(t, version, aws.ToString(get.VersionId))
	assert.Equal(t, "AES256", aws.ToString(get.SSECustomerAlgorithm))
	assert.Equal(t, base64.StdEncoding.EncodeToString(key), aws.ToString(get.SSECustomerKey))
	assert.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), aws.ToString(get.SSECustomerKeyMD5))
	assert.Equal(t, types.RequestPayerRequester, get.RequestPayer)
	assert.Equal(t, "123456789012", aws.ToString(get.ExpectedBucketOwner))
	assert.Equal(t, "text/plain", aws.ToString(get.ResponseContentType))

	head := fake.heads[0]
	assert.Equal(t, "key", aws.ToString(head.Key))
	assert.Equal(t, version, aws.ToString(head.VersionId))
	assert.Equal(t, "AES256", aws.ToString(head.SSECustomerAlgorithm))
	assert.Equal(t, types.RequestPayerRequester, head.RequestPayer)
	assert.Equal(t, "123456789012", aws.ToString(head.ExpectedBucketOwner))
	assert.Equal(t, types.ChecksumModeEnabled, head.ChecksumMode)
}

func TestIfMatchOfTheCallerWins(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, alphabet, streaming.WithGetObjectInput(func(in *s3.GetObjectInput) {
		in.IfMatch = aws.String(`"stale"`)
	}))
	_, err := obj.Read(make([]byte, 1))
	require.ErrorIs(t, err, streaming.ErrObjectChanged)
	assert.Equal(t, `"stale"`, aws.ToString(fake.gets[0].IfMatch))
}

func TestZip(t *testing.T) {
	t.Parallel()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	files := map[string]string{
		"a.txt":      strings.Repeat("hello ", 1000),
		"dir/b.json": `{"ok":true}`,
	}
	for _, name := range []string{"a.txt", "dir/b.json"} {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = io.WriteString(w, files[name])
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	obj, fake := newObject(t, archive.String())
	zr, err := obj.Zip()
	require.NoError(t, err)
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"a.txt", "dir/b.json"}, names)

	member, err := zr.Open("dir/b.json")
	require.NoError(t, err)
	content, err := io.ReadAll(member)
	require.NoError(t, err)
	require.NoError(t, member.Close())
	assert.JSONEq(t, files["dir/b.json"], string(content))

	before := len(fake.ranges())
	member, err = zr.Open("a.txt")
	require.NoError(t, err)
	content, err = io.ReadAll(member)
	require.NoError(t, err)
	require.NoError(t, member.Close())
	assert.Equal(t, files["a.txt"], string(content))
	assert.LessOrEqual(t, len(fake.ranges())-before, 2, "a member streams with few requests")

	notZip, _ := newObject(t, "not a zip archive")
	_, err = notZip.Zip()
	require.Error(t, err)
}

func TestDefaultClient(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://127.0.0.1:1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	obj := streaming.NewS3Object(ctx, nil, "bucket", "key")
	_, err := obj.Read(make([]byte, 1))
	require.Error(t, err, "a client is built from the default configuration")
	require.ErrorIs(t, err, context.Canceled)
}
