// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package sevenzip

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bodgit/sevenzip"
)

// write builds an archive with these entries and returns its path.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "made-here.7z")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	z, err := NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := z.Create(n)
		if err != nil {
			t.Fatalf("Create(%q): %v", n, err)
		}
		if _, err := io.WriteString(w, files[n]); err != nil {
			t.Fatalf("writing %q: %v", n, err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTheReferenceImplementationAcceptsWhatWeWrote is the only judge that
// settles a format whose specification is its own source.
//
// `7zz t` does not merely parse: it decodes every stream and checks every CRC,
// so an archive it passes is one whose header, offsets, sizes and checksums all
// agree. A test that only read the archive back with our own reader would prove
// that two halves of this repository agree with each other, which is not the
// question.
func TestTheReferenceImplementationAcceptsWhatWeWrote(t *testing.T) {
	bin := ""
	for _, name := range []string{"7zz", "7z", "7za"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("no 7-Zip binary here to judge the archive with")
	}

	archive := write(t, map[string]string{
		"notes.txt":           "hello",
		"photos/one.jpg":      "aaaa",
		"photos/2024/two.jpg": "bbbbbb",
		"empty-ish.bin":       "x",
	})

	out, err := exec.Command(bin, "t", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("7-Zip rejected the archive: %v\n%s", err, out)
	}
	t.Logf("7-Zip verdict:\n%s", out)

	// And it lists what we put in, by name.
	list, err := exec.Command(bin, "l", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("7-Zip could not list it: %v\n%s", err, list)
	}
	for _, want := range []string{"notes.txt", "one.jpg", "two.jpg"} {
		if !containsString(string(list), want) {
			t.Errorf("the listing does not mention %q:\n%s", want, list)
		}
	}
}

// TestOurArchiveReadsBackThroughTheGoReader is the second judge, and it is the
// one that runs everywhere: pure Go, no binary needed. It checks the BYTES,
// which a "t" verdict does not show.
func TestOurArchiveReadsBackThroughTheGoReader(t *testing.T) {
	files := map[string]string{
		"notes.txt":           "hello",
		"photos/one.jpg":      "aaaa",
		"photos/2024/two.jpg": "bbbbbb",
	}
	archive := write(t, files)

	r, err := sevenzip.OpenReader(archive)
	if err != nil {
		t.Fatalf("the Go reader refused it: %v", err)
	}
	defer r.Close()

	got := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Errorf("%s: %v", f.Name, err)
			continue
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("%s: %v", f.Name, err)
			continue
		}
		got[f.Name] = string(b)
	}
	if len(got) != len(files) {
		t.Errorf("read back %d entries, want %d: %v", len(got), len(files), got)
	}
	for name, want := range files {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

// TestAnEmptyArchiveIsByteIdenticalToTheReference.
//
// The first version of this test asked the pure-Go reader to open an empty
// archive and called the refusal a defect. It is not ours: that reader refuses
// 7-Zip's OWN empty archive with the same error ("error reading header id:
// EOF"), so the expectation was wrong rather than the writer.
//
// What is worth asserting is stronger than "accepted" anyway. An empty 7z is
// thirty-two bytes with the offset, size and CRC all zero and no header at all,
// which leaves nothing to choose: our bytes and the reference's are the same
// bytes, and the test says so by comparing them.
func TestAnEmptyArchiveIsByteIdenticalToTheReference(t *testing.T) {
	archive := write(t, nil)
	got, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}

	// What the format says an empty archive is: signature, version, the CRC of
	// the start header, then twenty zero bytes.
	want := make([]byte, signatureLen)
	copy(want[0:6], signature[:])
	want[6], want[7] = versionMajor, versionMinor
	binary.LittleEndian.PutUint32(want[8:12], crc32.ChecksumIEEE(want[12:32]))

	if len(got) != signatureLen {
		t.Fatalf("an empty archive is %d bytes, want %d", len(got), signatureLen)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("an empty archive reads\n  %x\nwant\n  %x", got, want)
	}

	// And the same bytes 7-Zip writes, when it is here to be asked.
	bin := ""
	for _, name := range []string{"7zz", "7z", "7za"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("no 7-Zip binary to compare against")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nothing"), 0o755); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(dir, "ref.7z")
	cmd := exec.Command(bin, "a", "-bso0", "-bsp0", ref, ".")
	cmd.Dir = filepath.Join(dir, "nothing")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7-Zip would not write an empty archive here: %v\n%s", err, out)
	}
	refBytes, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, refBytes) {
		t.Errorf("ours\n  %x\ndiffers from the reference\n  %x", got, refBytes)
	}
}

