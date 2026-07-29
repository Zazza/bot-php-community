// Package db подключает Postgres и применяет встроенные миграции.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"

	// Регистрирует pgx как driver для database/sql под именем "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Connect открывает пул соединений (database/sql + pgx driver), применяет миграции.
func Connect(ctx context.Context, dbURL string) (*sqlx.DB, error) {
	sqldb, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	dbx := sqlx.NewDb(sqldb, "pgx")
	if err := migrate(ctx, dbx); err != nil {
		dbx.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	slog.Info("db connected, migrations applied")
	return dbx, nil
}

// migrate применяет SQL-файлы из embed.FS по порядку имени. Каждая миграция
// идемпотентна (IF NOT EXISTS), поэтому отдельная схема истории не нужна.
func migrate(ctx context.Context, db *sqlx.DB) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx, string(b)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		slog.Info("migration applied", "file", name)
	}
	return nil
}
