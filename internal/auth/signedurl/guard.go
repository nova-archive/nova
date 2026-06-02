package signedurl

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Guard returns chi-compatible middleware that verifies a signed URL when the
// request carries any signed-URL parameter, and otherwise passes through
// unchanged — so public reads stay anonymous and private reads still get the
// usual 401. On a verified signature it grants the request authorization to
// read private content via storage.WithReadAuthz; on any failure it returns 403
// with the specific signature_* code in the body. Response timing is uniform
// across failures (the verifier always runs a constant-time HMAC compare).
func (v *Verifier) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !HasParams(q) {
			next.ServeHTTP(w, r)
			return
		}
		d := v.Verify(r.Context(), VerifyInput{
			Path:    r.URL.Path,
			Query:   q,
			Origin:  r.Header.Get("Origin"),
			Referer: r.Header.Get("Referer"),
			Now:     time.Now(),
		})
		if !d.OK {
			rid := middleware.RequestIDFromContext(r.Context())
			slog.Warn("signed-url rejected",
				"code", d.Code, "kid", d.Kid, "aud", d.Aud, "cid", d.CID,
				"path", r.URL.Path, "request_id", rid)
			httputil.WriteError(w, http.StatusForbidden, d.Code, "invalid signature", rid)
			return
		}
		next.ServeHTTP(w, r.WithContext(storage.WithReadAuthz(r.Context())))
	})
}
