// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestToIntRefusesWhatWouldWrap is the checked conversion every size crosses
// before it becomes an int. It must say no rather than wrap, on every platform.
func TestToIntRefusesWhatWouldWrap(t *testing.T) {
	if v, err := toInt(0); err != nil || v != 0 {
		t.Errorf("toInt(0) = (%d, %v)", v, err)
	}
	if v, err := toInt(math.MaxInt); err != nil || v != math.MaxInt {
		t.Errorf("toInt(MaxInt) = (%d, %v)", v, err)
	}
	if _, err := toInt(uint64(math.MaxInt) + 1); !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("toInt(MaxInt+1) gave %v, want ErrDictionaryTooLarge", err)
	}
	if _, err := toInt(math.MaxUint64); !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("toInt(MaxUint64) gave %v, want ErrDictionaryTooLarge", err)
	}
}

// TestDictPropsAtThePlatformEdge asks for the biggest dictionary an int can
// hold. On 64-bit that is the 4 GiB name; on 32-bit the sizes from 2 GiB up do
// not fit, and the answer must be the error, not a negative size.
func TestDictPropsAtThePlatformEdge(t *testing.T) {
	props, actual, err := dictProps(math.MaxInt)
	if strconv.IntSize == 32 {
		if !errors.Is(err, ErrDictionaryTooLarge) {
			t.Fatalf("dictProps(MaxInt) = (%d, %d, %v), want ErrDictionaryTooLarge", props, actual, err)
		}
		if _, err := newCoder(LZMA2, math.MaxInt); !errors.Is(err, ErrDictionaryTooLarge) {
			t.Errorf("newCoder gave %v, want ErrDictionaryTooLarge", err)
		}
		return
	}
	if err != nil || props != 40 || uint64(actual) != 1<<32 {
		t.Fatalf("dictProps(MaxInt) = (%d, %d, %v), want (40, 4 GiB, nil)", props, actual, err)
	}
}

// TestThe32BitRefusalOnAnyPlatform pretends int is 32 bits, so the refusal is
// held by the suite on a 64-bit machine as well as by the linux/386 job.
func TestThe32BitRefusalOnAnyPlatform(t *testing.T) {
	old := intMax
	intMax = math.MaxInt32
	defer func() { intMax = old }()

	// 1 GiB + 1 needs the 1.5 GiB name, which fits; 2 GiB - 1 needs the 2 GiB
	// name, which does not.
	if _, actual, err := dictProps(1<<30 + 1); err != nil || actual != 3<<29 {
		t.Errorf("dictProps(1 GiB+1) = (%d, %v), want 1.5 GiB", actual, err)
	}
	if _, _, err := dictProps(math.MaxInt32); !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("dictProps(MaxInt32) gave %v, want ErrDictionaryTooLarge", err)
	}
	if _, err := newCoder(LZMA2, math.MaxInt32); !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("newCoder gave %v, want ErrDictionaryTooLarge", err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "a.7z"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := NewWriter(f, WithMethod(LZMA2), WithDictionary(math.MaxInt32)); !errors.Is(err, ErrDictionaryTooLarge) {
		t.Errorf("NewWriter gave %v, want ErrDictionaryTooLarge", err)
	}
	// Store ignores the dictionary, so it never refuses.
	if _, err := newCoder(Store, math.MaxInt32); err != nil {
		t.Errorf("Store refused a dictionary it ignores: %v", err)
	}
}

// TestDictSizeIsNeverNegative walks every property the encoding names and
// checks the size is positive and strictly increasing in uint64, the property
// a wrapped int would lose on a 32-bit platform.
func TestDictSizeIsNeverNegative(t *testing.T) {
	var prev uint64
	for p := 0; p < 40; p++ {
		got, size := dictSize(int64(prev) + 1)
		if int(got) != p || size <= prev {
			t.Fatalf("p=%d: dictSize gave (%d, %d) after %d", p, got, size, prev)
		}
		prev = size
	}
	if p, size := dictSize(-5); p != 0 || size != 4<<10 {
		t.Errorf("dictSize(-5) = (%d, %d), want the smallest name", p, size)
	}
}
