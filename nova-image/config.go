// Package novaimage is the nova-image product layer: image-specific upload
// validation, on-the-fly transforms, and the /i/* read routes. It plugs into
// the coordinator via pkg/coordinator/product.Product.
package novaimage

import "fmt"

// Preset is a named, operator-defined transform. Exactly one of Width or Box
// must be set. Box is "WxH" (cover-crop when Fit == "cover").
type Preset struct {
	Width  int    `yaml:"width,omitempty"`
	Box    string `yaml:"box,omitempty"`
	Fit    string `yaml:"fit,omitempty"`
	Format string `yaml:"format"`
}

// FormatConversion controls optional upload-time re-encoding of lossless inputs
// (PNG/BMP/TIFF) to a web format. Default off; Lossless default true so the
// canonical stored original is not degraded.
type FormatConversion struct {
	Enabled  bool   `yaml:"enabled"`
	Target   string `yaml:"target"`
	Lossless bool   `yaml:"lossless"`
}

// Config is the nova-image operator config (operator.yaml `image:` section).
type Config struct {
	AllowedInputFormats     []string          `yaml:"allowed_input_formats"`
	AllowedOutputFormats    []string          `yaml:"allowed_output_formats"`
	AllowedWidths           []int             `yaml:"allowed_widths"`
	AllowedBoxes            []string          `yaml:"allowed_boxes"`
	Presets                 map[string]Preset `yaml:"presets"`
	PrewarmPresets          []string          `yaml:"prewarm_presets"`
	FormatConversion        FormatConversion  `yaml:"format_conversion"`
	MaxMegapixels           int               `yaml:"max_megapixels"`
	MaxConcurrentTransforms int               `yaml:"max_concurrent_transforms"`
	VipsCacheMaxMemBytes    int64             `yaml:"vips_cache_max_mem_bytes"`
}

// DefaultConfig returns the Phase-1 defaults. AVIF and JXL are accepted/served
// only when the operator adds them (off by default): JXL because browser
// delivery support was still partial in 2026, both because encode is heavy.
func DefaultConfig() Config {
	return Config{
		AllowedInputFormats:  []string{"jpeg", "png", "webp", "gif", "tiff", "bmp"},
		AllowedOutputFormats: []string{"jpeg", "png", "webp"},
		AllowedWidths:        []int{320, 512, 1024, 2048},
		AllowedBoxes:         []string{"256x256", "1200x630"},
		Presets: map[string]Preset{
			"thumb": {Width: 256, Format: "webp"},
			"og":    {Box: "1200x630", Fit: "cover", Format: "jpeg"},
			"hero":  {Width: 1920, Format: "webp"},
		},
		PrewarmPresets:          []string{"thumb", "og"},
		FormatConversion:        FormatConversion{Enabled: false, Target: "webp", Lossless: true},
		MaxMegapixels:           100,
		MaxConcurrentTransforms: 4,
		VipsCacheMaxMemBytes:    134217728, // 128 MiB
	}
}

// OutputAllowed reports whether f is an enabled output format.
func (c Config) OutputAllowed(f string) bool {
	for _, a := range c.AllowedOutputFormats {
		if a == f {
			return true
		}
	}
	return false
}

// Validate checks internal consistency (presets reference enabled output
// formats and carry a dimension; format-conversion target is enabled).
func (c Config) Validate() error {
	for name, p := range c.Presets {
		if !c.OutputAllowed(p.Format) {
			return fmt.Errorf("nova-image: preset %q uses output format %q not in allowed_output_formats", name, p.Format)
		}
		if p.Width == 0 && p.Box == "" {
			return fmt.Errorf("nova-image: preset %q has neither width nor box", name)
		}
	}
	if c.FormatConversion.Enabled && !c.OutputAllowed(c.FormatConversion.Target) {
		return fmt.Errorf("nova-image: format_conversion.target %q not in allowed_output_formats", c.FormatConversion.Target)
	}
	return nil
}
