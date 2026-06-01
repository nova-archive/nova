-- +goose Up
-- +goose StatementBegin
-- Migration 0007: refresh-token GC partial index for revoked-but-old rows.
-- Pairs with the M6.2 (C1) split of DeleteExpiredRefreshTokens into two
-- queries — one for expired-but-active rows (uses the existing
-- refresh_tokens_gc_idx from 0006), one for revoked rows past their grace
-- window (uses this new index).

CREATE INDEX refresh_tokens_revoked_gc_idx
    ON refresh_tokens (revoked_at)
    WHERE revoked_at IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX refresh_tokens_revoked_gc_idx;
-- +goose StatementEnd
