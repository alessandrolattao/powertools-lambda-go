# Streaming

The streaming utility processes S3 objects larger than the function's memory
as streams of data.

## Key features

- Stream Amazon S3 objects through the standard `io` interfaces, with minimal
  memory consumption
- Seek anywhere in an object without downloading what is skipped
- Built-in transformations and decoders: gzip, CSV (also as dictionaries),
  ZIP, lines, JSON Lines
- Build your own transformation and chain it with the others
- Overwrites detected: an object that changes while it is read fails instead
  of mixing two versions

## Background

Within Lambda, processing S3 objects larger than the allocated memory leads
to out-of-memory or timeout errors. Objects are also often compressed or
encoded (gzip, CSV, zip), which adds non-business code and reliability risks.

`S3Object` fetches parts of the object as they are read, with ranged
`GetObject` requests, and the transformations apply to the stream as it
flows. Processing one, a few or all the rows of a large dataset takes a few
MB of memory.

## Getting started

### Streaming from an S3 object

`NewS3Object` takes the invocation's context, an S3 client, the bucket and
the key. Nothing is requested until the first read.

```go
func handler(ctx context.Context, event events.S3Event) error {
	record := event.Records[0].S3
	obj := streaming.NewS3Object(ctx, client, record.Bucket.Name, record.Object.Key)
	defer obj.Close()

	for line, err := range streaming.Lines(obj) {
		if err != nil {
			return err
		}
		process(line)
	}
	return nil
}
```

`client` is an `*s3.Client` built once, in `main` or at package level; a nil
client is replaced by one built from the default AWS configuration.

For a versioned bucket, read a given version with `WithVersionID`:

```go
obj := streaming.NewS3Object(ctx, client, bucket, key, streaming.WithVersionID(versionID))
```

`S3Object` is an `io.Reader`, `io.Seeker`, `io.ReaderAt`, `io.WriterTo` and
`io.Closer`, so it plugs into everything that reads files: `bufio`,
`encoding/csv`, `io.Copy`, `archive/zip`.

### Data transformations

A `Transform` turns a byte stream into another one. `Transform` (or `Apply`
on any `io.Reader`) chains transformations in order, and the decoders turn
the result into an iterator:

```go
obj := streaming.NewS3Object(ctx, client, bucket, "orders.csv.gz")

// Gunzip, then parse: Python's S3Object(..., is_gzip=True, is_csv=True).
r, err := obj.Transform(streaming.Gzip())
if err != nil {
	return err
}
defer r.Close() // closes obj too

for row, err := range streaming.CSVDicts(r) {
	if err != nil {
		return err
	}
	fmt.Println(row["customer"], row["total"])
}
```

#### Handling ZIP files

`Zip` opens the object as a zip archive: it reads the central directory at
the end of the object, then only the members opened, each streamed with one
request.

```go
archive, err := obj.Zip()
if err != nil {
	return err
}
for _, f := range archive.File {
	fmt.Println(f.Name)
}
member, err := archive.Open("reports/today.txt")
```

As in Python, a zip archive cannot feed another transformation (it holds
many files, not one stream), but each member can:
`streaming.Apply(member, streaming.Gzip())`.

#### Built-in transformations and decoders

| Name | Description | Python |
| ---- | ----------- | ------ |
| `Gzip()` | Decompresses gzip, every member | `GzipTransform` |
| `S3Object.Zip()` | Opens a zip archive, `*zip.Reader` | `ZipTransform` |
| `CSVDicts(r, opts...)` | CSV rows as `map[string]string` | `CsvTransform` |
| `CSV(r, opts...)` | CSV rows as `[]string` | |
| `Lines(r)` | Lines without their line ending | iterating a `GzipFile` |
| `JSONValues[T](r)` | JSON Lines or concatenated JSON into `T` | the custom JSON transform of the docs |

## Advanced

### Skipping or reading backwards

`Seek` moves the reading position without downloading anything; the next read
opens a request there. To read only the last row of a large CSV, whose length
is known:

```go
obj := streaming.NewS3Object(ctx, client, bucket, "orders.csv")
defer obj.Close()

// Read only the last row, which is 12 bytes long: the header is not
// there, so name the columns.
if _, err := obj.Seek(-12, io.SeekEnd); err != nil {
	return err
}
for row, err := range streaming.CSVDicts(obj, streaming.WithFieldnames("id", "customer", "total")) {
	if err != nil {
		return err
	}
	fmt.Println(row["id"], row["customer"])
}
```

