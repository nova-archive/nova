-- name: TryFireSuppression :one
-- Atomic once-per-window gate for a scoped webhook event (D-M5-9a). Returns a row
-- (fired) iff no row existed or the window has elapsed since last_fired_at, recording
-- the new fire time in the same statement; returns NO row (ErrNoRows ⇒ suppressed)
-- when still inside the window. window_seconds=0 always fires (e.g. node_revoked,
-- which is deduped upstream by revoked_signaled_at). Durable, so once-per-window
-- survives a coordinator restart.
INSERT INTO webhook_suppression (event_type, destination, scope_key, last_fired_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (event_type, destination, scope_key) DO UPDATE
  SET last_fired_at = now()
  WHERE webhook_suppression.last_fired_at < now() - make_interval(secs => sqlc.arg(window_seconds)::int)
RETURNING event_type;
