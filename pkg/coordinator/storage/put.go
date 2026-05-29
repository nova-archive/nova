package storage

import (
	"fmt"
	"net/http"
	"strings"
)

// validateMIME is a cheap generic content floor. It blocks the XSS-relevant
// case of a text/script body declared as an image, without rejecting formats
// the stdlib sniffer doesn't recognize (e.g. AVIF → octet-stream). It does NOT
// prove the bytes are a valid instance of the declared type — that is the
// product layer's (M5) decode validation.
//
// Rules: empty declared ⇒ use detected; detected octet-stream (unknown) ⇒
// trust the declaration; otherwise reject when the detected top-level type
// contradicts the declared one.
func validateMIME(declared string, head []byte) (string, error) {
	detected := http.DetectContentType(head) // reads up to the first 512 bytes
	if declared == "" {
		return detected, nil
	}
	if detected == "application/octet-stream" {
		return declared, nil
	}
	if topLevel(detected) != topLevel(declared) {
		return "", fmt.Errorf("%w: declared %q, detected %q", ErrMimeRejected, declared, detected)
	}
	return declared, nil
}

func topLevel(mime string) string {
	if i := strings.IndexByte(mime, '/'); i >= 0 {
		return mime[:i]
	}
	return mime
}
