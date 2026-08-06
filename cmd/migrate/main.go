// Command migrate applies AutoDeploy's embedded forward-only PostgreSQL migrations.
package main

import (
	"context"
	"log"

	"autodeploy/internal/config"
	"autodeploy/migrations"

	"github.com/jackc/pgx/v5"
)

func main() {
	dsn, err := config.DatabaseURLFromEnvironment()
	if err != nil {
		log.Fatal("load database credential: invalid configuration")
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		log.Fatal("connect database")
	}
	defer conn.Close(context.Background())
	if err := migrations.Apply(context.Background(), conn); err != nil {
		log.Fatalf("apply migrations: %v", err)
	}
}