To skip the first N rows of a CSV whose rows all have the same length, seek to
`header + N*rowLength` the same way. `Seek` only opens a new request when the
position actually moves.

`ReadAt` reads at any offset without moving the reading position; reads that
follow each other share one request.

### Custom options for S3 requests

| Option | Use |
| ------ | --- |
| `WithVersionID(id)` | Read a version of a versioned bucket |
| `WithSSECustomerKey(key)` | Read an object encrypted with a customer key (SSE-C); the raw 256-bit key, encoded and checksummed for you |
| `WithRequestPayer(types.RequestPayerRequester)` | Read from a Requester Pays bucket |
| `WithExpectedBucketOwner(accountID)` | Fail if the bucket belongs to another account |
| `WithGetObjectInput(fn)` / `WithHeadObjectInput(fn)` | Set any other field of the requests (response header overrides, checksum mode, conditions) |
| `WithClientOptions(fns...)` | Client options of every request |

The typed options apply to both `GetObject` and `HeadObject` (the size of an
SSE-C object needs the key too); `Bucket`, `Key` and `Range` always are the
object's.

### Custom options for CSV

| Option | Python |
| ------ | ------ |
| `WithDelimiter('\t')` | `delimiter` (TSV) |
| `WithComment('#')` | |
| `WithLazyQuotes()` | (Python is lenient with stray quotes by default) |
| `WithTrimLeadingSpace()` | `skipinitialspace` |
| `WithFieldnames(names...)` | `fieldnames` |
| `WithRestKey(key)` | `restkey`; the extra values are joined by the delimiter |
| `WithRestValue(value)` | `restval` |

A UTF-8 byte order mark at the start is skipped. The quote character is
always `"`, and the text must be UTF-8: decode other encodings with a custom
transformation.

### Consistency

The first response pins the object's ETag, and every later request carries
it in `If-Match`. If the object is overwritten while it is read, the next
request fails with `ErrObjectChanged`:

```go
if errors.Is(err, streaming.ErrObjectChanged) {
	// Start over, or read a fixed version with WithVersionID.
}
```

Powertools Python does not check, and a seek after an overwrite reads the
new bytes as if they were the old object's.

### Building your own transformation

A transformation is any `func(io.Reader) (io.Reader, error)`. If what it
returns is an `io.Closer`, it is closed with the pipeline.

```go
// A custom transformation: any func(io.Reader) (io.Reader, error).
upper := func(r io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(strings.ToUpper(string(data))), nil
}

r, err := streaming.Apply(strings.NewReader("hello\n"), upper)
```

A character set decoder from `golang.org/x/text` is one line:
`func(r io.Reader) (io.Reader, error) { return transform.NewReader(r, charmap.Windows1252.NewDecoder()), nil }`.

## Testing your code

Transformations and decoders work on any `io.Reader`: test them on a
`strings.Reader` or `bytes.Reader`, without S3.

```go
func ExampleCSVDicts() {
	tsv := "id\tname\n1\tann\n2\tbob\n"
	for row, err := range streaming.CSVDicts(strings.NewReader(tsv), streaming.WithDelimiter('\t')) {
		if err != nil {
			panic(err)
		}
		fmt.Println(row["id"], row["name"])
	}
	// Output:
	// 1 ann
	// 2 bob
}
```

`NewS3Object` takes an `S3API`, the two methods it calls (`GetObject`,
`HeadObject`): a fake implementing them answers with in-memory data. It must
honor the `Range` header (`bytes=N-`) and report the total size in
`ContentRange` (`bytes 0-99/100`), as S3 does.

## Known limitations

### Tracing

Every range request is an S3 API call. With the S3 client instrumented by
OpenTelemetry, each one becomes a span, which adds up on large objects: keep
tracing away from the reading loop.

## Differences from Powertools Python

- `S3Object` is always the raw object. Python's `is_gzip`/`is_csv` constructor
  flags and `transform(in_place=True)` make the object yield transformed data
  while seeking in raw bytes; Go composes: `obj.Transform(...)` returns a new
  reader, `Seek` stays on the raw object.
- The context of the requests is the one given to `NewS3Object`.
- A missing CSV column is `""`, not `None`.
- No Powertools user agent is added to the requests.
- The file methods Python has to reject (`write`, `fileno`, `truncate`) do not
  exist.
