// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
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
	coder  *coder
	pos    int64 // bytes of packed data written so far
	cur    *entryWriter
	files  []fileRecord
	closed bool
}

// Option settles how a Writer stores what it is given.
type Option func(*config)

type config struct {
	method Method
	dict   int
}

// WithMethod chooses how entries are stored. The default is Store.
func WithMethod(m Method) Option { return func(c *config) { c.method = m } }

// WithDictionary asks for an LZMA2 dictionary of at least this many bytes.
//
// At least, because the format names sizes from a fixed sequence rather than
// carrying a number: the smallest one big enough is used, and it is that size
// the stream is encoded with. Asking for something the sequence cannot name and
// being given the next one up is the format's arithmetic, not a rounding this
// package chose.
func WithDictionary(bytes int) Option { return func(c *config) { c.dict = bytes } }

// fileRecord is what the header will have to say about one entry.
type fileRecord struct {
	name string
	// unpackSize is what the entry holds; packSize is what it takes on disk.
	// They are equal for Store, which is why a store-only writer cannot tell
	// whether it has put them in the right sections -- and they go in different
	// sections: PackInfo carries the packed sizes, CodersUnpackSize the others.
	unpackSize int64
	packSize   int64
	crc        uint32
	perm       fs.FileMode
	// dir and empty say which of the three kinds of entry this is. 7z stores no
	// stream for either, and tells them apart with two bit vectors: kEmptyStream
	// over every entry, then kEmptyFile over only the ones it marked. So a
	// directory is "no stream, and not an empty file" -- an absence described
	// twice, and a reader that sees only the first vector calls it an empty file.
	dir   bool
	empty bool
}

// hasStream is true for the entries a pack stream was written for.
func (f fileRecord) hasStream() bool { return !f.dir && !f.empty }

// POSIX file types, because the attribute word carries a POSIX mode and not an
// os.FileMode: os.ModeDir is 1<<31, so narrowing an os.FileMode to the sixteen
// bits this field has room for drops every type bit and leaves a directory
// indistinguishable from a file with the same permissions.
const (
	sIFDIR = 0o040000
	sIFREG = 0o100000
)

// Windows attribute bits, which is what the field is named after and holds
// first. 0x8000 says the high sixteen bits carry a POSIX mode -- an extension,
// and the one the reference implementation writes on Unix.
const (
	attrDirectory = 0x10
	attrArchive   = 0x20
	attrUnixMode  = 0x8000
)

// attributes is the word kWinAttributes carries for this entry.
func (f fileRecord) attributes() uint32 {
	mode := uint32(f.perm.Perm())
	win := uint32(attrArchive)
	if f.dir {
		win, mode = attrDirectory, mode|sIFDIR
	} else {
		mode |= sIFREG
	}
	return win | attrUnixMode | mode<<16
}

// withStreams are the entries that have one.
//
// PackInfo, UnpackInfo and SubStreamsInfo must count only these, while FilesInfo
// names them all. Counting every entry everywhere describes streams that were
// never written, and a reader then goes looking for them.
func (z *Writer) withStreams() []fileRecord {
	out := make([]fileRecord, 0, len(z.files))
	for _, f := range z.files {
		if f.hasStream() {
			out = append(out, f)
		}
	}
	return out
}

// NewWriter reserves the signature header and returns a Writer ready for its
// first entry.
func NewWriter(w io.WriteSeeker, opts ...Option) (*Writer, error) {
	cfg := config{method: Store, dict: 1 << 22}
	for _, o := range opts {
		o(&cfg)
	}
	c, err := newCoder(cfg.method, cfg.dict)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(make([]byte, signatureLen)); err != nil {
		return nil, err
	}
	return &Writer{w: w, coder: c}, nil
}

// Create starts an entry and returns the writer its contents go to.
//
// The name is the path inside the archive, with '/' separators. Writing to the
// returned writer is what puts bytes in the archive; the entry ends when the
// next Create or Close is called.
func (z *Writer) Create(name string) (io.Writer, error) {
	return z.create(name, 0o644)
}

// AddDir records a directory. No stream is written for it, and its mode is kept
// in the attribute word.
func (z *Writer) AddDir(name string, perm fs.FileMode) error {
	if z.closed {
		return ErrClosed
	}
	if err := z.finishEntry(); err != nil {
		return err
	}
	if name == "" {
		return errors.New("sevenzip: a directory with no name")
	}
	z.files = append(z.files, fileRecord{name: name, perm: perm, dir: true})
	return nil
}

