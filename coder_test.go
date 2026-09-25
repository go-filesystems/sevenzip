// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bodgit "github.com/bodgit/sevenzip"
)

// TestDictPropsNamesOnlyTheSizesTheFormatCanName.
//
// The property byte is an index into a sequence, not a size, so a writer cannot
// store what it was asked for -- it stores the smallest name that is big enough
// and must then ENCODE WITH THAT SIZE. Returning the wanted size instead of the
// named one is the bug this function's second return value exists to prevent:
// the property and the stream would disagree, and the archive would decode to
// rubbish somewhere past the first dictionary's worth of data.
func TestDictPropsNamesOnlyTheSizesTheFormatCanName(t *testing.T) {
	for _, c := range []struct {
		want   int
		props  byte
		actual int
	}{
		{1, 0, 4 << 10},             // anything tiny gets the smallest name
		{4 << 10, 0, 4 << 10},       // exactly the smallest
		{(4 << 10) + 1, 1, 6 << 10}, // one byte more needs the next name
		{6 << 10, 1, 6 << 10},
		{8 << 10, 2, 8 << 10},
		{12 << 10, 3, 12 << 10},
		// The index advances by TWO per doubling -- p=0 is 4 KiB, p=2 is 8 KiB --
		// so a megabyte is 16, not 18. This table said 18, and the decode-back
		// assertion below is what proved the TABLE wrong rather than the function:
		// it is there because a hand-written table of a computed sequence is the
		// likelier of the two to be mistaken.
		{1 << 20, 16, 1 << 20},
		{(1 << 20) + 1, 17, 3 << 19},
		{1 << 22, 20, 1 << 22}, // the default this package asks for
	} {
		props, actual := dictProps(c.want)
		if props != c.props || actual != c.actual {
			t.Errorf("dictProps(%d) = (%d, %d), want (%d, %d)",
				c.want, props, actual, c.props, c.actual)
		}
		// The property must decode back to the size that was returned.
		back := (2 | (int(props) & 1)) << (uint(props)/2 + 11)
		if props < 40 && back != actual {
			t.Errorf("property %d decodes to %d, but %d was reported", props, back, actual)
		}
	}
}

// compressible is data that must get smaller, so "did it actually compress?"
// can be asked. Random bytes would not answer it.
func compressible(n int) []byte {
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString("the same sentence over and over, which is what compression is for. ")
	}
	return b.Bytes()[:n]
}

// TestAnLZMA2ArchiveIsAcceptedAndIsSmaller puts the two judges on the
// compressed path, and adds the question they cannot answer on their own: did
// anything get compressed? A writer that quietly stored while announcing LZMA2
// would satisfy both of them.
func TestAnLZMA2ArchiveIsAcceptedAndIsSmaller(t *testing.T) {
	body := compressible(256 << 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "compressed.7z")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z, err := NewWriter(f, WithMethod(LZMA2), WithDictionary(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	w, err := z.Create("repeated.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() >= int64(len(body)) {
		t.Errorf("the archive is %d bytes for %d of highly repetitive data: "+
			"nothing was compressed", st.Size(), len(body))
	}
	t.Logf("%d bytes of data became a %d byte archive", len(body), st.Size())

	// Judge one: the reference decodes every stream and checks every CRC.
	for _, name := range []string{"7zz", "7z", "7za"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, "t", path).CombinedOutput()
		if err != nil {
			t.Fatalf("7-Zip rejected the LZMA2 archive: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "Everything is Ok") {
			t.Errorf("7-Zip did not say it was ok:\n%s", out)
		}
		break
	}

	// Judge two: the bytes come back, which a verdict does not show.
	r, err := bodgit.OpenReader(path)
	if err != nil {
		t.Fatalf("the Go reader refused it: %v", err)
	}
	defer r.Close()
	if len(r.File) != 1 {
		t.Fatalf("%d entries, want 1", len(r.File))
	}
	rc, err := r.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read back %d bytes, want %d, and they differ", len(got), len(body))
	}
}

// TestTheRefusalsAreNamed covers what the API promises when it says no. These
// are contracts rather than coverage: a caller that gets a nil error from
// Create after Close writes into nothing and finds out at Close, or later.
func TestTheRefusalsAreNamed(t *testing.T) {
	if _, err := newCoder(Method(99), 1<<20); !errors.Is(err, ErrUnknownMethod) {
		t.Errorf("an unknown method gave %v, want ErrUnknownMethod", err)
	}

	// A dictionary bigger than the encoding can name gets the largest name,
	// which is what 40 means. Nothing can ask for more.
	if props, actual := dictProps(1 << 33); props != 40 || actual != 1<<32 {
		t.Errorf("dictProps(8 GiB) = (%d, %d), want (40, 4 GiB)", props, actual)
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "a.7z"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := z.Create(""); err == nil {
		t.Error("an entry with no name was accepted")
	}
	w, err := z.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "x"); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close every door is shut, and says so.
	if err := z.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("a second Close gave %v, want ErrClosed", err)
	}
	if _, err := z.Create("b.txt"); !errors.Is(err, ErrClosed) {
		t.Errorf("Create after Close gave %v, want ErrClosed", err)
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, ErrClosed) {
		t.Errorf("writing to a finished entry gave %v, want ErrClosed", err)
	}
}
