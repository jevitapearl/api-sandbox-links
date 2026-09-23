package db

import (
	"fmt"

	migrate "github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/api-sandbox-links/backend/migrations"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Open connects to Postgres and returns a ready *gorm.DB. It does NOT create
// schema — call Migrate on the returned handle during startup.
func Open(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		// Console still shows a query on failure; discard the noise on success.
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("db: connecting to postgres: %w", err)
	}
	return db, nil
}

// Migrate applies the embedded, numbered SQL migration files to the database
// using golang-migrate. It is idempotent (tracks applied versions in a
// schema_migrations table) and returns an error if any migration fails.
func Migrate(db *gorm.DB) error {
	// golang-migrate works against *sql.DB, which gorm holds underneath.
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("db: getting underlying sql.DB: %w", err)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("db: loading embedded migrations: %w", err)
	}
	driver, err := migratepostgres.WithInstance(sqlDB, &migratepostgres.Config{})
	if err != nil {
		return fmt.Errorf("db: preparing migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return fmt.Errorf("db: initializing migrator: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("db: applying migrations: %w", err)
	}
	return nil
}