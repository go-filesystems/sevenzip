<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-sevenzip.png" alt="go-filesystems/sevenzip" width="720"></p>

# sevenzip

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/sevenzip.svg)](https://pkg.go.dev/github.com/go-filesystems/sevenzip)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/sevenzip/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/sevenzip/actions/workflows/ci.yml)

**Writes** 7z archives — pure Go, `CGO_ENABLED=0`, builds for every target Go
builds for.

```go
z, err := sevenzip.NewWriter(f)              // f is an io.WriteSeeker
z.AddDir("docs", 0o755)
z.AddFile("docs/notes.txt", 0o644, size, r)
z.Close()                                     // the header is written here
```

## Writing, not reading

This package **writes**. For reading, `github.com/bodgit/sevenzip` already does
it well and [`go-filesystems/unarchive`](https://github.com/go-filesystems/unarchive)
uses it. What did not exist was a pure-Go writer, and `overlay.Builder` needed one:

```go
var _ overlay.Builder = (*sevenzip.Writer)(nil)
```

`AddDir` and `AddFile` are that interface, so an
[`overlay`](https://github.com/go-filesystems/overlay) over any read-only
filesystem can be sealed into a `.7z`.

## It needs an `io.WriteSeeker`, and that is the format

The archive begins with a header carrying the **position, size and CRC of the
header at the end** — none of which is known until the end. The alternative is
holding every byte in memory to count them first, which for the files people put
in archives is not an alternative. So the first thirty-two bytes are reserved and
written last.

## Two methods

| | |
|---|---|
| `Store` | keeps the bytes as they are. Every reader takes it, it costs nothing to write, and an entry can be read without decoding anything |
| `LZMA2` | what 7-Zip writes by default, and what the name of the format means to most people |

`WithMethod` and `WithDictionary` choose; `Store` is the default.

## ⛔ An empty entry must not write a stream

An entry with no bytes used to get an LZMA2 stream anyway — and an LZMA2 stream
with nothing in it is still **one byte**: its end marker. That orphan byte sat in
the packed data that no entry claimed, so every stream after it was read one byte
out, and the archive was corrupt from the first empty file onward.

`Store` hid it completely: storing nothing writes nothing, so the whole test suite
passed on the default method. The witness that was missing was **the same tests run
over both methods**, and it is what the suite does now.

The fix is that the coder is created by the first `Write`, not when the entry is
opened — so an entry nobody writes to has no stream at all.

## Directories and empty files are two bit vectors, not one

7z records them with `kEmptyStream` over **every** entry and `kEmptyFile` over
**only the ones that vector marked** — MSB-first, padded to a byte. A reader that
treats the second as parallel to the first sees directories where files are.

The POSIX mode travels in `kWinAttributes`, in the **high sixteen bits** behind
flag `0x8000`. `os.ModeDir` is `1 << 31`, so narrowing an `fs.FileMode` into
sixteen bits drops every type bit: the constants written are `S_IFDIR 0o040000`,
`S_IFREG 0o100000` and `S_IFLNK 0o120000`.

## Judged by 7-Zip

Every archive the tests write is handed to `7zz t` and `7zz l -slt`, and the
entries are extracted and compared **on disk**. Exit status is never the
assertion on its own.

## Licence

BSD-3-Clause.
