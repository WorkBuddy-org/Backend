package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/pressly/goose/v3"
	"github.com/workbuddy/backend/internal/config"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		slog.Error("failed to set goose dialect", "error", err)
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	if err := runCommand(command, db, args); err != nil {
		slog.Error("migration failed", "command", command, "error", err)
		os.Exit(1)
	}
}

func runCommand(command string, db *sql.DB, args []string) error {
	switch command {
	case "up":
		return goose.Up(db, "migrations")
	case "down":
		return goose.Down(db, "migrations")
	case "status":
		return goose.Status(db, "migrations")
	case "version":
		version, err := goose.GetDBVersion(db)
		if err != nil {
			return err
		}
		fmt.Println(version)
		return nil
	default:
		printUsage()
		os.Exit(1)
		return nil
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: migrate [up|down|status|version]")
}
