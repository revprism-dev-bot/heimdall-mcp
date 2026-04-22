# schema/testdata

Reserved for static fixtures used by future per-migration integration
tests. The current migration-001 integration test (`schema_test.go`)
seeds its legacy `<model>_latest/` directory programmatically into
`t.TempDir()` rather than checking a binary SQLite file into git —
checked-in `vectors.db` files are fragile across SQLite versions and
schema rev-ups.

If a future migration needs a static pre-state that the production
OpenStore code path cannot construct (for example: a column drop where
re-seeding via the modern code path would never reproduce the legacy
shape), check the fixture in here under
`testdata/<migration-version>/<short-description>/` and document the
construction recipe in a sibling README.

This file is itself the placeholder that ensures `testdata/` ships in
the tree (git ignores empty dirs).
