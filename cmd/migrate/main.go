// Command migrate applies AutoDeploy's embedded forward-only PostgreSQL migrations.
package main

import (
	"context"
	"log"
	"os"

	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn := os.Getenv("AUTODEPLOY_DATABASE_URL")
	if dsn == "" {
		log.Fatal("AUTODEPLOY_DATABASE_URL is required")
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer conn.Close(context.Background())
	if err := migrations.Apply(context.Background(), conn); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}
}
