package img

import (
	"encoding/binary"
	"image"
)

// exifOrientation finds the EXIF Orientation tag (1-8) in a JPEG, or returns
// 1 (normal) if there isn't one. Go's JPEG decoder ignores EXIF, and we need
// only this one number, so this is a small hand parser rather than a
// dependency. It never panics on malformed input; it just gives up.
//
// Layout: the JPEG starts FFD8, then segments FFxx <len>. EXIF is the APP1
// segment (FFE1) starting "Exif\0\0", followed by a TIFF header (byte order,
// then the offset of the first directory). In that directory, tag 0x0112 is
// the orientation.
func exifOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xDA || size < 2 { // start of image data: no EXIF before it
			return 1
		}
		seg := i + 4
		end := i + 2 + size
		if end > len(b) {
			return 1
		}
		if marker == 0xE1 && end-seg >= 14 && string(b[seg:seg+6]) == "Exif\x00\x00" {
			return tiffOrientation(b[seg+6 : end])
		}
		i = end
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			v := int(bo.Uint16(t[e+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient returns src turned the right way up for EXIF orientation o.
//
//	1 normal        2 mirrored          3 rotated 180     4 mirrored vertically
//	5 transposed    6 rotated 90 CW     7 transversed     8 rotated 90 CCW
//
// (The number says how the camera held the sensor; applying the inverse
// gives the picture as the photographer saw it.)
func orient(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w // 5-8 swap width and height
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
