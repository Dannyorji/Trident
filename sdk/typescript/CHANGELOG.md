# Changelog

All notable changes to `@trident-indexer/sdk` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Versioning policy

Given a version `MAJOR.MINOR.PATCH`:

- **MAJOR** — breaking changes: removed/renamed exports, changed method
  signatures, altered runtime behavior existing callers depend on.
- **MINOR** — backwards-compatible additions: new exports, new optional
  params, new methods.
- **PATCH** — backwards-compatible bug fixes and internal changes with no
  API surface impact.

## Release process

1. Land changes on `dev` via the normal PR flow.
2. Add an entry under `[Unreleased]` in this file describing the change
   (Added / Changed / Fixed / Removed), following Keep a Changelog sections.
3. When ready to publish, trigger the **Release** GitHub Actions workflow
   (`.github/workflows/release.yml`) manually via `workflow_dispatch`,
   supplying the target semver version. The workflow runs lint + tests,
   bumps `package.json`, builds, smoke-tests the packed tarball, and
   publishes to npm with provenance.
4. Move the `[Unreleased]` entries into a new dated version section matching
   the version published.

## [Unreleased]

- No unreleased changes.
