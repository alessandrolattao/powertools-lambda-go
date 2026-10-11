package streaming

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"
)

// Transform turns a byte stream into another one, as the bytes are read:
// decompression, decryption, character set conversion. Powertools Python's
// BaseTransform; Gzip is the built-in one, and any function of this shape is
// a custom one.
//
// A transform that returns an io.Closer is closed with the pipeline.
type Transform func(r io.Reader) (io.Reader, error)

// Apply returns r through the transforms, applied in order: the first one
// reads r, the next one reads what the first one returns, and so on. Closing
// the result closes every layer that is an io.Closer, the outermost first,
// r included.
func Apply(r io.Reader, transforms ...Transform) (io.ReadCloser, error) {
	layers := []io.Reader{r}
	current := r
	for _, transform := range transforms {
		next, err := transform(current)
		if err != nil {
			_ = closeLayers(layers) //nolint:errcheck // the transform error is what the caller needs
			return nil, fmt.Errorf("streaming: transform: %w", err)
		}
		layers = append(layers, next)
		current = next
	}
	return &pipeline{Reader: current, layers: layers}, nil
}

// pipeline is a transformed stream that closes all of its layers.
type pipeline struct {
	io.Reader

	layers []io.Reader
}

func (p *pipeline) Close() error {
	if err := closeLayers(p.layers); err != nil {
		return fmt.Errorf("streaming: close: %w", err)
	}
	return nil
}

func closeLayers(layers []io.Reader) error {
	var errs []error
	for _, layer := range slices.Backward(layers) {
		if closer, ok := layer.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

// Gzip decompresses a gzip stream, member after member: Powertools Python's
// GzipTransform. An empty stream reads as empty, as Python's GzipFile does.
func Gzip() Transform {
	return func(r io.Reader) (io.Reader, error) {
		zr, err := gzip.NewReader(r)
		if errors.Is(err, io.EOF) {
			return emptyBody{}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return zr, nil
	}
}