func containsString(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// entry is one thing to put in an archive, with the kinds kept apart because
// telling them apart is what these tests are about.
type entry struct {
	name string
	perm os.FileMode
	dir  bool
	body string
}

// methods are the two ways an entry can be stored, and both judges run over
// both of them.
//
// The method was a constant in every earlier test, and that is how an archive
// holding an empty file came to be correct under Store and broken under LZMA2
// for every entry after it: Store writes nothing for an entry with no bytes,
// a compressor writes its end-of-stream marker. A fixture that fixes the method
// cannot see the difference between a rule and an accident of that method.
var methods = []struct {
	name   string
	method Method
}{
	{"Store", Store},
	{"LZMA2", LZMA2},
}

// writeTree builds an archive through AddDir and AddFile, in the order given.
func writeTree(t *testing.T, m Method, entries []entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tree.7z")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	z, err := NewWriter(f, WithMethod(m))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.dir {
			if err := z.AddDir(e.name, e.perm); err != nil {
				t.Fatalf("AddDir(%q): %v", e.name, err)
			}
			continue
		}
		if err := z.AddFile(e.name, e.perm, strings.NewReader(e.body)); err != nil {
			t.Fatalf("AddFile(%q): %v", e.name, err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// tree is the fixture, and its shape is the point.
//
// It holds all three kinds at once -- a directory, an empty FILE, and files with
// bytes -- because the second bit vector is over the stream-less entries only.
// With one such entry the vector is a single bit and its position cannot be
// wrong; with two of DIFFERENT kinds, a vector filled from the wrong end, or
// omitted, names the wrong one.
//
// The directory comes first and the empty file second so that the bits differ:
// were they the other way round the byte would be symmetric under the mistake.
func tree() []entry {
	return []entry{
		{name: "sub", perm: 0o755, dir: true},
		{name: "empty.txt", perm: 0o644},
		{name: "a.txt", perm: 0o644, body: "hello"},
		{name: "sub/b.txt", perm: 0o600, body: "x"},
	}
}

// TestTheReferenceExtractsADirectoryAsADirectory is the judge that cannot agree
// with us by accident.
//
// It does not ask 7-Zip to parse the header; it asks it to EXTRACT, and then
// looks at what appeared on the filesystem. A directory that arrives as an empty
// file, or an empty file that arrives as a directory, is then a stat away --
// which is the actual consequence of getting these two vectors wrong, and the
// one a "t" verdict cannot show, since both are valid archives.
func TestTheReferenceExtractsADirectoryAsADirectory(t *testing.T) {
	bin := ""
	for _, name := range []string{"7zz", "7z", "7za"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("no 7-Zip binary here to judge the archive with")
	}

	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			archive := writeTree(t, m.method, tree())
			into := filepath.Join(t.TempDir(), "out")

			out, err := exec.Command(bin, "x", "-bso0", "-bsp0", "-o"+into, archive).CombinedOutput()
			if err != nil {
				t.Fatalf("7-Zip could not extract it: %v\n%s", err, out)
			}

			for _, e := range tree() {
				fi, err := os.Lstat(filepath.Join(into, e.name))
				if err != nil {
					t.Errorf("%s did not arrive: %v", e.name, err)
					continue
				}
				if fi.IsDir() != e.dir {
					t.Errorf("%s: IsDir() = %v, want %v", e.name, fi.IsDir(), e.dir)
					continue
				}
				if e.dir {
					continue
				}
				b, err := os.ReadFile(filepath.Join(into, e.name))
				if err != nil {
					t.Errorf("%s: %v", e.name, err)
					continue
				}
				if string(b) != e.body {
					t.Errorf("%s = %q, want %q", e.name, b, e.body)
				}
				if got := fi.Mode().Perm(); got != e.perm {
					t.Errorf("%s: mode %v, want %v", e.name, got, e.perm)
				}
			}
		})
	}
}

// TestTheGoReaderTellsTheThreeKindsApart is the second judge, and it is the one
// that runs where no binary does.
func TestTheGoReaderTellsTheThreeKindsApart(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) { goReaderTellsThemApart(t, m.method) })
	}
}

func goReaderTellsThemApart(t *testing.T, m Method) {
	t.Helper()
	archive := writeTree(t, m, tree())

	r, err := sevenzip.OpenReader(archive)
	if err != nil {
		t.Fatalf("the Go reader refused it: %v", err)
	}
	defer r.Close()

	// The reader appends a slash to a directory's name -- its own convention, so
	// that the entries satisfy io/fs, and not something the archive carries. The
	// name is normalised here rather than expected, because asserting the slash
	// would pin that reader's convention as though it were the format's.
	seen := map[string]*sevenzip.File{}
	for _, f := range r.File {
		seen[strings.TrimSuffix(f.Name, "/")] = f
	}
	if len(seen) != len(tree()) {
		t.Fatalf("read back %d entries, want %d: %v", len(seen), len(tree()), keysOf(seen))
	}
	for _, e := range tree() {
		f, ok := seen[e.name]
		if !ok {
			t.Errorf("%s is missing from the archive", e.name)
			continue
		}
		fi := f.FileInfo()
		if fi.IsDir() != e.dir {
			t.Errorf("%s: IsDir() = %v, want %v", e.name, fi.IsDir(), e.dir)
			continue
		}
		if got := fi.Mode().Perm(); got != e.perm {
			t.Errorf("%s: mode %v, want %v", e.name, got, e.perm)
		}
		if e.dir {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Errorf("%s: %v", e.name, err)
			continue
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("%s: %v", e.name, err)
			continue
		}
		if string(b) != e.body {
			t.Errorf("%s = %q, want %q", e.name, b, e.body)
		}
	}
}

