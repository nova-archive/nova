package novaimage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigDefaults(t *testing.T) {
	c := DefaultConfig()
	require.Contains(t, c.AllowedOutputFormats, "webp")
	require.NotContains(t, c.AllowedOutputFormats, "avif", "avif off by default")
	require.NotContains(t, c.AllowedOutputFormats, "jxl", "jxl off by default")
	require.Equal(t, 100, c.MaxMegapixels)
	require.Positive(t, c.MaxConcurrentTransforms)
	require.Positive(t, c.VipsCacheMaxMemBytes)
	require.NoError(t, c.Validate())
}

func TestConfigValidateRejectsUnknownPresetFormat(t *testing.T) {
	c := DefaultConfig()
	c.Presets = map[string]Preset{"bad": {Width: 100, Format: "avif"}} // avif not in default allowed_output
	require.Error(t, c.Validate())
}

func TestConfigValidateRejectsPresetWithNoDimension(t *testing.T) {
	c := DefaultConfig()
	c.Presets = map[string]Preset{"bad": {Format: "webp"}} // neither width nor box
	require.Error(t, c.Validate())
}
