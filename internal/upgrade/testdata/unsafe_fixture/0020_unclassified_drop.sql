-- A COMMITTED FIXTURE. This migration is never applied (P2-M7.3, T18).
--
-- It exists so the refusal path can be proved against a real migration set
-- rather than a temporary edit to the source tree. A test that mutates
-- production files to make itself fail proves that the test can edit files.
--
-- What makes it unsafe is not the SQL. It is that this file has no entry in
-- internal/db/migrations/obligations.go, so nothing in the system can say what
-- applying it commits the operator to: whether the previous binary survives it,
-- whether it can run online, or whether going back needs a restore. `migrate
-- apply` refuses an unclassified migration for exactly that reason, and this
-- file is what proves the refusal fires.
--
-- The statement below is destructive on purpose. If the refusal ever regresses,
-- the test that applies this fixture will lose a table, and a test that fails
-- loudly is better than one that quietly starts applying unclassified
-- migrations.

-- +goose Up
-- +goose StatementBegin
DROP TABLE IF EXISTS upgrade_events;
DROP TABLE IF EXISTS upgrade_runs;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
