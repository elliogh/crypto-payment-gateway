# Working on This Project

This is a Senior Backend Engineer (Go) take-home assignment: design a USDT payment service and implement a simplified payment flow. Use Russian for communication and project documentation; this AGENTS.md is written in English.

## Current State and Boundaries

- Part 1 is complete: `DESIGN.md` and `output/pdf/usdt-payment-design.pdf`. Modify them when the task explicitly concerns the design.
- Part 2 is implemented and stages 1–4 are complete: API, PostgreSQL, money/ledger, simulator/scanner, AML processing, settlement/refunds, and the final delivery audit. Clean Docker startup, README scenarios and real-process crash recovery have been verified.
- Keep changes within the assignment's simplified flow unless the user requests an extension. The production architecture in the design does not require implementing every component; outbox and webhooks are outside the current Part 2 flow.

## Documents to Read

- Before changing behavior or reviewing requirements, read `README.md`: it describes the actual interface and implementation limitations.
- Before implementing a stage, read `plans/part-2/00-overview.md` and the corresponding stage file. The overview records assumptions; each stage file defines the work and completion criteria.
- For questions about the original assignment, read `TASK.md`; for production decisions and ADRs, read `DESIGN.md`.
- `TASK.md` and `plans/` are intentionally excluded from Git. They may be missing in a fresh checkout: use the available documents and task context, and request missing requirements when necessary. Their absence does not authorize inventing the next stage.

## Structure and Change Style

- Domain rules belong in `partner`, `invoice`, `payment`, `refund`, `aml`, `ledger`, and `money`; HTTP uses its own DTOs; the PostgreSQL adapter owns SQL and transactions. Startup and dependency wiring belong in `app`, with the entry point in `cmd/gateway`.
- `chain` owns simulator/source interfaces, USDT event validation, and scanning; `payment` describes the current observation. Source primitives change blocks/events and the cursor; the local Reorg command coordinates source rollback with holds, jobs and observation versions in one root LedgerTx. The scanner owns invoice matching, persistent transfer bindings, and observation versions; it reconciles rollbacks even with no new blocks. GET confirmations also check block canonicality.
- Lock `chain_state` first, then invoices and payments in UUID order, before ledger accounts. Read the head in a fresh statement after acquiring the chain lock. Apply batches only when both the chain version and starting cursor match; record payments and cursor atomically. Reorg rejects removal of deposits with committed credit or refund decisions. AML completion rechecks the current observation, block canonicality, confirmations and job ownership under these locks.
- Keep interfaces small and focused on complete operations. Prefer targeted changes; introduce a shared package or wrapper when it hides real rules or complexity rather than merely renaming a library.
- Generate UUID v4 values with `github.com/google/uuid.NewRandom()` and handle errors. The custom `identity` package has been removed. HTTP accepts the standard 36-character UUID form in either case; the ledger requires canonical lowercase.
- Errors preserve their causes for `errors.Is/As`. Diagnostics expose the operation, error type, and SQLSTATE; SQL error messages, parameters, keys, and connection URLs must stay out of logs and HTTP responses.
- README describes current behavior; keep the history of intermediate decisions in the local plan. After a change, update affected documents and remove outdated statements.

## Money and Financial Transactions

- USDT uses `int64` in microUSDT; domain amounts in PostgreSQL use `BIGINT`. The ledger entry amount column uses `NUMERIC(78,0)` for consistency with ADR 7; the USDT range is limited to `int64` in Go and by a database CHECK. The current code does not account for ETH.
- The fee is `floor(gross × bps / 10000)`, using `big.Int` for intermediate multiplication. HTTP accepts and returns amounts as strings; serialization belongs in `httpapi`.
- Post entries through `PostLedger` / `PostLedgerTx`. To change statuses and post entries together, obtain a root `LedgerTx` through `BeginLedgerTx`; it hides the underlying `pgx.Tx`, `Begin`, and `Conn`. On success, the caller commits.
- Any `PostLedgerTx` error rolls back the entire root transaction, including earlier status changes and successful postings. Rollback uses a context independent of cancellation, has a 5-second timeout, and preserves the original error cause. An SQL savepoint must not limit this rollback.
- Posting requires `READ COMMITTED`: lock accounts in UUID order, then read balances in a separate query after acquiring the locks. Validate the result of the entire operation rather than the intermediate balance after an individual entry.
- An operation contains at least two positive entries with equal debit and credit totals. Each affected account's balance on its normal side must remain within `0..MaxInt64`. Failure of one operation must not stop processing the others.
- The business key prevents duplicates: the same fingerprint returns the existing operation; different content produces a conflict. Existing journal records and accounts' financial attributes are not rewritten; corrections use new operations.
- AML calls run outside SQL transactions. Job claims use SKIP LOCKED and a lease/version; stale or expired owners cannot apply results. Hold/cancel keys include payment ID and observation version; the final credit key uses payment ID alone. Provider or balance failures keep the hold with a bounded retry and a safe error code, allowing other jobs to proceed.
- Refund READY means an outstanding obligation, not an asset withdrawal. Unsafe local return routes or missing automatic authorization produce HELD. Explicit local approval supplies a confirmed recipient and changes only the refund route/status, with no new postings.
- The fingerprint format is a persisted contract. Preserve canonicalization, fixed encoding, and the golden test with UUIDs when changing models or DTOs.

## Migrations and Data

- Goose SQL files live in the root `migrations/` directory, are embedded in the binary, and are applied before the API starts. The current history consists of 001–003; add new migrations for future changes.
- Custom triggers, trigger functions, and `sealed` have been removed. Go enforces financial rules; the database retains PK, FK, UNIQUE, NOT NULL, and CHECK constraints. Direct administrative SQL can bypass Go rules; this is an accepted limitation, and no additional database roles have been introduced.
- Preserve the Docker volume and existing data during normal updates. A previously approved cleanup of the demo database does not authorize another reset.
- Seed rotates demo partners' keys atomically, including swaps. Serialize initialization before freeing existing hashes; preserve partner settings and financial data, and roll back both key changes on a conflict with another partner.

## Validating Changes

Startup commands and the manual scenario are in README. For Go changes, run `gofmt` and appropriate tests; validate financial and SQL changes against real PostgreSQL. Without `TEST_DATABASE_URL`, integration tests are skipped; such a run does not verify SQL guarantees.

```bash
go test ./...
go vet ./...
TEST_DATABASE_URL='postgres://gateway:local-demo-password@127.0.0.1:54329/gateway?sslmode=disable' \
  go test -race -count=1 ./...
```

The URL above uses the demo Compose settings; use the actual connection details if your settings differ. Integration tests create separate temporary schemas and delete only those schemas.

- For financial changes, check idempotency, concurrency, monetary bounds, and rollback of statuses together with the journal. Test races through synchronization or lock observation rather than arbitrary delays.
- For migration changes, check `Up` on a clean database, concurrent `Up`, repeated application, `Down`, and rollback of a failing migration.
- When changing startup or Docker, build the image and check the API, repeated invoice creation, and data preservation after updating the container. UUID or DTO changes must preserve the HTTP contract and fingerprint.
- After checks pass, repeat them when new changes or a discovered issue require further validation. Documentation-only changes do not require Go tests.

## Git and Task Scope

Check `git status` at the start of work: some implementation files may not be committed yet. Preserve the user's existing changes. Create commits and publish changes only when separately instructed. For a review task, limit the work to inspection and findings; apply fixes when requested by the user.
