package logging

// buffer holds the encoded records of one invocation, oldest first, within a
// size limit in bytes.
type buffer struct {
	max     int
	size    int
	lines   [][]byte
	evicted bool
}

func newBuffer(maxBytes int) *buffer {
	return &buffer{max: maxBytes}
}

// add keeps line, evicting the oldest records until it fits. A line larger
// than the whole buffer is refused.
func (b *buffer) add(line []byte) bool {
	if len(line) > b.max {
		return false
	}
	for b.size+len(line) > b.max && len(b.lines) > 0 {
		b.size -= len(b.lines[0])
		b.lines[0] = nil
		b.lines = b.lines[1:]
		b.evicted = true
	}
	b.lines = append(b.lines, line)
	b.size += len(line)
	return true
}

// drain empties the buffer, returning what it held and whether records had
// to be evicted to make room.
func (b *buffer) drain() (lines [][]byte, evicted bool) {
	lines, evicted = b.lines, b.evicted
	b.reset()
	return lines, evicted
}

func (b *buffer) reset() {
	b.lines, b.size, b.evicted = nil, 0, false
}
