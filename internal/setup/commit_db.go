package setup

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/db/gen"
)

// DBUserCreator creates the operator via the existing CreateUser query + argon2id.
type DBUserCreator struct{ Q *gen.Queries }

func (d DBUserCreator) CreateOperator(ctx context.Context, email, plain string) error {
	hash, err := password.Hash(plain)
	if err != nil {
		return err
	}
	_, err = d.Q.CreateUser(ctx, gen.CreateUserParams{
		Email:        email,
		Role:         gen.UserRoleOperator,
		PasswordHash: pgtype.Text{String: hash, Valid: true},
	})
	return err
}
