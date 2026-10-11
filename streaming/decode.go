package streaming

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
)

// CSVOption configures CSV and CSVDicts, as the dialect options of Python's
// csv module do.
type CSVOption func(*csvOptions)

type csvOptions struct {
	comma            rune
	comment          rune
	lazyQuotes       bool
	trimLeadingSpace bool
	fieldnames       []string
	restKey          string
	hasRestKey       bool
	restValue        string
}

// WithDelimiter separates fields with r instead of a comma: '\t' for TSV.
func WithDelimiter(r rune) CSVOption {
	return func(o *csvOptions) { o.comma = r }
}

// WithComment skips the lines that start with r.
func WithComment(r rune) CSVOption {
	return func(o *csvOptions) { o.comment = r }
}

// WithLazyQuotes accepts quotes in unquoted fields and unescaped quotes in
// quoted ones.
func WithLazyQuotes() CSVOption {
	return func(o *csvOptions) { o.lazyQuotes = true }
}

// WithTrimLeadingSpace ignores the spaces after a delimiter: Python's
// skipinitialspace.
func WithTrimLeadingSpace() CSVOption {
	return func(o *csvOptions) { o.trimLeadingSpace = true }
}

// WithFieldnames names the columns for CSVDicts, which then reads the first
// row as data rather than as the header.
func WithFieldnames(names ...string) CSVOption {
	return func(o *csvOptions) { o.fieldnames = names }
}

// WithRestKey keeps the values of a row longer than the header under key,
// joined by the delimiter. Without it they are dropped.
func WithRestKey(key string) CSVOption {
	return func(o *csvOptions) { o.restKey, o.hasRestKey = key, true }
}

// WithRestValue fills the columns a short row lacks with value instead of
// "".
func WithRestValue(value string) CSVOption {
	return func(o *csvOptions) { o.restValue = value }
}

func csvConfig(opts []CSVOption) csvOptions {
	o := csvOptions{comma: ','}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// CSV returns the rows of the CSV stream r. A UTF-8 byte order mark at the
// start is skipped. Rows may have different lengths, as in Python's csv
// module. A malformed row comes with its *csv.ParseError and the iteration
// goes on; a read error ends it.
func CSV(r io.Reader, opts ...CSVOption) iter.Seq2[[]string, error] {
	o := csvConfig(opts)
	return func(yield func([]string, error) bool) {
		reader := o.reader(r)
		for {
			row, err := reader.Read()
			if errors.Is(err, io.EOF) || !yieldRow(yield, row, err) {
				return
			}
		}
	}
}

// yieldRow hands a row, or its error, to yield and reports whether the
// iteration goes on: not when the loop breaks, nor after a read error, which
// a csv.Reader cannot recover from.
func yieldRow(yield func([]string, error) bool, row []string, err error) bool {
	if err == nil {
		return yield(row, nil)
	}
	var malformed *csv.ParseError
	return yield(nil, fmt.Errorf("streaming: csv: %w", err)) && errors.As(err, &malformed)
}

func (o *csvOptions) reader(r io.Reader) *csv.Reader {
	reader := csv.NewReader(skipBOM(r))
	reader.Comma = o.comma
	reader.Comment = o.comment
	reader.LazyQuotes = o.lazyQuotes
	reader.TrimLeadingSpace = o.trimLeadingSpace
	reader.FieldsPerRecord = -1
	return reader
}

// CSVDicts returns the rows of the CSV stream r as maps from column name to
// value: Powertools Python's CsvTransform, a csv.DictReader. The first row
// names the columns, unless WithFieldnames does. A short row has the columns
// it lacks set to "" (WithRestValue); a long row loses the extra values
// (WithRestKey keeps them).
func CSVDicts(r io.Reader, opts ...CSVOption) iter.Seq2[map[string]string, error] {
	o := csvConfig(opts)
	return func(yield func(map[string]string, error) bool) {
		header := o.fieldnames
		for row, err := range CSV(r, opts...) {
			if err != nil {
				if !yield(nil, err) {
					return
				}
				continue
			}
			if header == nil {
				header = row
				continue
			}
			if !yield(o.dict(header, row), nil) {
				return
			}
		}
	}
}

func (o *csvOptions) dict(header, row []string) map[string]string {
	dict := make(map[string]string, len(header)+1)
	for i, name := range header {
		if i < len(row) {
			dict[name] = row[i]
		} else {
			dict[name] = o.restValue
		}
	}
	if o.hasRestKey && len(row) > len(header) {
		dict[o.restKey] = strings.Join(row[len(header):], string(o.comma))
	}
	return dict
}

var bom = []byte{0xEF, 0xBB, 0xBF}

// skipBOM drops the UTF-8 byte order mark Excel writes at the start of a
// CSV file, which would otherwise end up in the first column name.
func skipBOM(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if head, err := br.Peek(len(bom)); err == nil && bytes.Equal(head, bom) {
		_, _ = br.Discard(len(bom)) //nolint:errcheck // the bytes were just peeked
	}
	return br
}

// Lines returns the lines of r without their line ending ("\n" or "\r\n"),
// whatever their length: what iterating a gunzipped stream gives in Python.
// The last line needs no line ending. A read error ends the iteration.
func Lines(r io.Reader) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
				if !yield(line, nil) {
					return
				}
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(nil, fmt.Errorf("streaming: lines: %w", err))
				return
			}
		}
	}
}

// JSONValues returns the JSON values of r decoded into T, one at a time:
// newline-delimited JSON (JSON Lines) or values simply concatenated. It is
// the JSON transform of the Powertools Python documentation, with
// encoding/json/v2 and the options given. A malformed value ends the
// iteration with its error.
func JSONValues[T any](r io.Reader, opts ...json.Options) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		dec := jsontext.NewDecoder(r, opts...)
		for {
			var v T
			err := json.UnmarshalDecode(dec, &v, opts...)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(v, fmt.Errorf("streaming: json: %w", err))
				return
			}
			if !yield(v, nil) {
				return
			}
		}
	}
}
