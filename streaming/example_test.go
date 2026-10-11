package streaming_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/alessandrolattao/powertools-lambda-go/streaming"
)

// sampleBucket holds the objects of the examples; a function passes its
// *s3.Client instead.
func sampleBucket() *memS3 {
	bucket := newMemS3()
	bucket.put("orders.csv", []byte("id,customer,total\n1,ann,12.50\n2,bob,7.00\n3,eve,30.25\n"))

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	check(io.WriteString(w, "id,customer,total\n1,ann,12.50\n2,bob,7.00\n"))
	check(0, w.Close())
	bucket.put("orders.csv.gz", gz.Bytes())

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	member, err := zw.Create("reports/today.txt")
	check(0, err)
	check(io.WriteString(member, "all good\n"))
	check(0, zw.Close())
	bucket.put("reports.zip", archive.Bytes())
	return bucket
}

func check(_ int, err error) {
	if err != nil {
		panic(err)
	}
}

func ExampleNewS3Object() {
	ctx := context.Background()
	obj := streaming.NewS3Object(ctx, sampleBucket(), "my-bucket", "orders.csv")
	for line, err := range streaming.Lines(obj) {
		if err != nil {
			panic(err)
		}
		fmt.Println(string(line))
	}
	if err := obj.Close(); err != nil {
		panic(err)
	}
	// Output:
	// id,customer,total
	// 1,ann,12.50
	// 2,bob,7.00
	// 3,eve,30.25
}

func ExampleS3Object_Transform() {
	obj := streaming.NewS3Object(context.Background(), sampleBucket(), "my-bucket", "orders.csv.gz")

	// Gunzip, then parse: Python's S3Object(..., is_gzip=True, is_csv=True).
	r, err := obj.Transform(streaming.Gzip())
	if err != nil {
		panic(err)
	}
	for row, err := range streaming.CSVDicts(r) {
		if err != nil {
			panic(err)
		}
		fmt.Println(row["customer"], row["total"])
	}
	if err := r.Close(); err != nil { // closes obj too
		panic(err)
	}
	// Output:
	// ann 12.50
	// bob 7.00
}

func ExampleS3Object_Seek() {
	obj := streaming.NewS3Object(context.Background(), sampleBucket(), "my-bucket", "orders.csv")

	// Read only the last row, which is 12 bytes long: the header is not
	// there, so name the columns.
	if _, err := obj.Seek(-12, io.SeekEnd); err != nil {
		panic(err)
	}
	for row, err := range streaming.CSVDicts(obj, streaming.WithFieldnames("id", "customer", "total")) {
		if err != nil {
			panic(err)
		}
		fmt.Println(row["id"], row["customer"])
	}
	if err := obj.Close(); err != nil {
		panic(err)
	}
	// Output: 3 eve
}

func ExampleS3Object_Zip() {
	obj := streaming.NewS3Object(context.Background(), sampleBucket(), "my-bucket", "reports.zip")
	archive, err := obj.Zip()
	if err != nil {
		panic(err)
	}
	for _, f := range archive.File {
		fmt.Println(f.Name)
	}
	member, err := archive.Open("reports/today.txt")
	if err != nil {
		panic(err)
	}
	content, err := io.ReadAll(member)
	if err != nil {
		panic(err)
	}
	fmt.Print(string(content))
	if err := errors.Join(member.Close(), obj.Close()); err != nil {
		panic(err)
	}
	// Output:
	// reports/today.txt
	// all good
}

func ExampleApply() {
	// A custom transformation: any func(io.Reader) (io.Reader, error).
	upper := func(r io.Reader) (io.Reader, error) {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(strings.ToUpper(string(data))), nil
	}

	// Transformations are tested on any reader, without S3.
	r, err := streaming.Apply(strings.NewReader("hello\n"), upper)
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	fmt.Print(string(data))
	if err := r.Close(); err != nil {
		panic(err)
	}
	// Output: HELLO
}

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

func ExampleJSONValues() {
	type event struct {
		Type string `json:"type"`
	}
	lines := "{\"type\":\"created\"}\n{\"type\":\"deleted\"}\n"
	for e, err := range streaming.JSONValues[event](strings.NewReader(lines)) {
		if err != nil {
			panic(err)
		}
		fmt.Println(e.Type)
	}
	// Output:
	// created
	// deleted
}
