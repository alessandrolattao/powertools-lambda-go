package streaming_test

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/streaming"
)

func gzipped(t *testing.T, members ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, member := range members {
		w := gzip.NewWriter(&buf)
		_, err := io.WriteString(w, member)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}
	return buf.Bytes()
}

func TestGzipTransform(t *testing.T) {
	t.Parallel()
	obj, fake := newObject(t, string(gzipped(t, "first member\n", "second member\n")))

	r, err := obj.Transform(streaming.Gzip())
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "first member\nsecond member\n", string(data), "every gzip member, as GzipFile reads them")

	require.NoError(t, r.Close())
	assert.Zero(t, fake.openBodies())
	_, err = obj.Read(make([]byte, 1))
	require.Error(t, err, "closing the transformed stream closes the object")
}

func TestGzipOfNothing(t *testing.T) {
	t.Parallel()
	r, err := streaming.Apply(strings.NewReader(""), streaming.Gzip())
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Empty(t, data)

	_, err = streaming.Apply(strings.NewReader("not gzip at all"), streaming.Gzip())
	require.ErrorIs(t, err, gzip.ErrHeader)
}

type closeRecorder struct {
	io.Reader

	name   string
	closed *[]string
}

func (c closeRecorder) Close() error {
	*c.closed = append(*c.closed, c.name)
	return nil
}

func TestApplyComposesAndCloses(t *testing.T) {
	t.Parallel()
	var closed []string
	upper := func(r io.Reader) (io.Reader, error) {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return closeRecorder{Reader: strings.NewReader(strings.ToUpper(string(data))), name: "upper", closed: &closed}, nil
	}
	source := closeRecorder{Reader: bytes.NewReader(gzipped(t, "hello")), name: "source", closed: &closed}

	r, err := streaming.Apply(source, streaming.Gzip(), upper)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "HELLO", string(data))
	require.NoError(t, r.Close())
	assert.Equal(t, []string{"upper", "source"}, closed, "outermost first; gzip.Reader closes too")

	closed = nil
	boom := errors.New("boom")
	_, err = streaming.Apply(closeRecorder{Reader: strings.NewReader("x"), name: "source", closed: &closed},
		func(io.Reader) (io.Reader, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
	assert.Equal(t, []string{"source"}, closed, "a failed pipeline releases what it opened")

	r, err = streaming.Apply(strings.NewReader("as is"))
	require.NoError(t, err)
	data, err = io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "as is", string(data))
	require.NoError(t, r.Close())
}

// TestReadTheLastRow is the recipe of the Powertools Python documentation:
// seek to the last row of a large CSV, then parse from there.
func TestReadTheLastRow(t *testing.T) {
	t.Parallel()
	const sample = "id,name,location\n1,Ruben Fonseca, Denmark\n2,Heitor Lessa, Netherlands\n3,Leandro Damascena, Portugal\n"
	obj, fake := newObject(t, sample)
	lastRow := "3,Leandro Damascena, Portugal\n"

	_, err := obj.Seek(-int64(len(lastRow)), io.SeekEnd)
	require.NoError(t, err)
	var rows []map[string]string
	for row, err := range streaming.CSVDicts(obj, streaming.WithFieldnames("id", "name", "location"), streaming.WithTrimLeadingSpace()) {
		require.NoError(t, err)
		rows = append(rows, row)
	}
	assert.Equal(t, []map[string]string{{"id": "3", "name": "Leandro Damascena", "location": "Portugal"}}, rows)
	assert.Equal(t, []string{"bytes=70-"}, fake.ranges())
}

