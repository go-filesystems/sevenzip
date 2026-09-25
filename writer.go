// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"unicode/utf16"
)

// ErrClosed is returned by a Writer that has already been closed.
var ErrClosed = errors.New("sevenzip: writer is closed")

// ErrEntryOpen is returned when a second entry is started before the first is
// finished. Entries are written one after another because their bytes are
// written one after another: there is nowhere to put a second stream while the
// first is still arriving.
var ErrEntryOpen = errors.New("sevenzip: the previous entry is still open")

// signatureLen is the fixed header every archive begins with: the six magic
// bytes, two version bytes, a CRC, and the twenty-byte start header.
const signatureLen = 32

// Writer builds a 7z archive.
//
// It needs an io.WriteSeeker, and that is the format's doing rather than a
// convenience: the archive begins with a header that carries the POSITION, size
// and CRC of the header at the end, none of which is known until the end. The
// alternative is holding every byte in memory to count them first, which for
// the files people put in archives is not an alternative. So the first
// thirty-two bytes are reserved, the data is streamed straight through, and the
// beginning is filled in last.
//
// # What this writes
//
// Stored entries, one folder each, and an uncompressed header. That is a
// complete and valid archive -- every reader takes it -- and it is deliberately
// the smallest thing that can be verified against the reference implementation
// before compression is added on top. A folder per entry also means an entry can
// be read without decoding any other, which is what a solid archive gives up.
type Writer struct {
	w      io.WriteSeeker
	pos    int64 // bytes of packed data written so far
	cur    *entryWriter
	files  []fileRecord
	closed bool
}

// fileRecord is what the header will have to say about one entry.
type fileRecord struct {
	name       string
	unpackSize int64
	crc        uint32
}

// NewWriter reserves the signature header and returns a Writer ready for its
// first entry.
func NewWriter(w io.WriteSeeker) (*Writer, error) {
	if _, err := w.Write(make([]byte, signatureLen)); err != nil {
		return nil, err
	}
	return &Writer{w: w}, nil
}

// Create starts an entry and returns the writer its contents go to.
//
// The name is the path inside the archive, with '/' separators. Writing to the
// returned writer is what puts bytes in the archive; the entry ends when the
// next Create or Close is called.
func (z *Writer) Create(name string) (io.Writer, error) {
	if z.closed {
		return nil, ErrClosed
	}
	if z.cur != nil {
		if err := z.finishEntry(); err != nil {
			return nil, err
		}
	}
	if name == "" {
		return nil, errors.New("sevenzip: an entry with no name")
	}
	z.cur = &entryWriter{z: z, name: name, crc: crc32.NewIEEE()}
	return z.cur, nil
}

// finishEntry records what the open entry turned out to be.
func (z *Writer) finishEntry() error {
	e := z.cur
	z.cur = nil
	if e == nil {
		return nil
	}
	z.files = append(z.files, fileRecord{
		name:       e.name,
		unpackSize: e.n,
		crc:        e.crc.Sum32(),
	})
	return nil
}

// Close writes the header and the signature header, in that order, and the
// archive is complete when it returns.
func (z *Writer) Close() error {
	if z.closed {
		return ErrClosed
	}
	if err := z.finishEntry(); err != nil {
		return err
	}
	z.closed = true

	// An archive holding nothing has NO header: the reference writes exactly
	// thirty-two bytes, with the offset, the size and the CRC all zero. Asked
	// for one anyway, this writer emitted a FilesInfo saying "no files", which
	// is a different thing and which the Go reader refused -- "uint64 value must
	// be non-zero". The empty case is not the general case with zero in it.
	var header []byte
	if len(z.files) > 0 {
		header = z.header()
	}
	headerOffset := z.pos // relative to the end of the signature header
	if len(header) > 0 {
		if _, err := z.w.Write(header); err != nil {
			return err
		}
	} else {
		headerOffset = 0
	}
	// Now the beginning can be filled in, which is what the seek is for.
	if _, err := z.w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var headerCRC uint32
	if len(header) > 0 {
		headerCRC = crc32.ChecksumIEEE(header)
	}
	return writeSignature(z.w, headerOffset, int64(len(header)), headerCRC)
}

// writeSignature writes the thirty-two bytes at offset 0.
//
// These fields are FIXED-WIDTH little-endian, not the variable-length numbers
// the header is made of. Mixing the two encodings is the other way this header
// goes wrong, and it goes wrong silently: a reader finds a plausible offset and
// reads nonsense from it.
func writeSignature(w io.Writer, headerOffset, headerSize int64, headerCRC uint32) error {
	var b [signatureLen]byte
	copy(b[0:6], signature[:])
	b[6], b[7] = versionMajor, versionMinor
	start := b[12:32]
	binary.LittleEndian.PutUint64(start[0:8], uint64(headerOffset))
	binary.LittleEndian.PutUint64(start[8:16], uint64(headerSize))
	binary.LittleEndian.PutUint32(start[16:20], headerCRC)
	// The CRC at bytes 8..12 covers the start header, and only it.
	binary.LittleEndian.PutUint32(b[8:12], crc32.ChecksumIEEE(start))
	_, err := w.Write(b[:])
	return err
}