func keysOf(m map[string]*sevenzip.File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestWriteTaggedVectorFillsFromTheHighBit pins the bit order on its own,
// without a reader in the way.
//
// Both readers have another channel for the same fact -- the attribute word also
// says directory or file -- so a vector filled from the wrong end can be hidden
// by attributes that happen to be right. This asserts the bytes.
func TestWriteTaggedVectorFillsFromTheHighBit(t *testing.T) {
	for _, c := range []struct {
		bits []bool
		want []byte
	}{
		{[]bool{true}, []byte{0x80}},
		{[]bool{false, true}, []byte{0x40}},
		{[]bool{true, true, false, false}, []byte{0xC0}},
		{[]bool{false, false, false, false, false, false, false, true}, []byte{0x01}},
		// Nine bits, so the second byte is padded: the ninth is the high bit of
		// it and not the low bit of the first.
		{[]bool{false, false, false, false, false, false, false, false, true}, []byte{0x00, 0x80}},
	} {
		var b bytes.Buffer
		writeTaggedVector(&b, idEmptyStream, c.bits)
		got := b.Bytes()
		if len(got) < 2 || got[0] != idEmptyStream {
			t.Fatalf("%v: no tag in %x", c.bits, got)
		}
		if int(got[1]) != len(c.want) {
			t.Errorf("%v: size %d, want %d", c.bits, got[1], len(c.want))
			continue
		}
		if !bytes.Equal(got[2:], c.want) {
			t.Errorf("%v: payload %x, want %x", c.bits, got[2:], c.want)
		}
	}
}

// TestAnArchiveOfOrdinaryFilesWritesNeitherVector.
//
// Both vectors describe an absence, and an archive with nothing absent must not
// carry them: the reference omits them, and emitting a vector of all-zero bits
// would be a second way to say the same thing that readers have to agree about.
func TestAnArchiveOfOrdinaryFilesWritesNeitherVector(t *testing.T) {
	var b bytes.Buffer
	z := &Writer{files: []fileRecord{{name: "a", unpackSize: 1}}}
	z.writeEmptyVectors(&b)
	if b.Len() != 0 {
		t.Errorf("wrote %x, want nothing", b.Bytes())
	}
}

// TestEveryStreamlessEntryBeingADirectoryOmitsTheSecondVector.
//
// kEmptyFile absent means "all of them are directories", so an archive of
// nothing but directories writes only the first vector. Writing a second one
// full of zeros would say the same thing the long way.
func TestEveryStreamlessEntryBeingADirectoryOmitsTheSecondVector(t *testing.T) {
	var b bytes.Buffer
	z := &Writer{files: []fileRecord{{name: "a", dir: true}, {name: "b", dir: true}}}
	z.writeEmptyVectors(&b)
	want := []byte{idEmptyStream, 1, 0xC0}
	if !bytes.Equal(b.Bytes(), want) {
		t.Errorf("wrote %x, want %x", b.Bytes(), want)
	}
}

// TestBothVectorsReachTheHeader, and it runs where no 7-Zip binary does.
//
// The ablation that removes kEmptyFile altogether is caught by the reference's
// extraction and by NOTHING else: the pure-Go reader still reports the right
// kinds, because it falls back on the attribute word, which this writer also
// fills in correctly. Two channels carry the same fact, so a reader-level test
// cannot tell which one it read.
//
// This asserts the bytes instead. 0xC0 marks the first two of four entries as
// stream-less, and 0x40 says the second of THOSE two is a file -- so the
// directory is the first, by the absence of a bit.
func TestBothVectorsReachTheHeader(t *testing.T) {
	z := &Writer{coder: mustCoder(t), files: []fileRecord{
		{name: "sub", perm: 0o755, dir: true},
		{name: "empty.txt", perm: 0o644, empty: true},
		{name: "a.txt", perm: 0o644, unpackSize: 5, packSize: 5},
		{name: "sub/b.txt", perm: 0o600, unpackSize: 1, packSize: 1},
	}}
	h := z.header()
	want := []byte{idEmptyStream, 1, 0xC0, idEmptyFile, 1, 0x40}
	if !bytes.Contains(h, want) {
		t.Errorf("the header does not carry %x:\n%x", want, h)
	}
}

func mustCoder(t *testing.T) *coder {
	t.Helper()
	c, err := newCoder(Store, 1<<22)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
