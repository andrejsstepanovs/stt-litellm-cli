package main

import (
	"bytes"
	"encoding/binary"
)

// Tray icon colors (0xRRGGBB).
const (
	colIdle = 0x8a8f98 // gray
	colRec  = 0xe53935 // red
	colTx   = 0x1e88e5 // blue
	colOK   = 0x43a047 // green
	colErr  = 0xd81b60 // pink
)

// makeICO renders a small filled-circle icon in the given color. Enough for a
// demo; a proper mic glyph can replace it later.
func makeICO(rgb uint32) []byte {
	const w, h = 32, 32
	r := byte(rgb >> 16)
	g := byte(rgb >> 8)
	b := byte(rgb)

	pixels := make([]byte, w*h*4)
	mask := make([]byte, h*(w/8)) // AND mask, opaque
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx := float64(x-w/2) + 0.5
			dy := float64(y-h/2) + 0.5
			if dx*dx+dy*dy <= 13.5*13.5 {
				i := ((h-1-y)*w + x) * 4 // BGRA, bottom-up
				pixels[i] = b
				pixels[i+1] = g
				pixels[i+2] = r
				pixels[i+3] = 255
			}
		}
	}

	var out bytes.Buffer
	out.Write([]byte{0, 0, 1, 0, 1, 0}) // ICONDIR: 1 image

	entry := bytes.Buffer{}
	entry.WriteByte(w)
	entry.WriteByte(h)
	entry.WriteByte(0) // palette
	entry.WriteByte(0) // reserved
	binary.Write(&entry, binary.LittleEndian, uint16(1))
	binary.Write(&entry, binary.LittleEndian, uint16(32))
	dataSize := 40 + len(pixels) + len(mask)
	binary.Write(&entry, binary.LittleEndian, uint32(dataSize))
	binary.Write(&entry, binary.LittleEndian, uint32(22)) // offset
	out.Write(entry.Bytes())

	// BITMAPINFOHEADER (height is doubled: XOR + AND masks)
	hdr := bytes.Buffer{}
	binary.Write(&hdr, binary.LittleEndian, uint32(40))
	binary.Write(&hdr, binary.LittleEndian, int32(w))
	binary.Write(&hdr, binary.LittleEndian, int32(h*2))
	binary.Write(&hdr, binary.LittleEndian, uint16(1))
	binary.Write(&hdr, binary.LittleEndian, uint16(32))
	binary.Write(&hdr, binary.LittleEndian, uint32(0))
	binary.Write(&hdr, binary.LittleEndian, uint32(len(pixels)+len(mask)))
	hdr.Write(make([]byte, 16)) // rest of the header is zero
	out.Write(hdr.Bytes())

	out.Write(pixels)
	out.Write(mask)
	return out.Bytes()
}
