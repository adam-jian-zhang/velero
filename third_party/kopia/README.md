# Vendored Kopia `wcmatch`

This directory contains a copy of Kopia's gitignore-style wildcard matcher,
originally from `github.com/kopia/kopia/internal/wcmatch` (as used by the
`project-velero/kopia` fork that Velero depends on).

## Why it lives here

Velero validates `excludeFiles` volume-policy patterns with the **same** parser
Kopia uses at backup time, so validation and runtime stay in sync. Kopia keeps
`wcmatch` under `internal/`, which Go will not let Velero import across module
boundaries. Until that package is promoted upstream (or in the Velero fork),
this copy is the supported path.

## Source pin

Copied from `project-velero/kopia` at the commit pinned in Velero's `go.mod`
replace directive for `github.com/kopia/kopia`
(`v0.0.0-20260616052725-d83462d382c9` at the time of the initial vendor).

Files:
- `wcmatch.go`
- `tokens.go`
- `rune_scanner.go`
- `wcmatch_test.go`

## Maintenance

When bumping the Kopia dependency, re-sync these files from the fork's
`internal/wcmatch` (or the promoted public path if that lands) and re-run:

```bash
go test ./third_party/kopia/wcmatch/... ./internal/resourcepolicies/
```

Do not diverge the algorithm here for Velero-specific behavior; change callers
instead.
