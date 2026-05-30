package imageapi

import (
	"errors"
	"testing"

	novaimage "github.com/nova-archive/nova/nova-image"
	"github.com/nova-archive/nova/nova-image/internal/transform"
	"github.com/stretchr/testify/require"
)

func TestResolveSpec(t *testing.T) {
	cfg := novaimage.DefaultConfig()

	tests := []struct {
		name     string
		kind     Kind
		value    string
		ext      string
		wantSpec transform.Spec
		wantKey  string
		wantFmt  string
		wantErr  error
	}{
		{
			name:     "KindWidth 512 webp",
			kind:     KindWidth,
			value:    "512",
			ext:      "webp",
			wantSpec: transform.Spec{Width: 512},
			wantKey:  "w512",
			wantFmt:  "webp",
		},
		{
			name:    "KindWidth 999 webp — not allowed",
			kind:    KindWidth,
			value:   "999",
			ext:     "webp",
			wantErr: ErrDimensionNotAllowed,
		},
		{
			name:     "KindBox 256x256 jpeg",
			kind:     KindBox,
			value:    "256x256",
			ext:      "jpeg",
			wantSpec: transform.Spec{BoxW: 256, BoxH: 256, Fit: "cover"},
			wantKey:  "256x256",
			wantFmt:  "jpeg",
		},
		{
			name:    "KindBox 300x300 jpeg — not allowed",
			kind:    KindBox,
			value:   "300x300",
			ext:     "jpeg",
			wantErr: ErrDimensionNotAllowed,
		},
		{
			name:     "KindPreset thumb webp",
			kind:     KindPreset,
			value:    "thumb",
			ext:      "webp",
			wantSpec: transform.Spec{Width: 256},
			wantKey:  "thumb",
			wantFmt:  "webp",
		},
		{
			name:     "KindPreset og jpeg",
			kind:     KindPreset,
			value:    "og",
			ext:      "jpeg",
			wantSpec: transform.Spec{BoxW: 1200, BoxH: 630, Fit: "cover"},
			wantKey:  "og",
			wantFmt:  "jpeg",
		},
		{
			name:    "KindPreset missing — ErrUnknownPreset",
			kind:    KindPreset,
			value:   "missing",
			ext:     "webp",
			wantErr: ErrUnknownPreset,
		},
		{
			name:     "KindOrig empty webp",
			kind:     KindOrig,
			value:    "",
			ext:      "webp",
			wantSpec: transform.Spec{},
			wantKey:  "orig",
			wantFmt:  "webp",
		},
		{
			name:    "avif not in default allowed_output",
			kind:    KindOrig,
			value:   "",
			ext:     "avif",
			wantErr: ErrFormatNotAllowed,
		},
		{
			name:     "ext jpg normalizes to jpeg",
			kind:     KindOrig,
			value:    "",
			ext:      "jpg",
			wantSpec: transform.Spec{},
			wantKey:  "orig",
			wantFmt:  "jpeg",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, key, fmt, err := ResolveSpec(cfg, tc.kind, tc.value, tc.ext)
			if tc.wantErr != nil {
				require.Error(t, err)
				require.True(t, errors.Is(err, tc.wantErr), "want %v, got %v", tc.wantErr, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantSpec, spec)
			require.Equal(t, tc.wantKey, key)
			require.Equal(t, tc.wantFmt, fmt)
		})
	}
}
