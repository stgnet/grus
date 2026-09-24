// Package img turns an uploaded photo into the two JPEGs the site stores.
//
// Every photo is decoded and re-encoded by us, never stored as uploaded.
// That does three jobs at once:
//   - It applies the EXIF orientation (phones store photos sideways and set
//     a flag), so the stored pixels are the right way up.
//   - It drops all metadata, GPS included. For an RV crowd a photo's
//     location is often someone's campsite or home, so this matters.
//   - Whatever reaches a reader is an image we produced, not a file someone
//     crafted.
package img

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"io"

	_ "image/gif" // decoders register themselves with image.Decode
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	FullMax   = 2048 // longest side of the stored full-size image
	ThumbMax  = 480  // longest side of the thumbnail
	MaxPixels = 50_000_000
	quality   = 82
)

// ErrTooBig is returned for images whose dimensions are unreasonable: a tiny
// file can claim to be 100000x100000 and exhaust memory when decoded.
var ErrTooBig = errors.New("image is too large")

// Result is a processed photo.
type Result struct {
	Full, Thumb   []byte
	Width, Height int // of Full
}

// Process decodes an uploaded image and returns the full-size and thumbnail
// JPEGs.
func Process(data []byte) (*Result, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > MaxPixels || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrTooBig
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	src = orient(src, exifOrientation(data))

	full := fit(src, FullMax)
	thumb := fit(src, ThumbMax)
	var fb, tb bytes.Buffer
	if err := jpeg.Encode(&fb, full, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	if err := jpeg.Encode(&tb, thumb, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	b := full.Bounds()
	return &Result{Full: fb.Bytes(), Thumb: tb.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}

// ReadLimited reads at most max bytes, failing if there are more.
func ReadLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooBig
	}
	return data, nil
}

// fit scales src down so its longest side is at most max. Smaller images
// are copied as they are (never scaled up). Either way the result is a
// fresh RGBA image, which is also what flattens any transparency onto
// white before JPEG encoding.
func fit(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > max || h > max {
		if w >= h {
			h, w = h*max/w, max
		} else {
			w, h = w*max/h, max
		}
		if w < 1 {
			w = 1
		}
		if h < 1 {
			h = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	// Catmull-Rom is slower than bilinear but noticeably sharper for
	// photos, and each image is processed once.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