// AddFile records a file and copies its contents in.
//
// AddDir and AddFile together are the shape a deferred write layer seals
// through, so that the thing rewriting an archive at the end does not have to
// know which format it is rewriting.
func (z *Writer) AddFile(name string, perm fs.FileMode, src io.Reader) error {
	w, err := z.create(name, perm)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

func (z *Writer) create(name string, perm fs.FileMode) (io.Writer, error) {
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
	counted := &countingWriter{w: z.w}
	sink, finish, err := z.coder.wrap(counted)
	if err != nil {
		return nil, err
	}
	z.cur = &entryWriter{z: z, name: name, perm: perm, crc: crc32.NewIEEE(),
		sink: sink, counted: counted, finish: finish}
	return z.cur, nil
}

// finishEntry records what the open entry turned out to be.
func (z *Writer) finishEntry() error {
	e := z.cur
	z.cur = nil
	if e == nil {
		return nil
	}
	// The compressor's last chunk is written by its Close, so the packed size is
	// not knowable before it: reading the counter first reports an entry
	// shorter than it is, and the archive then points past its own data.
	if err := e.finish(); err != nil {
		return err
	}
	z.pos += e.counted.n
	z.files = append(z.files, fileRecord{
		name:       e.name,
		perm:       e.perm,
		empty:      e.n == 0,
		unpackSize: e.n,
		packSize:   e.counted.n,
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
	z       *Writer
	name    string
	perm    fs.FileMode
	n       int64 // bytes handed IN, which is the unpacked size
	sink    io.Writer
	counted *countingWriter
	finish  func() error
	crc     interface {
		io.Writer
		Sum32() uint32
	}
}

func (e *entryWriter) Write(p []byte) (int, error) {
	if e.z.closed {
		return 0, ErrClosed
	}
	// The checksum is of what came IN. It is the uncompressed data every reader
	// checks, so taking it after the coder would check the wrong bytes.
	n, err := e.sink.Write(p)
	if n > 0 {
		e.n += int64(n)
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

	if len(z.withStreams()) > 0 {
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
	streams := z.withStreams()
	b.WriteByte(idPackInfo)
	_ = writeNumber(b, 0) // packPos: the first stream begins right after the signature
	_ = writeNumber(b, uint64(len(streams)))
	b.WriteByte(idSize)
	for _, f := range streams {
		_ = writeNumber(b, uint64(f.packSize))
	}
	b.WriteByte(idEnd)
}

// writeUnpackInfo describes one folder per entry, each holding a single Copy
// coder, and what each folder checks out at.
func (z *Writer) writeUnpackInfo(b *bytes.Buffer) {
	streams := z.withStreams()
	b.WriteByte(idUnpackInfo)

	b.WriteByte(idFolder)
	_ = writeNumber(b, uint64(len(streams)))
	b.WriteByte(0) // external: the folder definitions are right here
	for range streams {
		_ = writeNumber(b, 1) // one coder
		b.WriteByte(z.coder.flags())
		b.Write(z.coder.id)
		if len(z.coder.props) > 0 {
			_ = writeNumber(b, uint64(len(z.coder.props)))
			b.Write(z.coder.props)
		}
	}

	b.WriteByte(idCodersUnpackSize)
	for _, f := range streams {
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
	for _, f := range z.withStreams() {
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
	z.writeEmptyVectors(b)

	b.WriteByte(idName)
	_ = writeNumber(b, uint64(names.Len()))
	b.Write(names.Bytes())

	var attrs bytes.Buffer
	attrs.WriteByte(1) // every attribute is defined
	attrs.WriteByte(0) // external: they are right here
	for _, f := range z.files {
		var a [4]byte
		binary.LittleEndian.PutUint32(a[:], f.attributes())
		attrs.Write(a[:])
	}
	b.WriteByte(idWinAttributes)
	_ = writeNumber(b, uint64(attrs.Len()))
	b.Write(attrs.Bytes())

	b.WriteByte(idEnd)
}

// writeEmptyVectors marks the entries that carry no stream, and then which of
// those are files rather than directories.
//
// The second vector is over the MARKED entries only, not over every entry. Its
// length therefore depends on the first vector's contents, which is why the two
// are written together here rather than wherever each tag belongs.
//
// Both are omitted when nothing is marked: kEmptyFile absent means every
// stream-less entry is a directory, so an archive of ordinary files writes
// neither and reads back exactly as it did before this existed.
func (z *Writer) writeEmptyVectors(b *bytes.Buffer) {
	var noStream, isFile []bool
	for _, f := range z.files {
		noStream = append(noStream, !f.hasStream())
		if !f.hasStream() {
			isFile = append(isFile, !f.dir)
		}
	}
	if !anySet(noStream) {
		return
	}
	writeTaggedVector(b, idEmptyStream, noStream)
	if anySet(isFile) {
		writeTaggedVector(b, idEmptyFile, isFile)
	}
}

func anySet(bits []bool) bool {
	for _, v := range bits {
		if v {
			return true
		}
	}
	return false
}

// writeTaggedVector writes a property whose payload is one bit per value.
//
// The bits go MOST significant first within each byte, and the last byte is
// padded with zeros. Filling from the low bit instead produces a vector of the
// right LENGTH holding the wrong entries, which a reader accepts and then
// reports a directory where a file was.
func writeTaggedVector(b *bytes.Buffer, id byte, bits []bool) {
	var v bytes.Buffer
	var cur byte
	var n int
	for _, set := range bits {
		if set {
			cur |= 0x80 >> n
		}
		if n++; n == 8 {
			v.WriteByte(cur)
			cur, n = 0, 0
		}
	}
	if n > 0 {
		v.WriteByte(cur)
	}
	b.WriteByte(id)
	_ = writeNumber(b, uint64(v.Len()))
	b.Write(v.Bytes())
}
