package database

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/sijms/go-ora/v2"
)

func NewOracleDB(dsn string) (*sql.DB, error) {
	if dsn == "" {
		dsn = os.Getenv("CONN_STRING")
	}
	if dsn == "" {
		return nil, fmt.Errorf("DB connection string is required")
	}

	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, fmt.Errorf("open oracle db: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping oracle db: %w", err)
	}

	return db, nil
}