// entryWriter is one open entry. It counts and checksums what goes through it,
// because the header has to say both and neither is knowable afterwards.
type entryWriter struct {
	z    *Writer
	name string
	n    int64
	crc  interface {
		io.Writer
		Sum32() uint32
	}
}

func (e *entryWriter) Write(p []byte) (int, error) {
	if e.z.closed {
		return 0, ErrClosed
	}
	n, err := e.z.w.Write(p)
	if n > 0 {
		e.n += int64(n)
		e.z.pos += int64(n)
		_, _ = e.crc.Write(p[:n])
	}
	return n, err
}

// header builds the structured header describing every entry.
//
// The shape is fixed by the format: streams first (where the packed bytes are,
// how the folders decode them, what they check out at), then the files (their
// names). Sizes are the variable-length numbers; CRCs are fixed-width.
func (z *Writer) header() []byte {
	var b bytes.Buffer
	b.WriteByte(idHeader)

	if len(z.files) > 0 {
		b.WriteByte(idMainStreamsInfo)
		z.writePackInfo(&b)
		z.writeUnpackInfo(&b)
		z.writeSubStreamsInfo(&b)
		b.WriteByte(idEnd) // end of MainStreamsInfo
	}
	z.writeFilesInfo(&b)

	b.WriteByte(idEnd) // end of Header
	return b.Bytes()
}

// writePackInfo says where the packed streams are and how big each one is. For
// stored entries the packed size IS the size.
func (z *Writer) writePackInfo(b *bytes.Buffer) {
	b.WriteByte(idPackInfo)
	_ = writeNumber(b, 0) // packPos: the first stream begins right after the signature
	_ = writeNumber(b, uint64(len(z.files)))
	b.WriteByte(idSize)
	for _, f := range z.files {
		_ = writeNumber(b, uint64(f.unpackSize))
	}
	b.WriteByte(idEnd)
}

// writeUnpackInfo describes one folder per entry, each holding a single Copy
// coder, and what each folder checks out at.
func (z *Writer) writeUnpackInfo(b *bytes.Buffer) {
	b.WriteByte(idUnpackInfo)

	b.WriteByte(idFolder)
	_ = writeNumber(b, uint64(len(z.files)))
	b.WriteByte(0) // external: the folder definitions are right here
	for range z.files {
		_ = writeNumber(b, 1) // one coder
		// The coder's flags byte: the low four bits are the length of its id,
		// and the id of Copy is one byte of zero. No attributes, not complex.
		b.WriteByte(0x01)
		b.WriteByte(0x00) // coder id: Copy
	}

	b.WriteByte(idCodersUnpackSize)
	for _, f := range z.files {
		_ = writeNumber(b, uint64(f.unpackSize))
	}

	b.WriteByte(idEnd)
}

// writeSubStreamsInfo carries the checksums.
//
// They could be read as belonging to the folders, and this writer put them in
// UnpackInfo at first for that reason. The reference implementation puts them
// HERE, and the two readers disagreed about the result in the most useful
// possible way: 7-Zip said "Everything is Ok" while the pure-Go reader could not
// find the second entry at all. A format whose specification is its own source
// is settled by what that source WRITES, so this now matches it.
//
// With one stream per folder there is no kNumUnpackStream and no kSize: the
// counts and sizes are already in UnpackInfo, and repeating them is how two
// numbers come to disagree.
func (z *Writer) writeSubStreamsInfo(b *bytes.Buffer) {
	b.WriteByte(idSubStreamsInfo)
	b.WriteByte(idCRC)
	b.WriteByte(1) // every digest is defined
	for _, f := range z.files {
		var crc [4]byte
		binary.LittleEndian.PutUint32(crc[:], f.crc)
		b.Write(crc[:])
	}
	// ONE terminator, closing SubStreamsInfo. kCRC is a leaf -- tag, a flag,
	// the digests -- and has no end of its own, so a second byte here is one
	// too many and both readers say so: 7-Zip exits 2, and the Go reader calls
	// it "too much data".
	b.WriteByte(idEnd)
}

// writeFilesInfo names the entries. Names are UTF-16LE and NUL-terminated,
// which is the one place this format shows its origins.
func (z *Writer) writeFilesInfo(b *bytes.Buffer) {
	b.WriteByte(idFilesInfo)
	_ = writeNumber(b, uint64(len(z.files)))

	var names bytes.Buffer
	names.WriteByte(0) // external: the names are right here
	for _, f := range z.files {
		for _, u := range utf16.Encode([]rune(f.name)) {
			var pair [2]byte
			binary.LittleEndian.PutUint16(pair[:], u)
			names.Write(pair[:])
		}
		names.Write([]byte{0, 0}) // the terminator is two bytes, like the units
	}
	b.WriteByte(idName)
	_ = writeNumber(b, uint64(names.Len()))
	b.Write(names.Bytes())

	b.WriteByte(idEnd)
}
