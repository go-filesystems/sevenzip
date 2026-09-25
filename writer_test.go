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
