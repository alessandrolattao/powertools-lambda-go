package streaming_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/streaming"
)

// FuzzS3Object replays arbitrary sequences of reads, seeks and positional
// reads against an S3Object and a bytes.Reader over the same data: they must
// return the same bytes and land at the same positions.
func FuzzS3Object(f *testing.F) {
	f.Add([]byte("abcdefghijklmnopqrstuvwxyz"), []byte{0, 5, 1, 3, 2, 10, 3, 4, 0, 30})
	f.Add([]byte{}, []byte{0, 1, 3, 0})
	f.Add([]byte("x"), []byte{2, 200, 0, 1, 4, 0})
	f.Fuzz(func(t *testing.T, data, ops []byte) {
		fake := newMemS3()
		fake.put("key", data)
		obj := streaming.NewS3Object(t.Context(), fake, "bucket", "key")
		defer func() {
			if err := obj.Close(); err != nil {
				t.Fatal(err)
			}
		}()
		want := bytes.NewReader(data)

		for len(ops) >= 2 {
			op, arg := ops[0]%5, int64(ops[1])
			ops = ops[2:]
			switch op {
			case 0: // read
				compareReads(t, obj, want, int(arg))
			case 1: // seek from the start
				compareSeeks(t, obj, want, arg, io.SeekStart)
			case 2: // seek from the current position, backwards for odd args
				if arg%2 == 1 {
					arg = -arg
				}
				compareSeeks(t, obj, want, arg, io.SeekCurrent)
			case 3: // seek from the end
				compareSeeks(t, obj, want, -arg, io.SeekEnd)
			case 4: // positional read
				compareReadAts(t, obj, want, int(arg)%16, int64(len(data))/2)
			}
		}
	})
}

func compareReads(t *testing.T, obj *streaming.S3Object, want *bytes.Reader, size int) {
	t.Helper()
	got, wantBuf := make([]byte, size), make([]byte, size)
	gotN, gotErr := io.ReadFull(obj, got)
	wantN, wantErr := io.ReadFull(want, wantBuf)
	if gotN != wantN || !bytes.Equal(got[:gotN], wantBuf[:wantN]) || !errors.Is(gotErr, wantErr) {
		t.Fatalf("read %d: got %d %q %v, want %d %q %v", size, gotN, got[:gotN], gotErr, wantN, wantBuf[:wantN], wantErr)
	}
}

// compareReadAts checks a positional read. A read that fills its buffer may
// report io.EOF or not at the end of the data: io.ReaderAt allows both.
func compareReadAts(t *testing.T, obj *streaming.S3Object, want *bytes.Reader, size int, off int64) {
	t.Helper()
	got, wantBuf := make([]byte, size), make([]byte, size)
	gotN, gotErr := obj.ReadAt(got, off)
	wantN, wantErr := want.ReadAt(wantBuf, off)
	if gotN != wantN || !bytes.Equal(got[:gotN], wantBuf[:wantN]) || (gotN < size && !errors.Is(gotErr, wantErr)) {
		t.Fatalf("ReadAt %d at %d: got %d %q %v, want %d %q %v", size, off, gotN, got[:gotN], gotErr, wantN, wantBuf[:wantN], wantErr)
	}
}

func compareSeeks(t *testing.T, obj *streaming.S3Object, want *bytes.Reader, offset int64, whence int) {
	t.Helper()
	gotPos, gotErr := obj.Seek(offset, whence)
	wantPos, wantErr := want.Seek(offset, whence)
	if (gotErr == nil) != (wantErr == nil) || (gotErr == nil && gotPos != wantPos) {
		t.Fatalf("seek %d %d: got %d %v, want %d %v", offset, whence, gotPos, gotErr, wantPos, wantErr)
	}
}

// FuzzDecoders feeds arbitrary streams to the iterators: they yield values
// or errors and always end.
func FuzzDecoders(f *testing.F) {
	f.Add([]byte("id,name\n1,\"ann\"\n2,bob,extra\n"))
	f.Add([]byte("\xef\xbb\xbfa\tb\r\n"))
	f.Add([]byte("{\"a\":1}\n[1,2]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for range streaming.CSVDicts(bytes.NewReader(data), streaming.WithRestKey("rest"), streaming.WithLazyQuotes()) {
		}
		for range streaming.CSV(bytes.NewReader(data), streaming.WithDelimiter('\t'), streaming.WithComment('#')) {
		}
		total := 0
		for line, err := range streaming.Lines(bytes.NewReader(data)) {
			if err != nil {
				t.Fatal(err)
			}
			total += len(line)
		}
		if total > len(data) {
			t.Fatalf("lines hold %d bytes of %d", total, len(data))
		}
		for range streaming.JSONValues[any](bytes.NewReader(data)) {
		}
	})
}
