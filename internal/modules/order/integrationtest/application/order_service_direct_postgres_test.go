//go:build integration

package application_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	databasemigrations "github.com/dujiao-next/internal/bootstrap/database/migrations"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	kafudadomain "github.com/dujiao-next/internal/modules/kafudagpt/domain"
	kafudastore "github.com/dujiao-next/internal/modules/kafudagpt/infrastructure/gormstore"
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

func TestPostgresConfirmOverCapDirectCompletion(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	testConfirmOverCapDirectCompletion(t, func(t *testing.T) directCheckoutFixture {
		db := openPostgresDirectRaceDB(t, dsn)
		return seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-over-cap", 0, 0))
	})
}

func TestPostgresDirectSubmitCheckedQuote(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	testSubmitCheckedDirectQuote(t, func(t *testing.T) directCheckoutFixture {
		db := openPostgresDirectRaceDB(t, dsn)
		return seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-quote", 0, 0))
	})
}

func TestPostgresDirectAfterSalesWithSalesStopped(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	fixture := func(t *testing.T) directCheckoutFixture {
		db := openPostgresDirectRaceDB(t, dsn)
		return seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-after-sales", 0, 0))
	}
	t.Run("refund", func(t *testing.T) { testDirectCheckoutRefunds(t, fixture) })
	for _, action := range []string{"direct-complete", "direct-release"} {
		t.Run(action, func(t *testing.T) { testControlledDirectManualAction(t, fixture(t), action, false) })
	}
}

func TestPostgresAdminDirectReviewFilterAndProjection(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	testAdminDirectReviewFilterAndProjection(t, seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-review", 0, 0)))
}

func TestPostgresDirectCheckoutDiscountsMatchPreviewAndSnapshot(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	testDirectCheckoutDiscountsMatchPreviewAndSnapshot(t, func(t *testing.T) directCheckoutFixture {
		db := openPostgresDirectRaceDB(t, dsn)
		return seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-price", 0, 0))
	})
}

func TestPostgresDirectCheckoutPaymentsPreserveRechargeSnapshot(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	testDirectCheckoutPaymentsPreserveRechargeSnapshot(t, func(t *testing.T) directCheckoutFixture {
		db := openPostgresDirectRaceDB(t, dsn)
		return seedDirectCheckoutFixture(t, seedCouponLimitRaceFixture(t, db, "direct-payment", 0, 0))
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

	if err := databasemigrations.AutoMigrate(); err != nil {
		t.Fatalf("initial central migration: %v", err)
	}
	const costReason = kafudadomain.ReasonCostLimitExceeded
	charge := int64(3000)
	for i, status := range []kafudadomain.AttemptStatus{kafudadomain.AttemptFailed, kafudadomain.AttemptCompleted} {
		orderID := uint(i + 1)
		upstreamID := uint64(orderID)
		attempt := kafudadomain.Attempt{
			OrderID: orderID, Sequence: 1, Status: status, Reference: string(status),
			UpstreamOrderID: &upstreamID, ChargedUSDCents: &charge,
		}
		if err := db.Create(&attempt).Error; err != nil {
			t.Fatal(err)
		}
		record := kafudadomain.Recharge{
			OrderID: orderID, CurrentAttemptID: &attempt.ID, AttemptCount: 1, Version: 1,
			Stage: kafudadomain.StageManualReview, ReasonCode: costReason,
			Snapshot: kafudadomain.Snapshot{MaxCostCents: 2500},
		}
		if err := db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrator().DropColumn(&kafudadomain.Attempt{}, "QuoteCosts"); err != nil {
		t.Fatalf("prepare attempts from before quote capture: %v", err)
	}
	for range 2 {
		if err := databasemigrations.AutoMigrate(); err != nil {
			t.Fatalf("central migration: %v", err)
		}
	}
	for _, want := range []struct {
		orderID uint
		stage   kafudadomain.Stage
		version uint64
	}{
		{1, kafudadomain.StageManualDelivery, 2},
		{2, kafudadomain.StageManualReview, 1},
	} {
		var record kafudadomain.Recharge
		if err := db.First(&record, "order_id = ?", want.orderID).Error; err != nil {
			t.Fatal(err)
		}
		if record.Stage != want.stage || record.Version != want.version || record.ReasonCode != costReason {
			t.Fatalf("order %d after backfill: stage=%s version=%d reason=%s", want.orderID, record.Stage, record.Version, record.ReasonCode)
		}
		var attempt kafudadomain.Attempt
		if err := db.First(&attempt, "order_id = ?", want.orderID).Error; err != nil {
			t.Fatal(err)
		}
		if attempt.QuoteCosts != nil || attempt.ChargedUSDCents == nil || *attempt.ChargedUSDCents != charge {
			t.Fatalf("migration changed legacy amounts for order %d: %+v", want.orderID, attempt)
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
	if !migrator.HasColumn(&kafudadomain.Attempt{}, "QuoteCosts") {
		t.Error("missing attempt quote cost snapshot column")
	}
}

func TestPostgresDirectCompletionEmailClaim(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is empty")
	}
	db := openPostgresDirectRaceDB(t, dsn)
	if err := db.AutoMigrate(&kafudadomain.Recharge{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, orderID := range []uint{10, 20} {
		recharge := kafudadomain.Recharge{
			OrderID: orderID, Stage: kafudadomain.StageCompleted,
			CompletionEmailPending: true, CompletedAt: &now, Version: 1,
		}
		if err := db.Create(&recharge).Error; err != nil {
			t.Fatal(err)
		}
	}

	store := kafudastore.New(db)
	ctx := t.Context()
	retryAt := now.Add(time.Minute)
	const workers = 4
	start := make(chan struct{})
	type result struct {
		claimed bool
		err     error
	}
	results := make(chan result, workers)
	for range workers {
		go func() {
			<-start
			claimed, err := store.ClaimCompletionEmail(ctx, 10, now, retryAt)
			results <- result{claimed: claimed, err: err}
		}()
	}
	close(start)
	claims := 0
	for range workers {
		outcome := <-results
		if outcome.err != nil {
			t.Error(outcome.err)
		}
		if outcome.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("successful claims = %d, want 1", claims)
	}

	checkPending := func(at time.Time, want []uint) {
		t.Helper()
		ids, err := store.PendingCompletionEmails(ctx, at, 2)
		if err != nil || !slices.Equal(ids, want) {
			t.Fatalf("pending emails at %v = %v, %v; want %v", at, ids, err, want)
		}
	}
	checkPending(now, []uint{20})
	checkPending(retryAt, []uint{20, 10})
	if err := store.ClearCompletionEmail(ctx, 10); err != nil {
		t.Fatal(err)
	}
	checkPending(retryAt, []uint{20})
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
