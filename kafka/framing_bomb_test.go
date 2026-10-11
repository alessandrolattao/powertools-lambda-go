package kafka

import (
	"bytes"
	"compress/zlib"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func zlibGlue(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{glueHeaderVersion, GlueCompressionZlib})
	buf.Write(make([]byte, 16)) // schema version id
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write(make([]byte, size))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestParseGlueHeaderRefusesADecompressionBomb(t *testing.T) {
	t.Parallel()
	bomb := zlibGlue(t, MaxGlueInflatedSize+1)
	assert.Less(t, len(bomb), 1<<20, "a few hundred kilobytes on the wire")

	_, _, err := ParseGlueHeader(bomb)
	require.ErrorIs(t, err, ErrInvalidWireFormat)

	_, payload, err := ParseGlueHeader(zlibGlue(t, 1024))
	require.NoError(t, err)
	assert.Len(t, payload, 1024)
}