func TestCSVDicts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		opts  []streaming.CSVOption
		want  []map[string]string
	}{
		{
			name:  "header row",
			input: "id,name\n1,ann\n2,bob\n",
			want:  []map[string]string{{"id": "1", "name": "ann"}, {"id": "2", "name": "bob"}},
		},
		{
			name:  "byte order mark",
			input: "\xef\xbb\xbfid,name\r\n1,ann\r\n",
			want:  []map[string]string{{"id": "1", "name": "ann"}},
		},
		{
			name:  "fieldnames",
			input: "1,ann\n",
			opts:  []streaming.CSVOption{streaming.WithFieldnames("id", "name")},
			want:  []map[string]string{{"id": "1", "name": "ann"}},
		},
		{
			name:  "short row",
			input: "id,name,city\n1,ann\n",
			opts:  []streaming.CSVOption{streaming.WithRestValue("n/a")},
			want:  []map[string]string{{"id": "1", "name": "ann", "city": "n/a"}},
		},
		{
			name:  "long row dropped",
			input: "id\n1,x,y\n",
			want:  []map[string]string{{"id": "1"}},
		},
		{
			name:  "long row kept",
			input: "id\n1,x,y\n",
			opts:  []streaming.CSVOption{streaming.WithRestKey("rest")},
			want:  []map[string]string{{"id": "1", "rest": "x,y"}},
		},
		{
			name:  "tab separated",
			input: "id\tname\n1\tann, the first\n",
			opts:  []streaming.CSVOption{streaming.WithDelimiter('\t')},
			want:  []map[string]string{{"id": "1", "name": "ann, the first"}},
		},
		{
			name:  "comments and blank lines",
			input: "# exported\nid,name\n\n1,ann\n",
			opts:  []streaming.CSVOption{streaming.WithComment('#')},
			want:  []map[string]string{{"id": "1", "name": "ann"}},
		},
		{
			name:  "quotes",
			input: "id,quote\n1,\"said \"\"hi\"\", left\"\n",
			want:  []map[string]string{{"id": "1", "quote": `said "hi", left`}},
		},
		{
			name:  "lazy quotes",
			input: "id,quote\n1,a \"quoted\" word\n",
			opts:  []streaming.CSVOption{streaming.WithLazyQuotes()},
			want:  []map[string]string{{"id": "1", "quote": `a "quoted" word`}},
		},
		{
			name:  "empty",
			input: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rows []map[string]string
			for row, err := range streaming.CSVDicts(strings.NewReader(tc.input), tc.opts...) {
				require.NoError(t, err)
				rows = append(rows, row)
			}
			assert.Equal(t, tc.want, rows)
		})
	}
}

func TestCSVErrors(t *testing.T) {
	t.Parallel()
	input := "id,quote\n1,a \"bad\" quote\n2,fine\n"
	var rows []map[string]string
	var errs []error
	for row, err := range streaming.CSVDicts(strings.NewReader(input)) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rows = append(rows, row)
	}
	require.Len(t, errs, 1)
	parseErr, ok := errors.AsType[*csv.ParseError](errs[0])
	require.True(t, ok)
	assert.Equal(t, 2, parseErr.Line)
	assert.Equal(t, []map[string]string{{"id": "2", "quote": "fine"}}, rows, "a malformed row does not stop the iteration")

	boom := errors.New("connection reset")
	count := 0
	for _, err := range streaming.CSV(io.MultiReader(strings.NewReader("a,b\n"), iotest.ErrReader(boom))) {
		count++
		if count == 2 {
			require.ErrorIs(t, err, boom)
		}
	}
	assert.Equal(t, 2, count, "a read error ends the iteration")

	count = 0
	for range streaming.CSV(strings.NewReader("a\nb\nc\n")) {
		count++
		break
	}
	assert.Equal(t, 1, count)
	count = 0
	for range streaming.CSVDicts(strings.NewReader("h\na\nb\n")) {
		count++
		break
	}
	assert.Equal(t, 1, count)
	for _, err := range streaming.CSVDicts(strings.NewReader("h\n\"\n")) {
		require.Error(t, err)
		break
	}
}

func TestCSVRows(t *testing.T) {
	t.Parallel()
	var rows [][]string
	for row, err := range streaming.CSV(strings.NewReader("a, b\nc\n"), streaming.WithTrimLeadingSpace()) {
		require.NoError(t, err)
		rows = append(rows, row)
	}
	assert.Equal(t, [][]string{{"a", "b"}, {"c"}}, rows)
}

func TestLines(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200_000)
	input := "first\r\nsecond\n\n" + long + "\nlast"
	var lines []string
	for line, err := range streaming.Lines(strings.NewReader(input)) {
		require.NoError(t, err)
		lines = append(lines, string(line))
	}
	assert.Equal(t, []string{"first", "second", "", long, "last"}, lines)

	boom := errors.New("connection reset")
	var errs []error
	for _, err := range streaming.Lines(io.MultiReader(strings.NewReader("a\n"), iotest.ErrReader(boom))) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	require.Len(t, errs, 1)
	require.ErrorIs(t, errs[0], boom)

	count := 0
	for range streaming.Lines(strings.NewReader("a\nb\n")) {
		count++
		break
	}
	assert.Equal(t, 1, count)
}

func TestJSONValues(t *testing.T) {
	t.Parallel()
	type event struct {
		ID int `json:"id"`
	}
	var ids []int
	for v, err := range streaming.JSONValues[event](strings.NewReader("{\"id\":1}\n{\"id\":2} {\"id\":3}\n")) {
		require.NoError(t, err)
		ids = append(ids, v.ID)
	}
	assert.Equal(t, []int{1, 2, 3}, ids)

	var errs []error
	for _, err := range streaming.JSONValues[event](strings.NewReader(`{"id":1} {"id":`)) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	require.Len(t, errs, 1, "a malformed value ends the iteration")

	count := 0
	for range streaming.JSONValues[any](strings.NewReader("1 2 3")) {
		count++
		break
	}
	assert.Equal(t, 1, count)
}
