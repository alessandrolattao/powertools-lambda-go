package streaming

import (
	"archive/zip"
	"context"
	"crypto/md5" //nolint:gosec // G501: S3 requires the MD5 of an SSE-C key as an integrity check, not for security
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/alessandrolattao/powertools-lambda-go/internal/debug"
)

// S3API is the part of the S3 client an S3Object calls; *s3.Client
// satisfies it.
type S3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
}

// ErrObjectChanged is what reading an object matches when it was overwritten
// after the reading began: the next range request no longer matches the
// ETag of the first response.
var ErrObjectChanged = errors.New("streaming: the S3 object changed while it was read")

// Option configures an S3Object.
type Option func(*options)

type options struct {
	versionID    string
	sseKey       string
	sseKeyMD5    string
	requestPayer types.RequestPayer
	bucketOwner  string
	getInput     []func(*s3.GetObjectInput)
	headInput    []func(*s3.HeadObjectInput)
	clientOpts   []func(*s3.Options)
}

// WithVersionID reads the given version of the object, in a versioned
// bucket.
func WithVersionID(id string) Option {
	return func(c *options) { c.versionID = id }
}

// WithSSECustomerKey reads an object encrypted with a customer-provided key
// (SSE-C, AES256): the raw 256-bit key, which the option encodes and
// checksums as S3 requires, as boto3 does.
func WithSSECustomerKey(key []byte) Option {
	return func(c *options) {
		sum := md5.Sum(key) //nolint:gosec // G401: S3 requires the MD5 of an SSE-C key as an integrity check, not for security
		c.sseKey = base64.StdEncoding.EncodeToString(key)
		c.sseKeyMD5 = base64.StdEncoding.EncodeToString(sum[:])
	}
}

// WithRequestPayer acknowledges the charges of a Requester Pays bucket
// (types.RequestPayerRequester).
func WithRequestPayer(payer types.RequestPayer) Option {
	return func(c *options) { c.requestPayer = payer }
}

// WithExpectedBucketOwner fails the requests if the bucket does not belong
// to the given account.
func WithExpectedBucketOwner(accountID string) Option {
	return func(c *options) { c.bucketOwner = accountID }
}

// WithGetObjectInput sets any other field of the GetObject requests
// (response header overrides, checksum mode, conditions), after the options
// above. Bucket, Key and Range always are the object's.
func WithGetObjectInput(fn func(*s3.GetObjectInput)) Option {
	return func(c *options) { c.getInput = append(c.getInput, fn) }
}

// WithHeadObjectInput sets any other field of the HeadObject request that
// reads the size, after the options above. Bucket and Key always are the
// object's.
func WithHeadObjectInput(fn func(*s3.HeadObjectInput)) Option {
	return func(c *options) { c.headInput = append(c.headInput, fn) }
}

// WithClientOptions applies fns to the client options of every request.
func WithClientOptions(fns ...func(*s3.Options)) Option {
	return func(c *options) { c.clientOpts = append(c.clientOpts, fns...) }
}

// S3Object reads an S3 object as a stream, fetching its bytes with ranged
// GetObject requests as they are read, so that objects larger than the
// function's memory can be processed. It implements io.Reader, io.Seeker,
// io.ReaderAt, io.WriterTo and io.Closer: Powertools Python's S3Object.
//
// The first response pins the object's ETag: every later request carries it
// in If-Match, so an object overwritten while it is read fails with
// ErrObjectChanged instead of mixing the bytes of two versions.
//
// Read, Seek and WriteTo share a position; ReadAt has its own, as io.ReaderAt
// requires. An S3Object is safe for concurrent use, though concurrent
// readers take turns.
type S3Object struct {
	// ctx bounds every request: io.Reader has no context parameter, so the
	// object keeps the one of the invocation that opened it.
	ctx    context.Context //nolint:containedctx // io.Reader methods take no context; the object belongs to one invocation
	client S3API
	bucket string
	key    string
	cfg    options

	mu     sync.Mutex
	closed bool
	size   int64
	sized  bool
	etag   string
	reader cursor // Read, Seek and WriteTo
	at     cursor // ReadAt
}

