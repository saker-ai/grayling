package sdk

import (
	"bufio"
	"io"
)

// newLineScanner returns a scanner tuned for SSE streams: line-oriented,
// with a larger token buffer so a big data payload cannot stall the stream.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return sc
}
