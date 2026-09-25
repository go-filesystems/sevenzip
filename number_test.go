// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"bytes"
	"math"
	"testing"
)

// TestNumberRoundTripsAtEveryBoundary is where this encoding breaks or does not.
//
// A variable-length number is easy to get right for small values and wrong at
// the point the length changes -- and an archive whose header encodes fine until
// some size is worse than one that never works, because the failure waits for a
// big file. So the values tested are the boundaries themselves: the last value
// of each width and the first value of the next.
func TestNumberRoundTripsAtEveryBoundary(t *testing.T) {
	var values []uint64
	for width := 1; width <= 8; width++ {
		limit := uint64(1) << (7 * width)
		values = append(values, limit-1, limit)
	}
	values = append(values, 0, 1, 2, 127, 128, 255, 256, math.MaxUint64)

	for _, v := range values {
		var buf bytes.Buffer
		if err := writeNumber(&buf, v); err != nil {
			t.Errorf("writeNumber(%d): %v", v, err)
			continue
		}
		got, err := readNumber(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Errorf("readNumber after %d: %v (bytes %x)", v, err, buf.Bytes())
			continue
		}
		if got != v {
			t.Errorf("%d round-tripped to %d (bytes %x)", v, got, buf.Bytes())
		}
	}
}

// TestNumberWidthsAreWhatTheFormatSays pins the byte counts, not only the round
// trip. A pair of functions can agree with each other and disagree with the
// format -- the round trip above would pass just as well if both were wrong in
// the same way, which is why these lengths are asserted against the
// specification rather than against readNumber.
func TestNumberWidthsAreWhatTheFormatSays(t *testing.T) {
	for _, c := range []struct {
		v    uint64
		want int
	}{
		{0, 1},
		{0x7F, 1},   // the largest one-byte value
		{0x80, 2},   // one more needs a second byte
		{0x3FFF, 2}, // the largest two-byte value
		{0x4000, 3},
		{0x1FFFFF, 3},
		{0x200000, 4},
		{math.MaxUint64, 9}, // the first byte says "eight follow"
	} {
		var buf bytes.Buffer
		if err := writeNumber(&buf, c.v); err != nil {
			t.Fatalf("writeNumber(%#x): %v", c.v, err)
		}
		if got := buf.Len(); got != c.want {
			t.Errorf("%#x encoded in %d byte(s) (%x), want %d", c.v, got, buf.Bytes(), c.want)
		}
	}
}

// TestTheFirstByteMarksTheLength checks the one structural claim the encoding
// makes: the high bits of the first byte count the bytes that follow.
func TestTheFirstByteMarksTheLength(t *testing.T) {
	for _, c := range []struct {
		v     uint64
		first byte
	}{
		{0x00, 0x00},
		{0x7F, 0x7F}, // no high bit: nothing follows
		{0x80, 0x80}, // one high bit: one byte follows
		{0x4000, 0xC0},
		{0x200000, 0xE0},
	} {
		var buf bytes.Buffer
		if err := writeNumber(&buf, c.v); err != nil {
			t.Fatal(err)
		}
		if got := buf.Bytes()[0]; got&0xF0 != c.first&0xF0 {
			t.Errorf("%#x began with %#02x, want the length bits of %#02x", c.v, got, c.first)
		}
	}
}
