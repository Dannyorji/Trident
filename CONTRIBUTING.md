# Contributing to Trident

Trident is infrastructure — the kind of system other developers will build products on top of without thinking about it. That means the bar for what gets merged is higher than it would be for an application. Code here needs to be correct, readable, and maintainable by someone who didn't write it. Those three things, in that order.

---

## Before the Codebase Is Public

The most useful thing you can do right now is engage with the design while it can still change. Read the full specification at [`docs/SPECIFICATION.md`](./docs/SPECIFICATION.md) and open an issue if something looks wrong, underspecified, or like a decision that hasn't been thought through. Open a [Discussion](https://github.com/trident-build/trident/discussions) and describe what you're building on Stellar and what you'd need from an indexer. If you've built event pipelines or indexing infrastructure before, your architectural critique matters more right now than it will once the code exists.

---

## Getting Set Up

You'll need Rust, Go, Node.js, and Docker with Compose v2. Once those are in place, getting a local environment running is simple:

```bash
git clone https://github.com/trident-build/trident.git
cd trident
cp .env.example .env
make dev
```

This starts all dependencies and services locally. If you only want the infrastructure (Postgres and Redis) to run the services separately, run:

```bash
make db
make migrate
make indexer       # runs the indexer in a separate shell
make grpc-api     # runs the gRPC server in a separate shell
make go-api       # runs the Go REST API in a separate shell
```

### Secret scanning (pre-commit)

Install the pre-commit hook so gitleaks catches secrets before they reach the remote:

```bash
pip install pre-commit   # or: brew install pre-commit
pre-commit install
```

After that, every `git commit` automatically scans staged files. To run the scan manually:

```bash
pre-commit run --all-files
```

The `.gitleaks.toml` allowlist covers the intentionally committed CI test credentials in `.env.ci` — they are not real secrets and are documented as such in that file. Any other finding blocks the commit.

CI runs the same scan on every push and pull request targeting `dev` or `main` via `.github/workflows/security.yml`.

---

## How the Repo Is Structured

```
trident/
├── crates/
│   ├── indexer/        # Rust core — streamer, XDR parser, cursor management
│   ├── api/            # Rust gRPC server
│   └── common/         # Shared types, errors, config
├── services/
│   └── api/            # Go front office — REST, GraphQL, WebSocket, Redis consumer
├── sdk/
│   └── typescript/     # TypeScript SDK (@trident-indexer/sdk)
├── database/
│   ├── schema.sql      # Canonical PostgreSQL schema
│   └── migrations/     # Versioned, append-only migration files
├── docker/
└── docs/
```

The Rust crates own everything from the chain to storage. The Go service owns everything from storage to the developer. The TypeScript SDK is a pure HTTP and WebSocket client with no knowledge of the database and no direct connection to any internal service.

---

## Workflow

Before writing code for anything non-trivial, open an issue first and comment to claim it. This prevents duplicated effort and makes sure your approach aligns with where the project is heading before you invest time in it.

All branches come off `dev` — `main` is for tagged releases only. Name branches as `type/issue-number-short-description`, for example `feature/42-graphql-subscriptions` or `fix/87-cursor-recovery`.

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org) with type, scope, and an imperative description:

```
feat(indexer): add cursor recovery on restart
fix(api): return 404 when event id not found
docs(sdk): add subscribeToContract example
chore(deps): update tokio to 1.35
```

Valid types are `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, and `chore`. Valid scopes are `indexer`, `api`, `sdk`, `db`, `docker`, and `docs`. Reference the issue in the commit footer with `Closes #NNN`.

---

## Pull Requests

A PR that moves through review quickly explains *why* the change exists, not just what it does. It has tests covering the new behaviour — for bug fixes specifically, a regression test that would have caught the original bug. CI is green before review is requested. It changes one thing.

A PR that comes back for revision is one that mixes concerns, doesn't explain the reasoning, or changes a public interface without prior discussion. One concern per PR is a firm rule regardless of how small the changes are.

---

## Code Standards

On the Rust side, `cargo clippy` must pass clean and `cargo fmt` must produce no diff — both enforced in CI with no exceptions. `.unwrap()` is not allowed in non-test code because a panic in the streaming loop means missed events, and missed events are the one thing Trident cannot tolerate. New error variants go in `crates/common`. Public functions and types get doc comments.

On the Go side, `golangci-lint` must pass. Errors are returned and never ignored. Every function doing I/O takes a `context.Context` as its first argument. Error messages returned to developers need to be genuinely useful — not "internal server error" but something that tells them exactly what was wrong and how to fix it.

On the TypeScript side, strict mode is on and `any` is not allowed. The SDK's public interface deserves particular care because renaming anything after v1 is a breaking change requiring a major version bump and migration docs. ESLint and Prettier must be clean.

SQL migrations are append-only — a migration committed to `main` is never edited, only superseded by a new numbered one. Index names follow `idx_<table>_<columns>`. Production queries always name columns explicitly.

---

## Good First Issues

Once development starts, we'll tag approachable issues with `good first issue`. These tend to be things like adding a missing filter parameter to an endpoint, writing tests for an existing parser function, improving an error message with more context, or documenting an undocumented function. They're scoped to help you understand the system without needing to know all of it upfront.

For more substantial work — changes to the indexer core, query performance improvements, new API capabilities — open a Discussion with a concrete use case before writing any code. The indexer's streaming and cursor logic in particular warrants a conversation before anyone touches it.

---

## Security

Security issues must not be filed as public GitHub issues. Send them to `security@trident.build` and expect a response within 48 hours.

---

## Getting Help

[GitHub Discussions](https://github.com/trident-build/trident/discussions) is the right place for questions, ideas, and design conversations before an issue is opened. [GitHub Issues](https://github.com/trident-build/trident/issues) is for confirmed bugs and concrete feature requests. For anything that shouldn't be public, reach out at `contributors@trident.build`.

---

*Trident is infrastructure for the whole Stellar ecosystem. Getting it right matters. Thanks for helping.*>>>>>>> 7e309c3 (chore(db): integrate sqlx-cli database migration management)