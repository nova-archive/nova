package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMIME(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	webp := append(append([]byte("RIFF"), 0, 0, 0, 0), []byte("WEBPVP8 ")...)
	script := []byte("#!/bin/sh\necho hi\n")

	cases := []struct {
		name     string
		declared string
		body     []byte
		want     string
		wantErr  bool
	}{
		{"jpeg ok", "image/jpeg", jpeg, "image/jpeg", false},
		{"png ok", "image/png", png, "image/png", false},
		{"webp ok", "image/webp", webp, "image/webp", false},
		{"empty declared uses detected", "", png, "image/png", false},
		{"unknown sniff accepts declared", "image/avif", []byte{0, 0, 0, 0x1c}, "image/avif", false},
		{"contradiction rejected", "image/jpeg", script, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateMIME(tc.declared, tc.body)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMimeRejected)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
