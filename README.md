# Penalty Refund Scorer

[![Release](https://img.shields.io/github/v/release/joefaron/penalty-refund-scorer?style=flat-square)](https://github.com/joefaron/penalty-refund-scorer/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat-square)](LICENSE)

Standalone command-line tool that reads IRS Account Transcript PDFs and writes a CSV listing every claimable penalty / interest finding under the **COVID disaster window (Jan 20 2020 – Jul 10 2023)**.

Feed it a folder of PDFs. You get back a CSV with the business name, EIN last-4, claimable dollar amount, and every contributing penalty line — one row per finding. No internet. No login. No data leaves your machine.

## Download

Go to **[Releases](https://github.com/joefaron/penalty-refund-scorer/releases/latest)** and grab the build for your OS:

| File | Platform |
|---|---|
| `go-scorer-windows-amd64.exe` | Windows 10/11 (most common) |
| `go-scorer-darwin-arm64` | macOS (Apple Silicon — M1/M2/M3) |
| `go-scorer-darwin-amd64` | macOS (Intel) |
| `go-scorer-linux-amd64` | Linux |
| `penalty-refund-scorer-windows.zip` | Windows bundle (exe + `Run-PenaltyScorer.bat` double-click wrapper) |

On Windows, the easiest path is the bundle: unzip, double-click `Run-PenaltyScorer.bat`, pick the folder with your transcript PDFs.

> **SmartScreen warning on first run is normal.** The binary is unsigned. Click "More info" → "Run anyway". Or build it yourself from the source in this repo.

## What it does

Walks a folder recursively, parses every IRS Account Transcript PDF, and applies the canonical COVID-window scoring rules:

- Filters to **target transaction codes**: 160, 166, 170, 176, 180, 186, 196, 234, 238, 240, 246, 270, 276.
- Applies the **Kwong COVID disaster window**: assessment date OR the form's original due-date must fall in Jan 20 2020 – Jul 10 2023.
- Nets out **reversal pairs** (TC 161/167 abate TC 166, TC 197 abates TC 196, etc.) using a cumulative pool — a $500 abatement against $1,500 of penalties leaves $1,000 claimable.
- Drops findings under **$25** (configurable via `--min-claim`).
- Detects **module-killing carve-outs**: TC 300 (additional tax via examination), TC 420/424 (exam indicator), TC 482 (Offer in Compromise accepted) — those modules need manual CPA review and are excluded from the totals.

Per-entity output includes the **EIN last-4** and the **truncated taxpayer name** as printed on the transcript. (IRS transcripts mask the first five EIN digits as `XX-XXX####` and use a shortened name format, so we surface only what's actually on the document — full EIN and full legal name are not in the source data.)

## Compatible input sources

The scorer is content-signature based — it reads the PDF text for the IRS Account Transcript header markers (`Form X Account Transcript`, `Page 1/N`, `Report for Tax Period Ending`, `Taxpayer Identification Number: XX-XXX####`) and works against transcripts exported from any of the common practitioner sources:

- **IRS e-Services TDS** (Transcript Delivery System) — direct ZIP downloads from the IRS portal
- **TaxNow** (taxnow.com)
- **Pitbull Tax / PitBullTax** (pitbulltax.com)
- **Canopy** (canopytax.com)
- **TaxStatus** (taxstatus.com)
- **Tax Help Software** (taxhelpsoftware.com)

The tool **does not** pull transcripts from any of these — you bring the PDFs you've already downloaded. (Automating the pull side against IRS e-Services is a Login.gov TOS violation and a CAF-revocation risk for the practitioner; that's intentionally out of scope.)

## Input shapes

The tool accepts two PDF layouts — mix them in the same run if you want:

**Shape A: One PDF per form-period.** This is the most common shape — one transcript per file. The recognized filename convention is:

```
ACTR-{FORM}-{YEAR}.pdf            for annual forms (1040, 1120, 1065, etc.)
ACTR-{FORM}-{YEAR}-Q{N}.pdf       for quarterly 941s (Q1-Q4)
```

One folder per client; folder name becomes the entity name:

```
my-clients/
├── Acme Restaurant Group/
│   ├── ACTR-941-2020-Q1.pdf
│   └── ACTR-941-2020-Q2.pdf
└── Beta Holdings LLC/
    └── ACTR-1120-2021.pdf
```

If your filenames don't follow this convention, the scorer still reads form / period / EIN / taxpayer name **from inside the PDF**. The filename pattern is only used to bucket multiple PDFs by entity — rename or move into per-entity folders if you want the entity column to come out clean.

**Shape B: One PDF per client (bundled multi-transcript export).** No naming rules. The scorer detects every transcript inside via the `Page 1/N` boundaries and `Tax Period Ending` headers. Entity name comes from the filename (date prefix stripped, Title Cased).

## Usage

**Windows (easy path):** double-click `Run-PenaltyScorer.bat`, pick a folder. Done.

**Command line (all platforms):**

```bash
# Walk a folder of PDFs, write findings.csv
./go-scorer --root /path/to/transcripts --out findings.csv

# Drop the $25 minimum claim threshold (include every penalty, no matter how small)
./go-scorer --root ./transcripts --out findings.csv --min-claim 0

# Also emit per-entity JSON for debugging / audit
./go-scorer --root ./transcripts --out findings.csv --json-out ./per_entity_jsons/
```

Alongside the CSV the scorer writes `findings-ingest-summary.txt` — a human-readable report of every PDF scanned, what kind of document it was classified as, and why anything got skipped. Useful for partner hand-off when totals look off.

## Partner handoff mode

Three flags let a partner (lender, intermediate service provider, payroll processor — anyone holding a book of transcripts on their clients' behalf) run the scorer on their side and ship a CSV back without disclosing identifying information.

```bash
./go-scorer \
  --root /path/to/archive \
  --hmac-key 'shared-secret-2026' \
  --bands \
  --bucket-by ein \
  --out partner-handoff.csv
```

| Flag | Effect |
|---|---|
| `--hmac-key SECRET` | Replaces `entity`, `ein_last4`, `irs_name`, and `finding_source_pdf` columns with a 16-char HMAC-SHA256 hash of the EIN (or original entity name when EIN is absent). Deterministic — same input + same secret → same hash, so both sides can join on the hash without exchanging plaintext identifiers. |
| `--bands` | Quantizes `our_total` into coarse bands (`$0`, `$1-$4,999`, `$5,000-$24,999`, `$25,000-$99,999`, `$100,000+`) and suppresses per-finding rows. Lower precision for use cases where exact dollar amounts are over-disclosing. |
| `--bucket-by ein` | Groups PDFs by EIN-last-4 instead of by folder/filename. Useful when the partner archive is a flat directory and the EIN inside each transcript is the only reliable identity signal. |

The flags compose freely. The canonical "send us a CSV we can join against without seeing your book" invocation is all three together (above). Output:

```
entity,transcripts,with_findings,finding_count,claimable_band
56b58286af801f06,14,12,18,"$25,000-$99,999"
a4ea190d9ec4131b,17,9,11,"$5,000-$24,999"
baabebd59725918e,8,0,0,$0
```

When that same EIN later shows up in our onboarding flow, we compute the same HMAC with the shared secret and join. No raw transcripts cross the wire. The partner's compliance team has clean cover.

## Worked example

The `examples/transcripts/` folder ships with four synthetic IRS Account Transcripts you can run the scorer against to verify it works end-to-end. All entity names, EINs, dollar amounts, and dates are fictional (characters borrowed from *Better Call Saul*).

```bash
./go-scorer --root examples/transcripts --out examples/findings.csv
```

Produces:

```
entity                       ein_last4 irs_name                       transcripts with_findings our_total
Saul Goodman Holdings LLC    0976      SAUL GOODMAN HOLDINGS LLC      1           1             1250.00
Kettleman Tax Group Inc      1138      KETTLEMAN TAX GROUP INC        1           1             2520.00
Mesa Verde Bank Corp         4422      MESA VERDE BANK CORP           1           0             0.00
Wexler McGill LLP            8801      WEXLER MCGILL LLP              1           0             0.00
```

Plus one row per individual finding (sorted by amount descending within each entity):

```
finding_form  finding_period  finding_tc  finding_label         finding_amount  finding_date
1120          2021            186         Federal Tax Deposit   1800.00         05-15-2022
1120          2021            196         Interest Assessed     720.00          05-15-2022
941           2020Q2          166         Failure to File       1000.00         11-30-2020
941           2020Q2          196         Interest Assessed     250.00          11-30-2020
```

Why each entity comes out the way it does:

- **Saul Goodman Holdings LLC** — exercises the reversal pool. TC 166 assesses $1,500, TC 167 abates $500 → net $1,000 claimable on TC 166. TC 196 interest of $250 stays.
- **Kettleman Tax Group Inc** — straightforward $1,800 + $720 with no reversals.
- **Mesa Verde Bank Corp** — has a TC 300 (additional tax via exam), which kills the whole module. Penalty lines that would otherwise be claimable are filtered with a carve-out reason. Entity still appears in the CSV with `$0.00`.
- **Wexler McGill LLP** — $12.50 interest finding falls below the $25 minimum-claim floor and is dropped.

To regenerate the PDFs (or change the test cases), edit and re-run `examples/generate_examples.py` — needs `pymupdf`. Full expected output: [`examples/EXPECTED.md`](examples/EXPECTED.md).

## CSV columns

One row per finding (each penalty/interest line item that's eligible). Clients with zero findings still get one summary row so every entity is represented.

| Column | What it is |
|---|---|
| `entity` | Client name (folder or filename) |
| `ein_last4` | Last 4 of EIN as read off the transcript (`XX-XXX####`) — empty if not present |
| `irs_name` | Truncated taxpayer name as printed on the transcript |
| `transcripts` | How many transcripts scored for this entity |
| `with_findings` | How many had ≥1 claimable penalty |
| `our_total` | Total $ claimable for this client (repeats on each row) |
| `by_tc` | JSON breakdown by transaction code, e.g. `{"166":"1000.00","196":"250.00"}` |
| `finding_idx` | Row number within the client (sorted by $ desc) |
| `finding_form` | Form (941, 1120, 1040, …) |
| `finding_period` | Period (`2020Q1`, `2021`, …) |
| `finding_tc` | IRS transaction code |
| `finding_label` | Human label ("Failure to File", "Interest Assessed", …) |
| `finding_amount` | $ for this finding (post-reversal net) |
| `finding_date` | Assessment date (`MM-DD-YYYY`) |
| `finding_source_pdf` | Source filename |

## Build from source

```bash
git clone https://github.com/joefaron/penalty-refund-scorer.git
cd penalty-refund-scorer
go build -o go-scorer .
```

Requires Go 1.24+. Single dependency: `github.com/ledongthuc/pdf` (PDF text extraction).

```bash
# Run tests
go test -v ./...

# Run against the bundled examples
./go-scorer --root examples/transcripts --out /tmp/findings.csv
```

## Limitations

- **Scrambled PDFs** (scans, non-standard text layers) parse poorly. The scorer reads the TRANSACTIONS section verbatim; if the text doesn't extract cleanly, lines are missed. Spot-check totals against a known number when you can.
- **Not Form 843 generation.** This tool identifies claimable findings and dollar amounts only. Generating the actual refund packet is out of scope.
- **EIN is partial.** IRS Account Transcripts mask the first five digits of the EIN. The scorer surfaces only the last four — enough to disambiguate within a small book, not enough to use as a primary key.
- **Multi-page text-order sensitivity.** On unusual transcript layouts with many pages, the Go PDF library may extract transactions in a different visual order than other parsers. For audit-grade numbers (CPA review, IRS filings), have a CPA verify each finding against the source transcript before sending anything to the IRS.

## Privacy & data handling

The binary is fully offline. It reads PDFs from a folder, writes a CSV next to them, and exits. No network calls, no telemetry, no upload. The output is bit-for-bit reproducible — same input → same output. If you're worried, run it in an air-gapped VM.

## License

MIT. See [LICENSE](LICENSE).

Copyright © 2026 Joe Faron, KYD Networks, LLC.

## Questions / weird results

Open an [issue](https://github.com/joefaron/penalty-refund-scorer/issues) with the entity name + the CSV row that looks wrong. The scorer doesn't upload anything anywhere, so attaching a sample PDF (with PII redacted) is the only way to reproduce on our side.