// cursor is a position in the object and the response body open there, if
// any.
type cursor struct {
	pos  int64
	body io.ReadCloser
}

func (c *cursor) close() error {
	if c.body == nil {
		return nil
	}
	err := c.body.Close()
	c.body = nil
	if err != nil {
		return fmt.Errorf("close the response body: %w", err)
	}
	return nil
}

// NewS3Object returns a stream over the object key of bucket. Nothing is
// requested until the first read. ctx bounds every request the object makes:
// pass the invocation's context. A nil client is replaced by one built from
// the default AWS configuration, once per process, as boto3 does.
func NewS3Object(ctx context.Context, client S3API, bucket, key string, opts ...Option) *S3Object {
	o := &S3Object{ctx: ctx, client: client, bucket: bucket, key: key}
	for _, opt := range opts {
		opt(&o.cfg)
	}
	return o
}

// defaultClient is the client of objects created without one.
var defaultClient = sync.OnceValues(func() (S3API, error) {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("streaming: load the default AWS configuration: %w", err)
	}
	return s3.NewFromConfig(cfg), nil
})

func (o *S3Object) s3() (S3API, error) {
	if o.client == nil {
		client, err := defaultClient()
		if err != nil {
			return nil, err
		}
		o.client = client
	}
	return o.client, nil
}

// Bucket returns the bucket of the object.
func (o *S3Object) Bucket() string { return o.bucket }

// Key returns the key of the object.
func (o *S3Object) Key() string { return o.key }

// VersionID returns the version WithVersionID asked for, if any.
func (o *S3Object) VersionID() string { return o.cfg.versionID }

func (o *S3Object) path() string { return "s3://" + o.bucket + "/" + o.key }

// Size returns the size of the object, from the first response if a read
// already happened, from a HeadObject request otherwise.
func (o *S3Object) Size() (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sizeLocked()
}

func (o *S3Object) sizeLocked() (int64, error) {
	if o.sized {
		return o.size, nil
	}
	client, err := o.s3()
	if err != nil {
		return 0, err
	}
	in := &s3.HeadObjectInput{
		VersionId:            optional(o.cfg.versionID),
		RequestPayer:         o.cfg.requestPayer,
		ExpectedBucketOwner:  optional(o.cfg.bucketOwner),
		SSECustomerAlgorithm: o.sseAlgorithm(),
		SSECustomerKey:       optional(o.cfg.sseKey),
		SSECustomerKeyMD5:    optional(o.cfg.sseKeyMD5),
	}
	for _, fn := range o.cfg.headInput {
		fn(in)
	}
	in.Bucket, in.Key = aws.String(o.bucket), aws.String(o.key)
	if in.IfMatch == nil && o.etag != "" {
		in.IfMatch = aws.String(o.etag)
	}
	debug.Logger().Debug("getting the size of an S3 object", "object", o.path())
	out, err := client.HeadObject(o.ctx, in, o.cfg.clientOpts...)
	if err != nil {
		return 0, o.requestError("head", err, in.IfMatch != nil)
	}
	o.size, o.sized = aws.ToInt64(out.ContentLength), true
	if o.etag == "" {
		o.etag = aws.ToString(out.ETag)
	}
	return o.size, nil
}

