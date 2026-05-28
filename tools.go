//go:build tools

// Package tools imports the Nova project's build-time-only
// dependencies (sqlc, testcontainers, etc.) so that `go mod tidy`
// does not prune them when no source file in the main module yet
// imports them. This is the canonical Go pattern for projects that
// pin a dependency surface before all callers exist.
//
// The build constraint above means this file is never compiled into
// any binary; it exists purely to anchor entries in go.mod.
//
// As actual source code in internal/, pkg/, cmd/, and nova-image/
// adopts each dependency (via real imports in production or test
// code), the corresponding blank import here can be removed without
// changing go.mod.
package tools

import (
	_ "github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/pressly/goose/v3"
	_ "github.com/stretchr/testify/assert"
	_ "github.com/testcontainers/testcontainers-go"
	_ "github.com/testcontainers/testcontainers-go/modules/postgres"
	_ "gopkg.in/yaml.v3"
)
