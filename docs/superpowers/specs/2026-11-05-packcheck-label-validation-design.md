# PackCheck — Label and box-contents validation before sealing

Date: 2026-11-05. Status: approved by the user for implementation.
Settled decisions: Go CLI + library, embedded SQLite, OCR port with a deterministic simulator and an optional Tesseract adapter.

## 1. Problem (expanded)

At the packing station, a box is sealed based on three sources of truth that are not systematically cross-checked today: what the printed label says, what the box physically contains, and what the production order requires. When one of the three differs, the error travels sealed to the customer.

The concrete failure modes are:

- Wrong label: the text or GTIN belongs to another product, presentation, or customer. Root causes are typically an order change without a label-roll change, a manual reprint, or labels mixed between lines.
- Illegible or wrong date: faint, blurry, film-glared, or curved-surface printing. An expiry date that cannot be read is, for the customer and for regulators, an absent expiry date. A readable but order-divergent expiry breaks FIFO rotation and can ship expired or short-shelf-life product.
- Wrong or illegible lot: without a trustworthy lot there is no traceability. Faced with a complaint or a recall, the company cannot scope it and must recall more than necessary.
- Incomplete kit or extras: a component is missing, an extra one is present, or a quantity differs. The customer stops their line, returns the whole lot, and applies the contractual fine per rejected box, not per missing part.
- Uncontrolled variability: different label fonts and label suppliers change contrast, tracking, and layout. Human visual control fatigues over a shift, and its judgment is neither reproducible nor auditable.

Cost of the problem: returns and return freight, non-compliance fines, re-invoicing and credit notes, 100% inspection rework, overtime, regulatory risk from broken traceability, and loss of customer trust. The cost grows with distance: detecting at the packing table takes seconds; detecting at the customer's warehouse costs the lot.

The systemic cause is not the operator. It is the absence of a verifiable gate between "box ready" and "box sealed" that compares every critical field against the order and leaves evidence.

## 2. Objective (expanded)

Primary objective: no box is sealed without validating, field by field, product text, GTIN code, expiry date, lot, and complete contents against its production order in SQL, with an explainable verdict and reproducible per-box evidence.

Specific objectives:

1. Detect 100% of seeded critical errors (GTIN, lot, expiry, missing kit item, nonexistent order) before sealing. A critical error never passes because it is "close enough".
2. Treat "cannot be read" as a first-class outcome: an illegible label means rejection for reprint or rescan, never a guess.
3. Measure reading quality per field with CER and successful-read rate, broken down by degradation profile, font, and supplier, so decisions about failing labels or printers are data-driven.
4. Keep false rejects of good boxes at zero for clean labels and measure them per shift, so the gate does not become a bottleneck that staff learn to bypass.
5. Keep per-box cycle time in the millisecond order for the logical validation, so the control does not alter line pace.
6. Leave evidence verifiable by humans and by automated systems: per-case PNG images, a manifest with ground truth and SHA-256 fingerprints, JSON and HTML reports, and integration tests that walk the full flow.
7. Isolate real OCR behind a port: domain and metrics must be provable today without hardware and accept Tesseract or another engine tomorrow without rewriting validators.

Out of scope in this version: camera or PLC control, physical label printing, user authentication, an HTTP service, and multi-plant deployment. The surface is a Go CLI + library; the database is embedded SQLite.

## 3. Business rules (validation contract)

Severities: `critical` and `major` reject the box; `minor` allows passing with warnings. Verdicts: `pass`, `pass_with_warnings`, `reject`.

| Field | Rule | Code on failure | Severity |
|---|---|---|---|
| Order | The order must exist in SQL | `ORDER_NOT_FOUND` | critical |
| Text | Normalized equality against the order; a difference with CER ≤ 0.05 is tolerable degradation | `TEXT_MISMATCH` / `TEXT_DEGRADED` | critical / minor |
| Text | Empty | `TEXT_MISSING` | critical |
| GTIN | 13 digits with a valid GTIN check digit | `GTIN_INVALID` | critical |
| GTIN | Identical to the order's GTIN | `GTIN_MISMATCH` | critical |
| Expiry | Parseable in the accepted formats | `EXPIRY_UNREADABLE` | critical |
| Expiry | Identical to the order's and not expired | `EXPIRY_MISMATCH` / `EXPIRY_EXPIRED` | critical |
| Lot | Valid format and identical to the order's | `LOT_INVALID` / `LOT_MISMATCH` | critical |
| Contents | Every ordered component present with exact quantity; missing or short rejects | `CONTENT_MISSING` / `CONTENT_SHORT` | critical |
| Contents | Unordered component or excess quantity | `CONTENT_EXTRA` / `CONTENT_OVER` | major |
| Legibility | When quality is known, confidence ≥ 0.80 and legibility ≥ 0.70 | `LABEL_ILLEGIBLE` | major |

Text normalization: trimming, whitespace collapsing, uppercasing, and removal of accents and non-significant punctuation. Critical fields (GTIN, lot, expiry) require exact equality after normalization; there is no fuzzy acceptance.

