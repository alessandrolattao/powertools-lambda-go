// Package streaming processes S3 objects larger than the function's memory
// as streams: the Go side of Powertools Python's streaming utility.
//
// # Reading an object
//
// S3Object fetches the bytes of an object with ranged GetObject requests, as
// they are read, and seeks without downloading what it skips:
//
//	obj := streaming.NewS3Object(ctx, client, bucket, key)
//	defer obj.Close()
//	for line, err := range streaming.Lines(obj) {
//		...
//	}
//
// It is an io.Reader, io.Seeker, io.ReaderAt, io.WriterTo and io.Closer, so
// it plugs into anything that reads files. Nothing is requested until the
// first read; a sequential read takes one request, and a Seek opens a new one
// only when the position moves. Reads at or past the end return io.EOF, as
// for a file. Size reports the size, from the first response or from a
// HeadObject request.
//
// Options pass what S3 needs to serve the object: WithVersionID for a
// versioned bucket, WithSSECustomerKey for SSE-C, WithRequestPayer,
// WithExpectedBucketOwner, and WithGetObjectInput / WithHeadObjectInput for
// any other request field.
//
// # Consistency
//
// The first response pins the object's ETag, and every later request
// carries it in If-Match: an object overwritten while it is read fails with
// ErrObjectChanged instead of returning the bytes of two versions. Powertools
// Python does not check; WithVersionID reads a version that cannot change.
//
// # Transformations
//
// A Transform turns a byte stream into another one: Gzip decompresses,
// and any func(io.Reader) (io.Reader, error) is a custom transformation
// (decryption, a character set decoder from golang.org/x/text). Apply, or
// S3Object.Transform, chains them in order:
//
//	r, err := obj.Transform(streaming.Gzip())
//	defer r.Close()
//	for row, err := range streaming.CSVDicts(r) {
//		fmt.Println(row["id"])
//	}
//
// The decoders turn a stream into an iterator: CSVDicts (Python's
// CsvTransform, a csv.DictReader) and CSV, Lines, and JSONValues for JSON
// Lines. They work on any io.Reader, so transformations are tested on a
// strings.Reader, without S3.
//
// S3Object.Zip opens a zip archive with random access: the central
// directory at the end, then only the members opened, each streamed (Python's
// ZipTransform). As in Python, a zip archive cannot feed another
// transformation, but each member can.
//
// # Differences from Powertools Python
//
//   - S3Object is the raw object. Python's constructor flags (is_gzip,
//     is_csv) and transform(in_place=True) make the object read transformed
//     data while it still seeks in raw bytes; Go composes instead:
//     obj.Transform returns a new reader, and Seek stays on the raw object.
//     Seek before transforming to start somewhere else, as the Python
//     recipes do.
//   - CSV uses encoding/csv: the delimiter, comment character, lazy quotes
//     and leading space trimming are options; the quote character is always
//     '"'. Text must be UTF-8: decode other encodings with a Transform. A
//     missing column is "" (WithRestValue), not None, and the extra values
//     of a long row are dropped unless WithRestKey keeps them, joined.
//   - The reading context is the one given to NewS3Object: io.Reader has no
//     context parameter.
//   - Reads are checked against the first ETag (see Consistency).
//   - The file methods Python must raise NotImplementedError for (write,
//     fileno, truncate) do not exist; readline and 1024-byte chunk iteration
//     become Lines and plain Read calls.
//   - Powertools Python tags its S3 requests with a Powertools user agent;
//     this port does not pose as the official library.
//
// # Tracing
//
// Every range request is an S3 call. Instrumenting the S3 client with
// OpenTelemetry records each one as a span, which adds up on large objects:
// as Powertools Python advises for X-Ray, keep tracing away from the reading
// loop.
//
// The package reads no environment variable of its own; a client built from
// the default configuration follows the AWS SDK's.
package streaming
