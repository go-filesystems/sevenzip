// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package sevenzip reads and WRITES the 7z archive format.
//
// Reading goes through github.com/bodgit/sevenzip, which is pure Go and
// maintained: a second decoder would duplicate LZMA, LZMA2, bzip2, deflate,
// brotli, lz4, PPMd and the BCJ filters, all of which already work there.
//
// Writing is here because nothing pure-Go writes 7z. The compression half was
// already available -- github.com/ulikunitz/xz/lzma encodes LZMA2 -- so what
// this package adds is the container: the signature header, the packed streams,
// and the structured header that describes them.
package sevenzip

import (
	"io"
)

// 7z property identifiers, from the format's own documentation. They are the
// tags of the structured header.
const (
	idEnd               = 0x00
	idHeader            = 0x01
	idArchiveProperties = 0x02
	idAdditionalStreams = 0x03
	idMainStreamsInfo   = 0x04
	idFilesInfo         = 0x05
	idPackInfo          = 0x06
	idUnpackInfo        = 0x07
	idSubStreamsInfo    = 0x08
	idSize              = 0x09
	idCRC               = 0x0A
	idFolder            = 0x0B
	idCodersUnpackSize  = 0x0C
	idNumUnpackStream   = 0x0D
	idEmptyStream       = 0x0E
	idEmptyFile         = 0x0F
	idAnti              = 0x10
	idName              = 0x11
	idCTime             = 0x12
	idATime             = 0x13
	idMTime             = 0x14
	idWinAttributes     = 0x15
	idComment           = 0x16
	idEncodedHeader     = 0x17
	idStartPos          = 0x18
	idDummy             = 0x19
)

// signature is the six bytes every 7z archive begins with.
var signature = [6]byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}

// The version this writer stamps. 0.4 is what 7-Zip has written for twenty
// years and what every reader expects.
const (
	versionMajor = 0
	versionMinor = 4
)

// writeNumber writes 7z's variable-length number.
//
// The encoding is unusual and worth spelling out, because getting it subtly
// wrong produces an archive that reads correctly for small values and falls
// apart at some size nobody tests: the high bits of the FIRST byte say how many
// bytes follow, and the value's low bytes are those following bytes,
// little-endian, while whatever is left over rides in the first byte's low bits.
//
//	0xxxxxxx                     one byte, 7 bits
//	10xxxxxx + 1 byte            14 bits
//	110xxxxx + 2 bytes           21 bits
//	...
//	11111111 + 8 bytes           64 bits
func writeNumber(w io.Writer, v uint64) error {
	var buf [9]byte
	first := byte(0)
	mask := byte(0x80)
	extra := 0
	for ; extra < 8; extra++ {
		// Does the value fit with this many following bytes? The following
		// bytes carry 8 bits each, and the first byte's remaining low bits
		// carry 7-extra more.
		if v < (uint64(1) << (7 * (extra + 1))) {
			first |= byte(v >> (8 * extra))
			break
		}
		first |= mask
		mask >>= 1
	}
	buf[0] = first
	for j := 0; j < extra; j++ {
		buf[1+j] = byte(v >> (8 * j))
	}
	_, err := w.Write(buf[:1+extra])
	return err
}

// readNumber is writeNumber's inverse, here so the encoding can be proven
// against itself rather than only against another implementation.
func readNumber(r io.ByteReader) (uint64, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	mask := byte(0x80)
	var v uint64
	for i := 0; i < 8; i++ {
		if first&mask == 0 {
			high := uint64(first) & (uint64(mask) - 1)
			return v | (high << (8 * i)), nil
		}
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(b) << (8 * i)
		mask >>= 1
	}
	return v, nil
}
