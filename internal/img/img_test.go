package img

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegWithOrientation makes a w x h JPEG, red on the left half, with an EXIF
// segment carrying orientation o and a fake GPS tag.
func jpegWithOrientation(t *testing.T, w, h, o int) []byte {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{0, 0, 255, 255}
			if x < w/2 {
				c = color.RGBA{255, 0, 0, 255}
			}
			m.Set(x, y, c)
		}
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, m, nil); err != nil {
		t.Fatal(err)
	}
	raw := enc.Bytes()

	// TIFF, little endian: header, one directory with two entries
	// (orientation, and a GPS pointer tag standing in for location data).
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 2, 0}
	entry := func(tag, typ uint16, count, val uint32) {
		e := make([]byte, 12)
		binary.LittleEndian.PutUint16(e[0:], tag)
		binary.LittleEndian.PutUint16(e[2:], typ)
		binary.LittleEndian.PutUint32(e[4:], count)
		binary.LittleEndian.PutUint32(e[8:], val)
		tiff = append(tiff, e...)
	}
	entry(0x0112, 3, 1, uint32(o))
	entry(0x8825, 4, 1, 0x12345678) // GPSInfo
	tiff = append(tiff, 0, 0, 0, 0)
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(app1)+2))
	seg = append(seg, app1...)

	out := append([]byte{}, raw[:2]...) // FFD8
	out = append(out, seg...)
	return append(out, raw[2:]...)
}

func TestOrientationAndStripping(t *testing.T) {
	in := jpegWithOrientation(t, 40, 20, 6) // stored sideways; rotate 90 CW
	if o := exifOrientation(in); o != 6 {
		t.Fatalf("read orientation %d, want 6", o)
	}
	r, err := Process(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Width != 20 || r.Height != 40 {
		t.Fatalf("size %dx%d, want 20x40 after rotating", r.Width, r.Height)
	}
	// Rotating 90 CW puts the red left half on top.
	out, _ := jpeg.Decode(bytes.NewReader(r.Full))
	if rr, _, bb, _ := out.At(10, 5).RGBA(); rr < bb {
		t.Error("top of the rotated image isn't red")
	}
	if bytes.Contains(r.Full, []byte("Exif")) || bytes.Contains(r.Thumb, []byte("Exif")) {
		t.Error("metadata survived re-encoding")
	}
}

func TestResize(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 4000, 1000))
	var b bytes.Buffer
	jpeg.Encode(&b, m, nil)
	r, err := Process(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if r.Width != FullMax || r.Height != 512 {
		t.Fatalf("full %dx%d", r.Width, r.Height)
	}
	th, _ := jpeg.DecodeConfig(bytes.NewReader(r.Thumb))
	if th.Width != ThumbMax {
		t.Fatalf("thumb width %d", th.Width)
	}
}

func TestRejectsGarbage(t *testing.T) {
	if _, err := Process([]byte("not an image")); err == nil {
		t.Fatal("accepted garbage")
	}
	// Malformed EXIF never panics.
	for i := 0; i < 64; i++ {
		exifOrientation(append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0, byte(i)}, []byte("Exif\x00\x00II*")...))
	}
}
