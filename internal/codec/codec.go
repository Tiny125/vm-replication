// Package codec wraps the optional per-block compression. DEFLATE (stdlib
// compress/flate) keeps the MVP dependency-free; it can be swapped for LZ4/zstd
// later for a better speed/ratio trade-off on the hot path.
package codec

import (
	"bytes"
	"compress/flate"
	"io"
	"sync"
)

var writerPool = sync.Pool{
	New: func() any {
		w, _ := flate.NewWriter(io.Discard, flate.BestSpeed)
		return w
	},
}

// Compress returns the DEFLATE-compressed form of src.
func Compress(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := writerPool.Get().(*flate.Writer)
	defer writerPool.Put(w)
	w.Reset(&buf)
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decompress inflates src, expecting exactly rawLen bytes out.
func Decompress(src []byte, rawLen int) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(src))
	defer r.Close()
	out := make([]byte, rawLen)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, err
	}
	return out, nil
}
