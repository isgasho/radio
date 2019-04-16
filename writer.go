package radio

import (
	"io"
)

// NewWriter initializes a RESP writer to write to given io.Writer.
func NewWriter(wr io.Writer) *Writer {
	return &Writer{
		w: wr,
	}
}

// Writer provides functions for writing RESP protocol values.
type Writer struct {
	w io.Writer
}

func (rw *Writer) Write(v Value) (int, error) {
	return rw.w.Write([]byte(v.Serialize()))
}
