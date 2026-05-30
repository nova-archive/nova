// Package transform wraps govips/libvips for image decode, resize, encode, and
// codec validation. It is a pure leaf package — only govips and stdlib; no
// nova-image config or handler types.
package transform

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"
)

// Sentinel errors.
var (
	ErrTooManyPixels     = errors.New("transform: image exceeds max megapixels")
	ErrDecode            = errors.New("transform: cannot decode image")
	ErrFormatUnsupported = errors.New("transform: unsupported output format")
)

// startupOnce ensures vips.Startup is called exactly once across all callers.
var (
	startupOnce sync.Once
	startupErr  error
)

// Startup initialises libvips. If cacheMaxMem is 0 a default of 128 MiB is
// used. It is idempotent — subsequent calls return the same error (or nil) as
// the first call. Callers do not need to call this explicitly; Render and
// Decode invoke it lazily.
func Startup(cacheMaxMem int64) error {
	startupOnce.Do(func() {
		mem := int(cacheMaxMem)
		if mem <= 0 {
			mem = 128 * 1024 * 1024 // 128 MiB
		}
		// Suppress verbose govips logging.
		vips.LoggingSettings(nil, vips.LogLevelError)
		startupErr = vips.Startup(&vips.Config{
			MaxCacheMem: mem,
		})
	})
	return startupErr
}

// Bounds defines safety limits for the Transformer.
type Bounds struct {
	// MaxMegapixels is the maximum allowed source image size in megapixels
	// (width*height/1_000_000). Zero means no limit.
	MaxMegapixels int
	// MaxConcurrent is the maximum number of concurrent Render operations. If
	// <=0, defaults to 4.
	MaxConcurrent int
}

// Spec describes the desired output geometry. An empty Spec means transcode
// only (no resize).
type Spec struct {
	// Width: scale to this width, preserving aspect ratio. Only used when
	// BoxW==0.
	Width int
	// BoxW, BoxH: resize to fit exactly into this box. Fit controls crop vs
	// contain behaviour.
	BoxW, BoxH int
	// Fit: "cover" = cover-crop to exact BoxW×BoxH dimensions.
	// Any other value (including empty) = fit within the box (contain).
	Fit string
}

// Transformer performs image operations subject to concurrency and megapixel
// bounds.
type Transformer struct {
	bounds Bounds
	sem    chan struct{}
}

// New creates a Transformer. See Bounds for field semantics.
func New(b Bounds) *Transformer {
	n := b.MaxConcurrent
	if n <= 0 {
		n = 4
	}
	return &Transformer{
		bounds: b,
		sem:    make(chan struct{}, n),
	}
}

// Render decodes src, applies spec (resize/crop), encodes to format, and
// returns the encoded bytes plus the output dimensions. format is a file
// extension string: "jpeg"/"jpg", "png", "webp", "avif", "jxl".
func (t *Transformer) Render(src []byte, spec Spec, format string) (out []byte, w, h int, err error) {
	if err := Startup(0); err != nil {
		return nil, 0, 0, fmt.Errorf("transform: vips startup: %w", err)
	}

	// Acquire concurrency slot.
	t.sem <- struct{}{}
	defer func() { <-t.sem }()

	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %w", ErrDecode, err)
	}
	defer img.Close()

	// Megapixel guard (before any operation, using source dimensions).
	if t.bounds.MaxMegapixels > 0 {
		if img.Width()*img.Height() > t.bounds.MaxMegapixels*1_000_000 {
			return nil, 0, 0, ErrTooManyPixels
		}
	}

	// Resize / crop.
	switch {
	case spec.Width > 0 && spec.BoxW == 0:
		scale := float64(spec.Width) / float64(img.Width())
		if err := img.Resize(scale, vips.KernelLanczos3); err != nil {
			return nil, 0, 0, fmt.Errorf("transform: resize: %w", err)
		}

	case spec.BoxW > 0 && spec.BoxH > 0:
		// Thumbnail with cover-crop produces an exact BoxW×BoxH image.
		// InterestingCentre centres the attention area.
		crop := vips.InterestingNone
		if strings.EqualFold(spec.Fit, "cover") {
			crop = vips.InterestingCentre
		}
		if err := img.Thumbnail(spec.BoxW, spec.BoxH, crop); err != nil {
			return nil, 0, 0, fmt.Errorf("transform: thumbnail: %w", err)
		}
	}
	// else: empty Spec — transcode only, no resize.

	encoded, err := encode(img, format)
	if err != nil {
		return nil, 0, 0, err
	}

	return encoded, img.Width(), img.Height(), nil
}

