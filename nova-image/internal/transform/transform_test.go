package transform

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestRenderWidthPreservesAspect(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	out, w, h, err := tr.Render(makePNG(t, 800, 400), Spec{Width: 200}, "webp")
	require.NoError(t, err)
	require.Equal(t, 200, w)
	require.Equal(t, 100, h)
	require.True(t, len(out) > 0 && bytes.HasPrefix(out, []byte("RIFF")) && bytes.Contains(out[:16], []byte("WEBP")))
}

func TestRenderBoxCover(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	out, w, h, err := tr.Render(makePNG(t, 800, 400), Spec{BoxW: 100, BoxH: 100, Fit: "cover"}, "jpeg")
	require.NoError(t, err)
	require.Equal(t, 100, w)
	require.Equal(t, 100, h)
	require.True(t, bytes.HasPrefix(out, []byte{0xff, 0xd8, 0xff})) // JPEG SOI
}

func TestRenderTranscodeNoResize(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	out, w, h, err := tr.Render(makePNG(t, 64, 48), Spec{}, "webp") // empty Spec ⇒ transcode only
	require.NoError(t, err)
	require.Equal(t, 64, w)
	require.Equal(t, 48, h)
	require.True(t, bytes.HasPrefix(out, []byte("RIFF")))
}

func TestRenderMegapixelReject(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 1, MaxConcurrent: 2})                       // 1 MP cap
	_, _, _, err := tr.Render(makePNG(t, 2000, 2000), Spec{Width: 100}, "webp") // 4 MP source
	require.ErrorIs(t, err, ErrTooManyPixels)
}

func TestDecodeReturnsDims(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	w, h, err := tr.Decode(makePNG(t, 320, 240))
	require.NoError(t, err)
	require.Equal(t, 320, w)
	require.Equal(t, 240, h)
}

func TestValidateCodecsPassesForCoreFormats(t *testing.T) {
	require.NoError(t, Startup(0)) // 0 ⇒ a sane default cache cap
	require.NoError(t, ValidateCodecs([]string{"png", "jpeg"}, []string{"jpeg", "png", "webp"}))
}

// TestRenderLossless verifies that Spec.Lossless is threaded into the encoder.
// It uses a 64×64 gradient PNG (enough variation for lossy vs lossless to
// differ in encoded bytes) and encodes to webp both ways.
func TestRenderLossless(t *testing.T) {
	tr := New(Bounds{MaxMegapixels: 100, MaxConcurrent: 2})
	src := makePNG(t, 64, 64)

	// Lossless round-trip: output must be non-empty and decode to original dims.
	outLossless, w, h, err := tr.Render(src, Spec{Lossless: true}, "webp")
	require.NoError(t, err)
	require.Equal(t, 64, w)
	require.Equal(t, 64, h)
	require.NotEmpty(t, outLossless)
	require.True(t, bytes.HasPrefix(outLossless, []byte("RIFF")) && bytes.Contains(outLossless[:16], []byte("WEBP")))

	// Lossy encode of the same source.
	outLossy, w2, h2, err := tr.Render(src, Spec{Lossless: false}, "webp")
	require.NoError(t, err)
	require.Equal(t, 64, w2)
	require.Equal(t, 64, h2)
	require.NotEmpty(t, outLossy)

	// The two encodings must produce different byte streams — lossless WebP is
	// always larger (and structurally different) than lossy WebP for a
	// gradient image.
	require.NotEqual(t, outLossless, outLossy, "lossless and lossy webp outputs should differ")
}