// open starts a response body at off: a GetObject with an open range, so
// that sequential reads need a single request.
func (o *S3Object) open(off int64) (io.ReadCloser, error) {
	if o.sized && off >= o.size {
		return emptyBody{}, nil
	}
	client, err := o.s3()
	if err != nil {
		return nil, err
	}
	in := &s3.GetObjectInput{
		VersionId:            optional(o.cfg.versionID),
		RequestPayer:         o.cfg.requestPayer,
		ExpectedBucketOwner:  optional(o.cfg.bucketOwner),
		SSECustomerAlgorithm: o.sseAlgorithm(),
		SSECustomerKey:       optional(o.cfg.sseKey),
		SSECustomerKeyMD5:    optional(o.cfg.sseKeyMD5),
	}
	for _, fn := range o.cfg.getInput {
		fn(in)
	}
	in.Bucket, in.Key = aws.String(o.bucket), aws.String(o.key)
	in.Range = aws.String("bytes=" + strconv.FormatInt(off, 10) + "-")
	if in.IfMatch == nil && o.etag != "" {
		in.IfMatch = aws.String(o.etag)
	}
	debug.Logger().Debug("starting a new stream", "object", o.path(), "range", *in.Range)
	out, err := client.GetObject(o.ctx, in, o.cfg.clientOpts...)
	if err != nil {
		if hasCode(err, "InvalidRange", http.StatusRequestedRangeNotSatisfiable) {
			// S3 rejects a range that starts at or past the end, which
			// includes any range of an empty object: a file reads nothing
			// there.
			debug.Logger().Debug("position at or past the end of the object", "object", o.path(), "position", off)
			return emptyBody{}, nil
		}
		return nil, o.requestError("get", err, in.IfMatch != nil)
	}
	if o.etag == "" {
		o.etag = aws.ToString(out.ETag)
	}
	if !o.sized {
		if total, ok := totalSize(aws.ToString(out.ContentRange)); ok {
			o.size, o.sized = total, true
		}
	}
	return out.Body, nil
}

func (o *S3Object) sseAlgorithm() *string {
	if o.cfg.sseKey == "" {
		return nil
	}
	return aws.String("AES256")
}

func (o *S3Object) requestError(op string, err error, conditional bool) error {
	if conditional && hasCode(err, "PreconditionFailed", http.StatusPreconditionFailed) {
		return fmt.Errorf("%w: %s %s: %w", ErrObjectChanged, op, o.path(), err)
	}
	return fmt.Errorf("streaming: %s %s: %w", op, o.path(), err)
}

// hasCode reports whether err is an S3 error with the given code or HTTP
// status.
func hasCode(err error, code string, status int) bool {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok && apiErr.ErrorCode() == code {
		return true
	}
	if respErr, ok := errors.AsType[statusError](err); ok && respErr.HTTPStatusCode() == status {
		return true
	}
	return false
}

// statusError is an error of the SDK that carries the HTTP status of the
// response.
type statusError interface {
	error
	HTTPStatusCode() int
}

// totalSize reads the size of the object from a Content-Range header,
// "bytes 0-99/100".
func totalSize(contentRange string) (int64, bool) {
	_, total, found := strings.Cut(contentRange, "/")
	if !found {
		return 0, false
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// Read reads from the current position, opening a ranged GetObject the first
// time and after a Seek that moves.
func (o *S3Object) Read(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, &fs.PathError{Op: opRead, Path: o.path(), Err: fs.ErrClosed}
	}
	if len(p) == 0 {
		return 0, nil
	}
	if o.reader.body == nil {
		body, err := o.open(o.reader.pos)
		if err != nil {
			return 0, err
		}
		o.reader.body = body
	}
	n, err := o.reader.body.Read(p)
	o.reader.pos += int64(n)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		_ = o.reader.close() //nolint:errcheck // the stream is spent; closing only releases the connection
		return n, io.EOF
	default:
		_ = o.reader.close() //nolint:errcheck // the read error is what the caller needs
		return n, fmt.Errorf("streaming: read %s: %w", o.path(), err)
	}
}

