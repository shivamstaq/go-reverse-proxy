package fixtures

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
)

// gzipBytes compresses b with gzip.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// pngWithKeyword builds a small, valid PNG that contains Keyword as literal
// bytes inside a tEXt metadata chunk. The keyword is present in the file but is
// not text the proxy is allowed to touch: rewriting it changes the byte length
// of a chunk whose declared length and CRC no longer match, so the image stops
// decoding. That makes "did the proxy corrupt a binary?" a yes/no question.
func pngWithKeyword() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			img.Set(x, y, color.RGBA{R: uint8(x * 32), G: uint8(y * 32), B: 128, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	raw := buf.Bytes()

	// A PNG ends with a 12-byte IEND chunk. Ancillary chunks are legal anywhere
	// between IHDR and IEND, so splice ours in just before it.
	iend := len(raw) - 12

	text := append([]byte("Comment\x00"), []byte("contains "+Keyword+" literally")...)

	out := make([]byte, 0, len(raw)+len(text)+12)
	out = append(out, raw[:iend]...)
	out = append(out, pngChunk("tEXt", text)...)
	out = append(out, raw[iend:]...)
	return out
}

// pngChunk frames data as a PNG chunk: length, type, data, CRC of type+data.
func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)

	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	return binary.BigEndian.AppendUint32(b, crc.Sum32())
}
