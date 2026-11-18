//go:build integration

package application_test

import (
	"os"
	"strings"
	"testing"

	databasemigrations "github.com/dujiao-next/internal/bootstrap/database/migrations"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	"github.com/dujiao-next/internal/platform/database/gormdb"

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

func TestPostgresDirectCentralMigration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	previousDB := gormdb.DB
	gormdb.DB = db
	t.Cleanup(func() { gormdb.DB = previousDB })

	for range 2 {
		if err := databasemigrations.AutoMigrate(); err != nil {
			t.Fatalf("central migration: %v", err)
		}
	}
	migrator := db.Migrator()
	for _, model := range []any{&kafudadomain.Catalog{}, &kafudadomain.Recharge{}, &kafudadomain.Attempt{}} {
		if !migrator.HasTable(model) {
			t.Fatalf("missing direct-recharge table for %T", model)
		}
		for _, field := range []string{"session", "session_token", "access_token", "api_key", "account_email"} {
			if migrator.HasColumn(model, field) {
				t.Errorf("unexpected credential or account field %s in %T", field, model)
			}
		}
	}
	if !migrator.HasColumn(&productdomain.ProductSKU{}, "DirectCostConfigured") ||
		!migrator.HasColumn(&productdomain.ProductSKU{}, "DirectCatalogRevision") ||
		!migrator.HasColumn(&kafudadomain.Catalog{}, "AvailabilityRevisions") {
		t.Error("missing direct SKU price or catalog review metadata")
	}
	if !migrator.HasIndex(&kafudadomain.Attempt{}, "idx_kafuda_gpt_active_order") ||
		!migrator.HasConstraint(&kafudadomain.Attempt{}, "chk_kafuda_gpt_active_attempt") {
		t.Error("missing active attempt index or constraint")
	}
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
