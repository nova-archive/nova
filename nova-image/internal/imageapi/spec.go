// Package imageapi implements the nova-image /i/* read + transform routes.
package imageapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/internal/transform"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Store is the storage surface the /i/* handler needs (satisfied by *storage.Service).
type Store interface {
	Resolve(ctx context.Context, cid string) (*storage.BlobView, error)
	OpenBytes(ctx context.Context, v *storage.BlobView) (io.ReadCloser, error)
	PutDerivative(ctx context.Context, plaintext []byte, dc storage.DerivativeContext,
		persist func(context.Context, pgx.Tx, string) error) (*storage.PutResult, error)
	GetDerivativeCID(ctx context.Context, parent, preset, format string) (string, bool, error)
}

// Route-classification errors (mapped to HTTP status by the handler).
var (
	ErrUnknownPreset       = errors.New("imageapi: unknown preset")            // 404
	ErrDimensionNotAllowed = errors.New("imageapi: dimension not allowed")     // 400
	ErrFormatNotAllowed    = errors.New("imageapi: output format not allowed") // 406
)

// Kind identifies the transform-route shape.
type Kind int

const (
	KindOrig   Kind = iota // /i/{cid}.{ext}        — transcode, no resize
	KindWidth              // /i/{cid}/w{value}.{ext}
	KindBox                // /i/{cid}/{value}.{ext} where value = "WxH"
	KindPreset             // /i/{cid}/p/{value}.{ext}
)

// normalizeFormat folds "jpg" to "jpeg".
func normalizeFormat(ext string) string {
	ext = strings.ToLower(ext)
	if ext == "jpg" {
		return "jpeg"
	}
	return ext
}

// ResolveSpec validates a transform route against the operator config and
// returns the transform spec, the canonical derivative-lookup key (presetKey),
// and the normalized output format. The bare /i/{cid} route (serve stored
// original) does NOT go through ResolveSpec.
func ResolveSpec(cfg novaimage.Config, kind Kind, value, ext string) (spec transform.Spec, presetKey, format string, err error) {
	format = normalizeFormat(ext)
	if !cfg.OutputAllowed(format) {
		return transform.Spec{}, "", "", fmt.Errorf("%w: %q", ErrFormatNotAllowed, format)
	}
	switch kind {
	case KindOrig:
		return transform.Spec{}, "orig", format, nil
	case KindWidth:
		n, perr := strconv.Atoi(value)
		if perr != nil || !intIn(cfg.AllowedWidths, n) {
			return transform.Spec{}, "", "", fmt.Errorf("%w: w%s", ErrDimensionNotAllowed, value)
		}
		return transform.Spec{Width: n}, "w" + value, format, nil
	case KindBox:
		if !strIn(cfg.AllowedBoxes, value) {
			return transform.Spec{}, "", "", fmt.Errorf("%w: %s", ErrDimensionNotAllowed, value)
		}
		w, h, perr := parseBox(value)
		if perr != nil {
			return transform.Spec{}, "", "", fmt.Errorf("%w: %s", ErrDimensionNotAllowed, value)
		}
		return transform.Spec{BoxW: w, BoxH: h, Fit: "cover"}, value, format, nil
	case KindPreset:
		p, ok := cfg.Presets[value]
		if !ok {
			return transform.Spec{}, "", "", fmt.Errorf("%w: %s", ErrUnknownPreset, value)
		}
		sp := transform.Spec{Width: p.Width}
		if p.Box != "" {
			w, h, perr := parseBox(p.Box)
			if perr != nil {
				return transform.Spec{}, "", "", fmt.Errorf("imageapi: preset %q has invalid box %q", value, p.Box)
			}
			sp = transform.Spec{BoxW: w, BoxH: h, Fit: p.Fit}
		}
		return sp, value, format, nil
	default:
		return transform.Spec{}, "", "", fmt.Errorf("imageapi: unknown route kind")
	}
}

func parseBox(s string) (int, int, error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad box %q", s)
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad box %q", s)
	}
	return w, h, nil
}

func intIn(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func strIn(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