// Decode decodes src and returns its width and height without any output
// encoding. Useful for dimension inspection at upload time.
func (t *Transformer) Decode(src []byte) (w, h int, err error) {
	if err := Startup(0); err != nil {
		return 0, 0, fmt.Errorf("transform: vips startup: %w", err)
	}

	img, err := vips.NewImageFromBuffer(src)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w", ErrDecode, err)
	}
	defer img.Close()

	if t.bounds.MaxMegapixels > 0 {
		if img.Width()*img.Height() > t.bounds.MaxMegapixels*1_000_000 {
			return 0, 0, ErrTooManyPixels
		}
	}

	return img.Width(), img.Height(), nil
}

// ValidateCodecs checks that the given input and output format strings are
// usable in this libvips build.
//
// For OUTPUT formats the check is rigorous: a 1×1 synthetic image is encoded to
// each requested format; any failure means the codec saver is missing.
//
// For INPUT formats the check is best-effort: known format names are accepted
// unconditionally (true codec availability surfaces at decode time). An
// unrecognised INPUT format string returns an error.
func ValidateCodecs(inputs, outputs []string) error {
	if err := Startup(0); err != nil {
		return fmt.Errorf("transform: vips startup: %w", err)
	}

	// Check input formats — best-effort.
	known := map[string]bool{
		"jpeg": true, "jpg": true, "png": true, "webp": true,
		"gif": true, "tiff": true, "tif": true, "bmp": true,
		"avif": true, "jxl": true, "heif": true, "heic": true,
	}
	for _, f := range inputs {
		if !known[strings.ToLower(f)] {
			return fmt.Errorf("transform: unrecognised input format %q", f)
		}
	}

	// Check output formats — rigorous encode probe.
	probe, err := vips.Black(1, 1)
	if err != nil {
		return fmt.Errorf("transform: cannot create probe image: %w", err)
	}
	defer probe.Close()

	for _, f := range outputs {
		if _, err := encode(probe, f); err != nil {
			return fmt.Errorf("transform: output format %q unavailable in this libvips build: %w", f, err)
		}
	}

	return nil
}

// encode encodes img to the given format and returns the bytes.
// format is normalised: "jpg" is treated as "jpeg".
func encode(img *vips.ImageRef, format string) ([]byte, error) {
	f := strings.ToLower(format)
	if f == "jpg" {
		f = "jpeg"
	}

	switch f {
	case "jpeg":
		params := vips.NewJpegExportParams()
		params.Quality = 82
		b, _, err := img.ExportJpeg(params)
		return b, err

	case "png":
		params := vips.NewPngExportParams()
		b, _, err := img.ExportPng(params)
		return b, err

	case "webp":
		params := vips.NewWebpExportParams()
		params.Quality = 80
		b, _, err := img.ExportWebp(params)
		return b, err

	case "avif":
		params := vips.NewAvifExportParams()
		params.Quality = 60
		b, _, err := img.ExportAvif(params)
		return b, err

	case "jxl":
		params := vips.NewJxlExportParams()
		params.Quality = 75
		b, _, err := img.ExportJxl(params)
		return b, err

	default:
		return nil, fmt.Errorf("%w: %q", ErrFormatUnsupported, format)
	}
}