Accepted expiry formats: `2006-01-02`, `02/01/2006`, `02-01-2006`, `20060102`, `01/2006`, and `2006-01` (month-year means the last day of that month). Any other text is `EXPIRY_UNREADABLE`.

## 4. Architecture

Hexagonal architecture (ports and adapters) with dependencies pointing inward. The domain knows nothing about SQL, OCR, or files.

- `internal/domain`: entities, pure rules (normalization, GTIN, dates, lot), and sentinel errors.
- `internal/ports`: `OrderRepository`, `OCREngine`, and `Clock` interfaces.
- `internal/validation`: per-field validators using the Strategy pattern, chained by an accumulating validator chain; `BoxValidator` orchestrates and measures cycle time with the injected `Clock`.
- `internal/metrics`: CER (two-row Levenshtein), per-field rates, recall, per-shift false rejects, and cycle percentiles.
- `internal/store/sqlite`: SQLite adapter (pure-Go `modernc.org/sqlite` driver), embedded schema, 100% parameterized queries.
- `internal/ocr`: reproducible deterministic simulator and optional Tesseract adapter (`NewEngine` factory); no shell, with timeout.
- `internal/labelgen`: stdlib PNG generation (in-house bitmap font, A/B profiles, suppliers, blurry/glare/curved/faint degradations) and a SHA-256 manifest.
- `internal/report`: JSON (machine), HTML, and text (human) reports with correct escaping.
- `internal/app`: use cases and the demo batch with declared ground truth.
- `cmd/packcheck`: thin CLI: `seed`, `genlabels`, `validate`, `runbatch`, `version`.

Patterns used with a purpose: Hexagonal, Strategy + Chain of Responsibility, Repository, Factory Method, constructor dependency injection, and an injectable clock for deterministic tests.

## 5. Verifiable tests (humans and automated systems)

Every case declares its ground truth before running: expected verdict and expected finding codes. A human verifies by opening the PNG and the HTML report; an automated system verifies by reading the JSON manifest and running `go test ./...`.

Minimum matrix: clean label font A, clean font B with another supplier, faint (passes with CER > 0), curved (passes with CER > 0), blurry (rejected `LABEL_ILLEGIBLE`), glare (rejected, expiry unread), another product's GTIN, GTIN with invalid check digit, wrong lot, wrong expiry, expired product, unreadable expiry, another product's text, missing component, short quantity, extra component, and nonexistent order. The cross-check is always against the SQL orders table seeded by the system itself. (The matrix was later expanded; the current case list is the one in `internal/app` and the README.)

End-to-end integration tests: create a temporary SQLite database, seed orders, generate images, read every box through the OCR port, validate against SQL, aggregate metrics, and write reports, asserting every expected verdict and every threshold metric.

## 6. Metrics and acceptance gates

- Per-field CER: `levenshtein(reference, reading) / len(reference in runes)`. Empty reference: 0 when the reading is also empty, 1 otherwise.
- Per-field successful-read rate: readings exactly equal after normalization / attempts.
- Seeded-error recall: seeded errors rejected / seeded errors. Gate: 100% on criticals.
- False reject: a box whose truth is `pass` but the system rejects. Gate: 0 in the batch; reported per shift. An illegibility rejection whose truth is `reject` is a correct "no-read", not a false reject.
- Returns before/after: in the demo batch, "before" counts the critical errors that would ship without a gate and "after" counts those that escape with the gate (false negatives). Reported as a simulation of the batch, not as plant history.
- Cycle time per box: `ValidateBox` duration on an injected monotonic clock; mean, p50, p95, and max are reported. Measurement note (verified with an independent probe): on coarse-tick-clock environments a single validation can measure 0 ns by resolution; in that case the valid cycle evidence is the repeated measurement (accumulate N validations and divide), exactly what the Go benchmark harness does. The integration test applies both techniques and asserts the gate with the repeated measurement.

## 7. Security and performance

Security: parameterized queries always; SQL interpolation is forbidden. Tesseract runs only via `exec.CommandContext` with fixed arguments and a timeout, never through a shell. Size limits on JSON inputs. Outputs confined to the destination directory with `os.OpenRoot`. Restrictive file permissions. No secrets in the repository. Errors never expose SQL or unnecessary internal paths.

Performance: validators are O(n) on field size; Levenshtein is O(n·m) time and O(min(n,m)) memory; regular expressions are compiled once; no avoidable allocations on hot paths; benchmarks included for CER and full validation.

## 8. Mandatory evidence per process

Every stage leaves artifacts: this specification, command outputs quoted in the terminal, images in `evidence/labels/`, `evidence/manifest.json` with SHA-256, `evidence/batch-report.json`, `evidence/report.html`, `evidence/summary.txt`, and the full output of `go test`, `go vet`, and benchmarks. No result is claimed without having been executed.

## 9. Specification self-review

No pending placeholders. No contradictions between rules, metrics, and the matrix: the CER ≤ 0.05 tolerance applies only to the corroborative product text; critical fields are exact. Scope fits a single implementation plan. Ambiguity resolved: "false reject" and "returns before/after" carry operational definitions in section 6.
