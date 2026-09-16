package main

import (
	"database/sql"
	"fmt"
	"os"

	authallstore "github.com/alternayte/auth-all/store"
	"github.com/alternayte/auth-all/store/postgres"
	"github.com/alternayte/auth-all/store/sqlite"
)

// sqlDB is the gateway's own store. SQLite fits one gateway on one box, and
// Postgres fits a hosted service that runs more than one.
type sqlDB struct {
	*sql.DB
	Driver string
}

// openStore takes Postgres when KILN_DATABASE_URL names one, and SQLite
// otherwise. KILN_DB names the SQLite file.
func openStore() (*sqlDB, error) {
	if dsn := os.Getenv("KILN_DATABASE_URL"); dsn != "" {
		db, err := postgres.Open(dsn)
		if err != nil {
			return nil, fmt.Errorf("gateway: postgres: %w", err)
		}
		return &sqlDB{DB: db, Driver: "postgres"}, nil
	}
	path := os.Getenv("KILN_DB")
	if path == "" {
		path = "file:kiln-gateway.db"
	}
	db, err := sqlite.Open(path)
	if err != nil {
		return nil, fmt.Errorf("gateway: sqlite %s: %w", path, err)
	}
	return &sqlDB{DB: db, Driver: "sqlite"}, nil
}

// authStore returns the auth-all store for this handle.
func (d *sqlDB) authStore() authallstore.Store {
	if d.Driver == "postgres" {
		return postgres.New(d.DB)
	}
	return sqlite.New(d.DB)
}
