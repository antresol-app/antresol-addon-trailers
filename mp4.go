package main

import (
	"encoding/binary"
	"io"
	"os"
)

// looksMP4 says whether the file at p starts with an ISO BMFF "ftyp" box
// (an MP4 the apps play), not an error page or a half-written file.
func looksMP4(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 8)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	size := binary.BigEndian.Uint32(head[:4])
	return string(head[4:]) == "ftyp" && (size == 1 || size >= 8)
}

// mp4Duration is the length in ms that the MP4's movie header
// (moov → mvhd) gives; 0 when the file has none it can read.
func mp4Duration(p string) int64 {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	start, end, ok := box(f, 0, st.Size(), "moov")
	if !ok {
		return 0
	}
	start, end, ok = box(f, start, end, "mvhd")
	if !ok {
		return 0
	}
	// version (1 byte), flags (3), then the times: 32-bit in version 0,
	// 64-bit in version 1 (created, modified, timescale, duration)
	hdr := make([]byte, 32)
	n, _ := f.ReadAt(hdr, start)
	if n < 20 || start+int64(n) > end {
		return 0
	}
	var scale, length uint64
	if hdr[0] == 1 {
		if n < 32 {
			return 0
		}
		scale = uint64(binary.BigEndian.Uint32(hdr[20:24]))
		length = binary.BigEndian.Uint64(hdr[24:32])
	} else {
		scale = uint64(binary.BigEndian.Uint32(hdr[12:16]))
		length = uint64(binary.BigEndian.Uint32(hdr[16:20]))
	}
	if scale == 0 {
		return 0
	}
	return int64(length * 1000 / scale)
}

// box looks for the box kind among the boxes in [from, to) of f: the range
// of its contents.
func box(f *os.File, from, to int64, kind string) (int64, int64, bool) {
	hdr := make([]byte, 16)
	for at := from; at+8 <= to; {
		if _, err := f.ReadAt(hdr[:8], at); err != nil {
			return 0, 0, false
		}
		size, skip := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		if size == 1 { // a 64-bit size follows the type
			if _, err := f.ReadAt(hdr[8:16], at+8); err != nil {
				return 0, 0, false
			}
			size, skip = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		} else if size == 0 { // up to the end
			size = to - at
		}
		if size < skip || at+size > to {
			return 0, 0, false
		}
		if string(hdr[4:8]) == kind {
			return at + skip, at + size, true
		}
		at += size
	}
	return 0, 0, false
}
