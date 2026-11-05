# PackCheck — Security report (Go best practices)

Date: 2026-11-05. Scope: the whole `packcheck` module (Go CLI + library, embedded SQLite, no network surface).
Tools executed: `go vet`, `staticcheck`, `gosec`, `govulncheck`, `go test -race`, plus a manual review against Go backend security guidance (stdlib-oriented).

## Executive summary

No open findings of high or medium severity remain. Initial `gosec` output reported 8 warnings (G115×4, G204×1, G304×3); all were fixed by construction or accepted with a written justification in code and a documented trust boundary. Final verified state: `gosec` = 0 issues, `staticcheck` clean, `govulncheck` = "No vulnerabilities found", race detector clean. The system surface is local by design (CLI, no server): there is no HTTP, cookies, CORS, CSRF, or secret management; those web rule families are N/A here, declared as such rather than claimed as "compliant".

## Findings

### SEC-01 — G115 int-to-uint8 conversion in blur averaging (gosec: high; real risk: low) — FIXED

- Original location: [labelgen.go](internal/labelgen/labelgen.go) in `blurPass` (`uint8(sr / win)`).
- Potential impact: an overflow would silently corrupt pixel values in evidence images.
- Fix: an `avgByte(sum, n)` helper at [labelgen.go:348](internal/labelgen/labelgen.go:348) with an explicit [0,255] bound before converting; all three channels use it at [labelgen.go:286](internal/labelgen/labelgen.go:286). One `#nosec G115` remains on the already-bounded conversion because gosec cannot prove the bound; the bound is visible and commented.

### SEC-02 — G115 int-to-rune conversion when completing a GTIN (gosec: high; real risk: low) — FIXED

- Original location: [normalize.go](internal/domain/normalize.go) in `WithCheckDigit` (`string(rune('0'+c))`).
- Fix: `c < 0 || c > 9` guards returning an empty string at [normalize.go:119](internal/domain/normalize.go:119). No annotation needed: gosec no longer flags it.

### SEC-03 — G204 Tesseract subprocess execution (medium) — MITIGATED AND ACCEPTED

- Location: [tesseract.go:70](internal/ocr/tesseract.go:70).
- Potential impact: command injection if the image path or the binary were attacker-controlled.
- Controls: binary resolved with `exec.LookPath` ([tesseract.go:32](internal/ocr/tesseract.go:32)); execution via `exec.CommandContext` with an argument list, **no shell**; per-read context timeout (15 s default); the image must exist and be a regular file ([tesseract.go:54](internal/ocr/tesseract.go:54)); the path is converted to absolute ([tesseract.go:64](internal/ocr/tesseract.go:64)) so no variable argument can be parsed as a flag.
- Declared residual risk: trust in the `tesseract` binary on the operator's `PATH`. On a hardened station the binary should be pinned to a controlled absolute install path. Justified `#nosec G204` at [tesseract.go:68](internal/ocr/tesseract.go:68).

### SEC-04 — G304 file writes via variable paths (medium) — FIXED BY CONSTRUCTION

- Original locations: `WriteImage` (labelgen) and `WriteFile` (report).
- Fix: both writes use `os.OpenRoot(dir)` — [labelgen.go:390](internal/labelgen/labelgen.go:390), [report.go:191](internal/report/report.go:191) — which confines operations to the directory at the OS level (including symlink defense), on top of requiring a flat name without `..` and `0600`/`0750` permissions. A traversal-rejection test lives in `internal/labelgen`.

### SEC-05 — G304 file reads via variable paths (medium; real risk: low) — ACCEPTED WITH JUSTIFICATION

- Locations: [app.go:282](internal/app/app.go:282) (SHA-256 fingerprint of images just generated inside the confined directory) and [main.go:266](cmd/packcheck/main.go:266) (reading the JSON passed via `--input`).
- Trust boundary: PackCheck is a local CLI; paths are typed by the operator on their own machine, exactly as with `cat` or `cp`. There is no remote input.
- Compensating controls: scan reads are capped at 10 MiB with `io.LimitReader` and an explicit `ErrInputTooLarge`. `#nosec G304` annotations carry the justification in code.

## Verified positive controls (evidence)

- **GO-INJECT-001 (SQL)**: every query uses `?` placeholders and `*Context` methods in [sqlite.go](internal/store/sqlite/sqlite.go). The probe `OP-1" OR "1"="1` returns `ErrOrderNotFound` with the table intact (`internal/store/sqlite` tests).
- **GO-INJECT-002 (commands)**: no shell usage anywhere in the module (verified by searching for `sh -c`/interpreters); the only subprocess is SEC-03.
- **GO-XSS-001**: reports render with `html/template`, with a test requiring `<script>` in label text to be escaped (`internal/report`). The template is a program constant: there is no template parsing from input (no SSTI surface, GO-SSTI-001).
- **GO-PATH-001**: SEC-04/SEC-05; no `ServeFile`/`FileServer` exists (no HTTP).
- **GO-SUPPLY-001**: `go.mod`/`go.sum` present; the checksum database is never disabled; `govulncheck` reports no vulnerabilities. Single runtime dependency: `modernc.org/sqlite` v1.61.1 (pure Go, no cgo of its own in our code).
- **GO-CONC-001**: `go test -race` clean for `internal/app`, `internal/validation`, and `internal/store/sqlite`. No mutable global state on validation paths.
- **GO-CRYPTO-001 / GO-AUTH-001**: N/A (no authentication or tokens). `math/rand` is used nowhere (verified by search). SHA-256 is used only as an evidence-image integrity fingerprint, never for passwords or secrets.
- **GO-UNSAFE-001**: no `import "unsafe"` and no cgo in first-party code (verified by search; the only "unsafe" text is the error name `ErrUnsafePath`). The transpiled SQLite driver sits behind the `OrderRepository` port and under `govulncheck`.
- **GO-CONFIG-001**: no secrets in code, fixtures, or evidence; the demo orders are synthetic data.

## Reproduce this review

```powershell
go vet ./...
staticcheck ./...
gosec ./...
govulncheck ./...
go test -race ./internal/app ./internal/validation ./internal/store/sqlite -count=1
```

Or everything at once: `.\scripts\verify.ps1` (analyzers are skipped with an explicit notice when not installed; results are never faked). CI runs the same analyzers on every push.
