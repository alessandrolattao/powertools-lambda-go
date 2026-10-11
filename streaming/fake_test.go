package streaming_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// memS3 is an S3 bucket in memory that answers ranged GetObject and
// HeadObject requests as S3 does, and records them.
type memS3 struct {
	mu      sync.Mutex
	objects map[string]*memObject
	gets    []*s3.GetObjectInput
	heads   []*s3.HeadObjectInput
	open    int // response bodies not closed yet
}

type memObject struct {
	data     []byte
	etag     string
	versions map[string][]byte
}

func newMemS3() *memS3 {
	return &memS3{objects: map[string]*memObject{}}
}

// put stores data under key, as a new version with a new ETag.
func (m *memS3) put(key string, data []byte) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		obj = &memObject{versions: map[string][]byte{}}
		m.objects[key] = obj
	}
	sum := sha256.Sum256(data)
	obj.data = data
	obj.etag = strconv.Quote(hex.EncodeToString(sum[:16]))
	version := "v" + strconv.Itoa(len(obj.versions)+1)
	obj.versions[version] = data
	return version
}

func (m *memS3) lookup(key, version, ifMatch *string) (data []byte, etag string, err error) {
	obj, ok := m.objects[aws.ToString(key)]
	if !ok {
		return nil, "", &smithy.GenericAPIError{Code: "NoSuchKey", Message: "The specified key does not exist."}
	}
	data = obj.data
	if version != nil {
		if data, ok = obj.versions[*version]; !ok {
			return nil, "", &smithy.GenericAPIError{Code: "NoSuchVersion", Message: "The specified version does not exist."}
		}
	}
	if ifMatch != nil && *ifMatch != obj.etag {
		return nil, "", &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
	}
	return data, obj.etag, nil
}

func (m *memS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets = append(m.gets, in)
	data, etag, err := m.lookup(in.Key, in.VersionId, in.IfMatch)
	if err != nil {
		return nil, err
	}
	start, end, err := byteRange(aws.ToString(in.Range), int64(len(data)))
	if err != nil {
		return nil, err
	}
	m.open++
	return &s3.GetObjectOutput{
		Body:          &memBody{Reader: bytes.NewReader(data[start : end+1]), s3: m},
		ContentLength: aws.Int64(end - start + 1),
		ContentRange:  aws.String(fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))),
		ETag:          aws.String(etag),
	}, nil
}

// byteRange reads a Range header, "bytes=first-" or "bytes=first-last".
func byteRange(header string, size int64) (start, end int64, err error) {
	if header == "" {
		return 0, size - 1, nil
	}
	first, last, _ := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if start, err = strconv.ParseInt(first, 10, 64); err != nil {
		return 0, 0, &smithy.GenericAPIError{Code: "InvalidArgument", Message: err.Error()}
	}
	end = size - 1
	if last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil {
			return 0, 0, &smithy.GenericAPIError{Code: "InvalidArgument", Message: err.Error()}
		}
		end = min(end, size-1)
	}
	if start >= size {
		return 0, 0, &smithy.GenericAPIError{Code: "InvalidRange", Message: "The requested range is not satisfiable"}
	}
	return start, end, nil
}

func (m *memS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heads = append(m.heads, in)
	data, etag, err := m.lookup(in.Key, in.VersionId, in.IfMatch)
	if err != nil {
		return nil, err
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data))), ETag: aws.String(etag)}, nil
}

// ranges returns the Range of every GetObject so far.
func (m *memS3) ranges() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.gets))
	for _, in := range m.gets {
		out = append(out, aws.ToString(in.Range))
	}
	return out
}

func (m *memS3) openBodies() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open
}

type memBody struct {
	io.Reader

	s3     *memS3
	closed bool
}

func (b *memBody) Close() error {
	if !b.closed {
		b.closed = true
		b.s3.mu.Lock()
		b.s3.open--
		b.s3.mu.Unlock()
	}
	return nil
}
