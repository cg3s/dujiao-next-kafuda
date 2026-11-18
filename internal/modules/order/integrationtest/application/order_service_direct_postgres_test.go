//go:build integration

package application_test

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPostgresDirectAfterSalesCompetingResolutions(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	testDirectAfterSalesCompetingResolutions(t, func(t *testing.T) directCheckoutFixture {
		db := openPostgresDirectRaceDB(t, dsn)
		return seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-checkout", 0, 0))
	})
}

func openPostgresDirectRaceDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid PostgreSQL test DSN")
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = admin.Close() })

	schema := pgx.Identifier{"kafuda_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema.Sanitize()); err != nil {
		t.Fatalf("create PostgreSQL test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + schema.Sanitize() + " CASCADE"); err != nil {
			t.Errorf("drop PostgreSQL test schema: %v", err)
		}
	})

	// Set search_path on every connection so competing transactions use the
	// same isolated schema while retaining independent PostgreSQL connections.
	scopedConfig := config.Copy()
	scopedConfig.RuntimeParams["search_path"] = schema[0]
	conn := stdlib.OpenDB(*scopedConfig)
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(4)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: conn}), &gorm.Config{})
	if err != nil {
		t.Fatalf("open PostgreSQL test schema: %v", err)
	}
	return db
}
