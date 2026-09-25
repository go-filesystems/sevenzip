// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"errors"
	"io"

	"github.com/ulikunitz/xz/lzma"
)

// Method is how an entry's bytes are stored.
type Method int

const (
	// Store keeps the bytes as they are. Every reader takes it, it costs
	// nothing to write, and an entry can be read without decoding anything.
	Store Method = iota
	// LZMA2 compresses. It is what 7-Zip writes by default and what the name
	// of the format means to most people.
	LZMA2
)

// coderIDs are the format's own identifiers, one byte each for these two.
var coderIDs = map[Method][]byte{
	Store: {0x00},
	LZMA2: {0x21},
}

// ErrUnknownMethod is returned for a Method this writer does not have.
var ErrUnknownMethod = errors.New("sevenzip: unknown method")

// dictProps encodes a dictionary size the way LZMA2's one property byte does.
//
// The byte is not the size: it is an index into a sequence that alternates
// between two and three times a power of two --
//
//	p even: 2 << (p/2 + 11)
//	p odd:  3 << (p/2 + 11)
//
// so the sizes go 4 KiB, 6 KiB, 8 KiB, 12 KiB, 16 KiB and so on, and 40 means
// the whole 4 GiB. A writer therefore cannot store an arbitrary size; it stores
// the smallest one of these that is big enough, and must then USE that size
// rather than the one it was asked for, or the property and the stream disagree.
func dictProps(want int) (props byte, actual int) {
	for p := 0; p < 40; p++ {
		size := (2 | (p & 1)) << (uint(p)/2 + 11)
		if size >= want {
			return byte(p), size
		}
	}
	return 40, 1 << 32 // the largest the encoding can name
}

// coder is one method's contribution to a folder: what the header says about it,
// and what it does to the bytes.
type coder struct {
	method Method
	id     []byte
	props  []byte
	dict   int
}

// newCoder settles the coder for a method and a wanted dictionary size, which
// is ignored by Store.
func newCoder(m Method, wantDict int) (*coder, error) {
	id, ok := coderIDs[m]
	if !ok {
		return nil, ErrUnknownMethod
	}
	c := &coder{method: m, id: id}
	if m == LZMA2 {
		p, actual := dictProps(wantDict)
		c.props = []byte{p}
		c.dict = actual
	}
	return c, nil
}

// wrap returns the writer an entry's bytes go through, and the function that
// finishes it. Store has nothing to finish; a compressor has to flush its last
// chunk before the packed size is known.
func (c *coder) wrap(w io.Writer) (io.Writer, func() error, error) {
	if c.method == Store {
		return w, func() error { return nil }, nil
	}
	z, err := lzma.Writer2Config{DictCap: c.dict}.NewWriter2(w)
	if err != nil {
		return nil, nil, err
	}
	return z, z.Close, nil
}

// flags is the coder's first header byte: the low four bits are the length of
// its id, and 0x20 says properties follow.
func (c *coder) flags() byte {
	f := byte(len(c.id)) & 0x0F
	if len(c.props) > 0 {
		f |= 0x20
	}
	return f
}

// countingWriter counts what passes through, which is how a packed size is
// learned: the compressor does not say.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
