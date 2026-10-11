package integration

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/postgres"
	"crypto-payment-gateway/migrations"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const currentMigrationVersion = 2

func TestMigrationsRoundTrip(t *testing.T) {
	pool := databasePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	assertState := func(version int64, foundation, processing bool) {
		t.Helper()
		got, err := provider.GetDBVersion(ctx)
		if err != nil || got != version {
			t.Fatalf("version = %d, want %d: %v", got, version, err)
		}
		var hasInvoices, hasRefunds bool
		err = pool.QueryRow(ctx, `SELECT to_regclass('invoices') IS NOT NULL,
			to_regclass('refunds') IS NOT NULL`).Scan(&hasInvoices, &hasRefunds)
		if err != nil || hasInvoices != foundation || hasRefunds != processing {
			t.Fatalf("schema state = (%v, %v): %v", hasInvoices, hasRefunds, err)
		}
		var triggers, functions, sealedColumns int
		err = pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_trigger t
			 JOIN pg_class c ON c.oid = t.tgrelid
			 JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE NOT t.tgisinternal AND n.nspname = current_schema()),
			(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = current_schema() AND p.prorettype = 'trigger'::regtype),
			(SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = 'ledger_operations' AND column_name = 'sealed')
		`).Scan(&triggers, &functions, &sealedColumns)
		if err != nil || triggers != 0 || functions != 0 || sealedColumns != 0 {
			t.Fatalf("obsolete ledger schema = (%d triggers, %d functions, %d sealed columns): %v", triggers, functions, sealedColumns, err)
		}
	}
	assertState(currentMigrationVersion, true, true)
	for _, version := range []int64{2, 1} {
		result, err := provider.Down(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if result.Source.Version != version {
			t.Fatalf("rolled back version %d, want %d", result.Source.Version, version)
		}
		assertState(version-1, version > 1, version > 2)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	assertState(currentMigrationVersion, true, true)
	results, err := provider.Up(ctx)
	if err != nil || len(results) != 0 {
		t.Fatalf("repeated Up applied %d migrations: %v", len(results), err)
	}
}

func TestMigrationsConcurrent(t *testing.T) {
	pool := databasePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const workers = 4
	start := make(chan struct{})
	results := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			results <- postgres.Migrate(ctx, pool)
		}()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	var first, second int
	err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE version_id=1),
		count(*) FILTER (WHERE version_id=2)
		FROM goose_db_version WHERE version_id > 0 AND is_applied`).Scan(&first, &second)
	if err != nil || first != 1 || second != 1 {
		t.Fatalf("applied versions counts = (%d, %d): %v", first, second, err)
	}
	var legacy bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&legacy); err != nil || legacy {
		t.Fatalf("legacy migration table exists = %v: %v", legacy, err)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	pool := databasePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	source := fstest.MapFS{
		"001_probe.sql": {Data: []byte("-- +goose Up\nCREATE TABLE migration_probe (id INTEGER);\nSELECT 1/0;\n")},
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err == nil {
		t.Fatal("expected migration failure")
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil || version != 0 {
		t.Fatalf("failed migration advanced version to %d: %v", version, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('migration_probe') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration left its table = %v: %v", exists, err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrations could not resume after rollback: %v", err)
	}
}

func TestRepeatedMigrationsPreserveJournal(t *testing.T) {
	pool := databasePool(t)
	ctx := context.Background()
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	store := postgres.New(pool)
	if err := store.Seed(ctx, alphaKey, betaKey); err != nil {
		t.Fatal(err)
	}
	s := &databaseFixture{Store: store, pool: pool}
	v, a := fundedInvoice(t, s, 100)
	entries := []ledger.Entry{
		{AccountID: a[ledger.Hold], Side: ledger.Debit, Amount: 100},
		{AccountID: a[ledger.PartnerAvailable], Side: ledger.Credit, Amount: 100},
	}
	id, err := store.PostLedger(ctx, "settlement-before-repeat", entries)
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	if err := pool.QueryRow(ctx, `SELECT fingerprint FROM ledger_operations WHERE id=$1`, id).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []func(context.Context) error{
		func(ctx context.Context) error { _, err := provider.Up(ctx); return err },
		func(ctx context.Context) error { return postgres.Migrate(ctx, pool) },
	} {
		if err := apply(ctx); err != nil {
			t.Fatal(err)
		}
		if replayed, err := store.PostLedger(ctx, "settlement-before-repeat", entries); err != nil || replayed != id {
			t.Fatal("migration changed persisted idempotency", replayed, err)
		}
		var stored string
		if err := pool.QueryRow(ctx, `SELECT fingerprint FROM ledger_operations WHERE id=$1`, id).Scan(&stored); err != nil || stored != fingerprint {
			t.Fatal("migration changed the fingerprint", err)
		}
		if got, err := store.Balance(ctx, postgres.AlphaID); err != nil || got.Available != 100 {
			t.Fatal("migration changed ledger balances", got, err)
		}
		if got, err := store.GetInvoice(ctx, postgres.AlphaID, v.ID); err != nil || got.ID != v.ID {
			t.Fatal("migration lost the invoice", err)
		}
	}
}
