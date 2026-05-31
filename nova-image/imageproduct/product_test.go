package imageproduct

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// fakeTx satisfies pgx.Tx; only Exec is overridden.
type fakeTx struct {
	pgx.Tx
	sql  string
	args []any
}

func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql, f.args = sql, args
	return pgconn.CommandTag{}, nil
}

// makeRGBAImage returns a small RGBA image of the given dimensions.
func makeRGBAImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	return img
}

func encodeJPEG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestName checks the product name.
func TestName(t *testing.T) {
	p := New(novaimage.DefaultConfig(), nil, nil, nil)
	require.Equal(t, "image", p.Name())
}

// TestAcceptedMimeTypes checks that the default config yields the expected MIME set.
func TestAcceptedMimeTypes(t *testing.T) {
	p := New(novaimage.DefaultConfig(), nil, nil, nil)
	mimes := p.AcceptedMimeTypes()

	set := make(map[string]bool)
	for _, m := range mimes {
		set[m] = true
	}

	require.True(t, set["image/jpeg"], "should contain image/jpeg")
	require.True(t, set["image/png"], "should contain image/png")
	require.True(t, set["image/webp"], "should contain image/webp")
	require.False(t, set["image/avif"], "avif is off by default")
}

// TestAnalyzeUpload_JPEG_NoConversion verifies pass-through for JPEG with
// format conversion disabled (the default).
func TestAnalyzeUpload_JPEG_NoConversion(t *testing.T) {
	cfg := novaimage.DefaultConfig() // FormatConversion.Enabled == false
	p := New(cfg, nil, nil, nil)

	img := makeRGBAImage(16, 12)
	jpgBytes := encodeJPEG(img)

	ctx := context.Background()
	uc := &product.UploadContext{DeclaredMimeType: "image/jpeg"}

	meta, scan, transformed, err := p.AnalyzeUpload(ctx, uc, bytes.NewReader(jpgBytes))
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.NotNil(t, scan)
	require.Equal(t, storage.ActionAllow, scan.Action)
	require.Nil(t, transformed, "no conversion: transformed should be nil")

	im, ok := meta.(ImageMetadata)
	require.True(t, ok, "metadata should be ImageMetadata")
	require.Equal(t, 16, im.Width)
	require.Equal(t, 12, im.Height)
}

// TestAnalyzeUpload_PNG_FormatConversion verifies that a PNG is re-encoded to
// WebP when format conversion is enabled.
func TestAnalyzeUpload_PNG_FormatConversion(t *testing.T) {
	cfg := novaimage.DefaultConfig()
	cfg.FormatConversion = novaimage.FormatConversion{Enabled: true, Target: "webp", Lossless: true}

	p := New(cfg, nil, nil, nil)

	img := makeRGBAImage(32, 24)
	pngBytes := encodePNG(img)

	ctx := context.Background()
	uc := &product.UploadContext{DeclaredMimeType: "image/png"}

	meta, scan, transformed, err := p.AnalyzeUpload(ctx, uc, bytes.NewReader(pngBytes))
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.NotNil(t, scan)
	require.Equal(t, storage.ActionAllow, scan.Action)
	require.NotNil(t, transformed, "format conversion enabled: transformed should be non-nil")

	outBytes, rerr := bytes.NewBuffer(nil), error(nil)
	buf := new(bytes.Buffer)
	_, rerr = buf.ReadFrom(transformed)
	require.NoError(t, rerr)
	outBytes = buf
	require.NotEmpty(t, outBytes.Bytes())

	// Check WebP magic: RIFF....WEBP
	data := outBytes.Bytes()
	require.GreaterOrEqual(t, len(data), 12)
	require.Equal(t, "RIFF", string(data[0:4]), "should be RIFF header")
	require.Equal(t, "WEBP", string(data[8:12]), "should be WEBP header")

	im, ok := meta.(ImageMetadata)
	require.True(t, ok)
	require.Equal(t, 32, im.Width)
	require.Equal(t, 24, im.Height)
}

// TestAnalyzeUpload_NonImage_Rejected verifies that a non-image MIME is rejected.
func TestAnalyzeUpload_NonImage_Rejected(t *testing.T) {
	p := New(novaimage.DefaultConfig(), nil, nil, nil)

	ctx := context.Background()
	uc := &product.UploadContext{DeclaredMimeType: "text/plain"}

	meta, scan, transformed, err := p.AnalyzeUpload(ctx, uc, bytes.NewReader([]byte("hello world")))
	require.ErrorIs(t, err, ErrNotAnImage)
	require.Nil(t, meta)
	require.Nil(t, scan)
	require.Nil(t, transformed)
}

// TestOnDelete_CascadesChildState verifies that OnDelete issues the correct UPDATE.
func TestOnDelete_CascadesChildState(t *testing.T) {
	p := New(novaimage.DefaultConfig(), nil, nil, nil)

	ft := &fakeTx{}
	ctx := context.Background()

	err := p.OnDelete(ctx, ft, "parentcid123", "soft_deleted")
	require.NoError(t, err)
	require.Contains(t, ft.sql, "UPDATE blobs SET state")
	require.Len(t, ft.args, 2)
	require.Equal(t, "soft_deleted", ft.args[0])
	require.Equal(t, "parentcid123", ft.args[1])
}

// TestOnCommitted_NilQueueNoop verifies that OnCommitted is a no-op when queue is nil.
func TestOnCommitted_NilQueueNoop(t *testing.T) {
	p := New(novaimage.DefaultConfig(), nil, nil, nil)
	ctx := context.Background()
	err := p.OnCommitted(ctx, &storage.CommittedRef{CID: "x"}, nil)
	require.NoError(t, err)
}

// TestPresetURLs verifies that PresetURLs returns the expected canonical URLs.
func TestPresetURLs(t *testing.T) {
	p := New(novaimage.DefaultConfig(), nil, nil, nil)
	urls := p.PresetURLs("bafyCID")

	// DefaultConfig has presets: thumb (webp), og (jpeg), hero (webp).
	require.Equal(t, "/i/bafyCID/p/thumb.webp", urls["thumb"])
	require.Equal(t, "/i/bafyCID/p/og.jpeg", urls["og"])
	require.Equal(t, "/i/bafyCID/p/hero.webp", urls["hero"])
	require.Len(t, urls, 3, "should have exactly 3 presets")
}

// TestValidateCodecsDefaults verifies that the default allowed formats are all
// available in the local libvips build (jpeg/png/webp are universally supported).
func TestValidateCodecsDefaults(t *testing.T) {
	cfg := novaimage.DefaultConfig()
	require.NoError(t, ValidateCodecs(cfg.AllowedInputFormats, cfg.AllowedOutputFormats))
}