// Seek sets the position of the next Read: io.SeekStart, io.SeekCurrent or
// io.SeekEnd (which needs the size). The response body open at the old
// position is discarded only when the position moves, because some readers,
// such as zip ones, seek to where they already are.
func (o *S3Object) Seek(offset int64, whence int) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, &fs.PathError{Op: opSeek, Path: o.path(), Err: fs.ErrClosed}
	}
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = o.reader.pos + offset
	case io.SeekEnd:
		size, err := o.sizeLocked()
		if err != nil {
			return o.reader.pos, err
		}
		pos = size + offset
	default:
		return o.reader.pos, &fs.PathError{Op: opSeek, Path: o.path(), Err: fmt.Errorf("%w: whence %d", fs.ErrInvalid, whence)}
	}
	if pos < 0 {
		return o.reader.pos, &fs.PathError{Op: opSeek, Path: o.path(), Err: fmt.Errorf("%w: negative position %d", fs.ErrInvalid, pos)}
	}
	if pos != o.reader.pos {
		_ = o.reader.close() //nolint:errcheck // the stream is abandoned; closing only releases the connection
		o.reader.pos = pos
	}
	return pos, nil
}

// ReadAt reads len(p) bytes at off, without moving the position of Read.
// Reads that continue where the previous one ended share one response body,
// so that reading a zip member takes a single request.
func (o *S3Object) ReadAt(p []byte, off int64) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, &fs.PathError{Op: opReadAt, Path: o.path(), Err: fs.ErrClosed}
	}
	if off < 0 {
		return 0, &fs.PathError{Op: opReadAt, Path: o.path(), Err: fmt.Errorf("%w: negative offset %d", fs.ErrInvalid, off)}
	}
	if len(p) == 0 {
		return 0, nil
	}
	if o.at.body != nil && o.at.pos != off {
		_ = o.at.close() //nolint:errcheck // the stream is abandoned; closing only releases the connection
	}
	if o.at.body == nil {
		body, err := o.open(off)
		if err != nil {
			return 0, err
		}
		o.at.body, o.at.pos = body, off
	}
	n, err := io.ReadFull(o.at.body, p)
	o.at.pos += int64(n)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		_ = o.at.close() //nolint:errcheck // the stream is spent; closing only releases the connection
		return n, io.EOF
	default:
		_ = o.at.close() //nolint:errcheck // the read error is what the caller needs
		return n, fmt.Errorf("streaming: read %s: %w", o.path(), err)
	}
}

// WriteTo writes the object from the current position to its end into w,
// with a single request: io.Copy uses it.
func (o *S3Object) WriteTo(w io.Writer) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, &fs.PathError{Op: opRead, Path: o.path(), Err: fs.ErrClosed}
	}
	if o.reader.body == nil {
		body, err := o.open(o.reader.pos)
		if err != nil {
			return 0, err
		}
		o.reader.body = body
	}
	n, err := io.Copy(w, o.reader.body)
	o.reader.pos += n
	_ = o.reader.close() //nolint:errcheck // the stream is spent, or failed with the error returned below
	if err != nil {
		return n, fmt.Errorf("streaming: copy %s: %w", o.path(), err)
	}
	return n, nil
}

// Close releases the open response bodies. Reading afterwards fails; closing
// again does nothing.
func (o *S3Object) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	if err := errors.Join(o.reader.close(), o.at.close()); err != nil {
		return fmt.Errorf("streaming: close %s: %w", o.path(), err)
	}
	return nil
}

// Transform returns the object, from the current position, through the
// transforms: Powertools Python's S3Object.transform. Closing the result
// closes the object too.
func (o *S3Object) Transform(transforms ...Transform) (io.ReadCloser, error) {
	return Apply(o, transforms...)
}

// Zip opens the object as a zip archive, reading the central directory at
// its end and then only the members that are opened: Powertools Python's
// ZipTransform. Store and Deflate are supported; register other methods
// (LZMA, Zstandard, bzip2) on the returned reader with RegisterDecompressor.
func (o *S3Object) Zip() (*zip.Reader, error) {
	size, err := o.Size()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(o, size)
	if err != nil {
		return nil, fmt.Errorf("streaming: zip %s: %w", o.path(), err)
	}
	return zr, nil
}

// The operations PathError reports.
const (
	opRead   = "read"
	opSeek   = "seek"
	opReadAt = "readat"
)

// emptyBody is the response of a range past the end of the object.
type emptyBody struct{}

func (emptyBody) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyBody) Close() error             { return nil }
