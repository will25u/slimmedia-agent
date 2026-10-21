package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func makeTestPNG(t *testing.T, size int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0xaa, A: 0xff})
		}
	}
	b, err := encodePNG(img)
	if err != nil {
		t.Fatalf("encodePNG: %v", err)
	}
	return b
}

type icoEntry struct {
	width    byte
	height   byte
	planes   uint16
	bitCount uint16
	size     uint32
	offset   uint32
}

func parseICO(t *testing.T, ico []byte) (uint16, []icoEntry) {
	t.Helper()
	if len(ico) < icoHeaderLen {
		t.Fatalf("ICO too short: %d bytes", len(ico))
	}
	if reserved := binary.LittleEndian.Uint16(ico[0:2]); reserved != 0 {
		t.Fatalf("reserved = %d, want 0", reserved)
	}
	typ := binary.LittleEndian.Uint16(ico[2:4])
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if len(ico) < icoHeaderLen+icoEntryLen*count {
		t.Fatalf("ICO header truncated: %d bytes for %d entries", len(ico), count)
	}
	entries := make([]icoEntry, 0, count)
	for i := 0; i < count; i++ {
		p := icoHeaderLen + icoEntryLen*i
		entries = append(entries, icoEntry{
			width:    ico[p],
			height:   ico[p+1],
			planes:   binary.LittleEndian.Uint16(ico[p+4:]),
			bitCount: binary.LittleEndian.Uint16(ico[p+6:]),
			size:     binary.LittleEndian.Uint32(ico[p+8:]),
			offset:   binary.LittleEndian.Uint32(ico[p+12:]),
		})
		if ico[p+2] != 0 || ico[p+3] != 0 {
			t.Errorf("entry %d: colorCount/reserved = %d/%d, want 0/0", i, ico[p+2], ico[p+3])
		}
	}
	return typ, entries
}

func TestEncodeICORoundtrip(t *testing.T) {
	png16 := makeTestPNG(t, 16)
	png32 := makeTestPNG(t, 32)

	ico, err := EncodeICO(png16, png32)
	if err != nil {
		t.Fatalf("EncodeICO: %v", err)
	}

	typ, entries := parseICO(t, ico)
	if typ != 1 {
		t.Fatalf("type = %d, want 1", typ)
	}
	if len(entries) != 2 {
		t.Fatalf("count = %d, want 2", len(entries))
	}

	wantOffset := uint32(icoHeaderLen + icoEntryLen*2)
	for i, png := range [][]byte{png16, png32} {
		e := entries[i]
		if e.size != uint32(len(png)) {
			t.Errorf("entry %d: bytesInRes = %d, want %d", i, e.size, len(png))
		}
		if e.offset != wantOffset {
			t.Errorf("entry %d: offset = %d, want %d", i, e.offset, wantOffset)
		}
		if int(e.offset+e.size) > len(ico) {
			t.Fatalf("entry %d: payload [%d:%d) exceeds ICO length %d", i, e.offset, e.offset+e.size, len(ico))
		}
		payload := ico[e.offset : e.offset+e.size]
		if !bytes.Equal(payload, png) {
			t.Errorf("entry %d: embedded PNG is not byte-identical to input", i)
		}
		wantOffset += uint32(len(png))
	}
	if wantOffset != uint32(len(ico)) {
		t.Errorf("ICO length = %d, want %d (no trailing garbage)", len(ico), wantOffset)
	}

	if entries[0].width != 16 || entries[0].height != 16 {
		t.Errorf("entry 0 dims = %dx%d, want 16x16", entries[0].width, entries[0].height)
	}
	if entries[1].width != 32 || entries[1].height != 32 {
		t.Errorf("entry 1 dims = %dx%d, want 32x32", entries[1].width, entries[1].height)
	}
	for i, e := range entries {
		if e.planes != 1 || e.bitCount != 32 {
			t.Errorf("entry %d: planes/bitCount = %d/%d, want 1/32", i, e.planes, e.bitCount)
		}
	}
}

func TestEncodeICO256UsesZeroByte(t *testing.T) {
	png256 := makeTestPNG(t, 256)
	ico, err := EncodeICO(png256)
	if err != nil {
		t.Fatalf("EncodeICO: %v", err)
	}
	_, entries := parseICO(t, ico)
	if len(entries) != 1 {
		t.Fatalf("count = %d, want 1", len(entries))
	}
	if entries[0].width != 0 || entries[0].height != 0 {
		t.Errorf("dims bytes = %d/%d, want 0/0 for 256px", entries[0].width, entries[0].height)
	}
}

func TestEncodeICOErrors(t *testing.T) {
	if _, err := EncodeICO(); err == nil {
		t.Error("EncodeICO() with no images: want error, got nil")
	}
	if _, err := EncodeICO([]byte("not a png at all")); err == nil {
		t.Error("non-PNG input: want error, got nil")
	}
	truncated := []byte(pngSignature + "\x00\x00")
	if _, err := EncodeICO(truncated); err == nil {
		t.Error("truncated PNG: want error, got nil")
	}
}

func TestStatusIcon(t *testing.T) {
	conns := []ConnState{
		ConnConnected,
		ConnReconnecting,
		ConnStarting,
		ConnOffline,
	}

	for _, conn := range conns {
		t.Run(string(conn), func(t *testing.T) {
			ico := StatusIcon(conn)
			if len(ico) == 0 {
				t.Fatal("StatusIcon returned no bytes")
			}
			typ, entries := parseICO(t, ico)
			if typ != 1 {
				t.Fatalf("type = %d, want 1", typ)
			}
			if len(entries) != 2 {
				t.Fatalf("count = %d, want 2 (16px and 32px)", len(entries))
			}
			if entries[0].width != 16 || entries[1].width != 32 {
				t.Errorf("width bytes = %d/%d, want 16/32", entries[0].width, entries[1].width)
			}
			for i, e := range entries {
				payload := ico[e.offset : e.offset+e.size]
				if !bytes.HasPrefix(payload, []byte(pngSignature)) {
					t.Errorf("entry %d does not embed a PNG", i)
				}
				img, err := png.Decode(bytes.NewReader(payload))
				if err != nil {
					t.Errorf("entry %d: embedded PNG does not decode: %v", i, err)
					continue
				}
				bounds := img.Bounds()
				if i == 0 && (bounds.Dx() != 16 || bounds.Dy() != 16) {
					t.Errorf("entry 0 dimensions = %dx%d, want 16x16", bounds.Dx(), bounds.Dy())
				}
				if i == 1 && (bounds.Dx() != 32 || bounds.Dy() != 32) {
					t.Errorf("entry 1 dimensions = %dx%d, want 32x32", bounds.Dx(), bounds.Dy())
				}
			}
		})
	}
}

func TestMascotImage(t *testing.T) {
	img, err := MascotImage()
	if err != nil {
		t.Fatalf("MascotImage failed: %v", err)
	}
	if img == nil {
		t.Fatal("MascotImage returned nil image")
	}
	bounds := img.Bounds()
	if bounds.Dx() != 256 || bounds.Dy() != 256 {
		t.Errorf("MascotImage dimensions = %dx%d, want 256x256", bounds.Dx(), bounds.Dy())
	}
}
