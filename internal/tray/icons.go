package tray

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
)

//go:embed mascot_16.png
var mascot16PNG []byte

//go:embed mascot_32.png
var mascot32PNG []byte

//go:embed mascot_256.png
var mascot256PNG []byte

var mascotICO []byte

func init() {
	var err error
	mascotICO, err = EncodeICO(mascot16PNG, mascot32PNG)
	if err != nil {
		mascotICO = mascot16PNG
	}
}

// MascotImage decodes and returns the high-resolution mascot image.
func MascotImage() (image.Image, error) {
	return png.Decode(bytes.NewReader(mascot256PNG))
}

// ICO container layout sizes: a 6-byte ICONDIR followed by one 16-byte
// ICONDIRENTRY per image, then the image payloads.
const (
	icoHeaderLen = 6
	icoEntryLen  = 16
)

// pngSignature is the 8-byte magic prefix every PNG starts with.
const pngSignature = "\x89PNG\r\n\x1a\n"

// EncodeICO encodes one or more PNG-encoded images into a Windows .ico container.
func EncodeICO(pngs ...[]byte) ([]byte, error) {
	if len(pngs) == 0 {
		return nil, errors.New("tray: EncodeICO requires at least one PNG image")
	}

	widths := make([]int, len(pngs))
	heights := make([]int, len(pngs))
	for i, p := range pngs {
		w, h, err := pngDimensions(p)
		if err != nil {
			return nil, err
		}
		widths[i], heights[i] = w, h
	}

	var buf bytes.Buffer
	// ICONDIR: reserved=0, type=1 (icon), image count.
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(pngs)))

	offset := icoHeaderLen + icoEntryLen*len(pngs)
	for i, p := range pngs {
		w, h := icoDimByte(widths[i]), icoDimByte(heights[i])
		buf.WriteByte(w)
		buf.WriteByte(h)
		buf.WriteByte(0)                                        // colorCount: not used for true-color images
		buf.WriteByte(0)                                        // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bitCount
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(p)))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(p)
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes(), nil
}

// pngDimensions validates the PNG signature and reads the width and height
// from the IHDR chunk (big-endian uint32 at offsets 16 and 20).
func pngDimensions(p []byte) (int, int, error) {
	if len(p) < 24 || !bytes.HasPrefix(p, []byte(pngSignature)) {
		return 0, 0, errors.New("tray: input is not a PNG image")
	}
	w := binary.BigEndian.Uint32(p[16:20])
	h := binary.BigEndian.Uint32(p[20:24])
	return int(w), int(h), nil
}

// icoDimByte maps a pixel dimension to the ICO entry byte, where 0 means 256.
func icoDimByte(d int) byte {
	if d <= 0 || d >= 256 {
		return 0
	}
	return byte(d)
}

// StatusIcon renders the tray icon for a connection state as ICO bytes. It
// returns the SlimMedia mascot icon. It never fails: encoding problems fall
// back to a minimal transparent icon.
func StatusIcon(conn ConnState) []byte {
	if len(mascotICO) > 0 {
		return mascotICO
	}
	col := statusColor(conn)

	var pngs [][]byte
	for _, size := range []int{16, 32} {
		if p, err := encodePNG(renderDot(size, col)); err == nil {
			pngs = append(pngs, p)
		}
	}
	if len(pngs) == 0 {
		p, _ := encodePNG(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
		pngs = append(pngs, p)
	}
	if ico, err := EncodeICO(pngs...); err == nil {
		return ico
	}
	return pngs[0]
}

// statusColor maps a connection state to its icon fill color.
func statusColor(conn ConnState) color.NRGBA {
	switch conn {
	case ConnConnected:
		return color.NRGBA{R: 0x2e, G: 0xcc, B: 0x71, A: 0xff}
	case ConnReconnecting, ConnStarting:
		return color.NRGBA{R: 0xf1, G: 0xc4, B: 0x0f, A: 0xff}
	default:
		return color.NRGBA{R: 0x95, G: 0xa5, B: 0xa6, A: 0xff}
	}
}

// renderDot draws a filled circle with a darker border on a transparent
// square of the given size.
func renderDot(size int, col color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	edge := darken(col, 0.55)

	center := float64(size-1) / 2
	radius := float64(size) / 2
	border := 1.0
	if size >= 32 {
		border = 2
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)-center, float64(y)-center)
			switch {
			case d <= radius-border:
				img.SetNRGBA(x, y, col)
			case d <= radius:
				img.SetNRGBA(x, y, edge)
			}
		}
	}
	return img
}

// darken scales a color's channels toward black, keeping alpha.
func darken(c color.NRGBA, f float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(float64(c.R) * f),
		G: uint8(float64(c.G) * f),
		B: uint8(float64(c.B) * f),
		A: c.A,
	}
}

// encodePNG encodes an image to PNG bytes.
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
