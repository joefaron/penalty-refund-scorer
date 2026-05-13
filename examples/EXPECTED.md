# Expected output for the example transcripts

This file documents what the scorer should produce when run against `examples/transcripts/`. If your output differs, something has regressed.

## Run

```bash
go build -o go-scorer .
./go-scorer --root examples/transcripts --out examples/findings.csv
```

## Expected totals

| Entity | EIN last-4 | IRS name | Findings | Claimable |
|---|---|---|---|---|
| Saul Goodman Holdings LLC | 0976 | SAUL GOODMAN HOLDINGS LLC | 2 | **$1,250.00** |
| Kettleman Tax Group Inc | 1138 | KETTLEMAN TAX GROUP INC | 2 | **$2,520.00** |
| Mesa Verde Bank Corp | 4422 | MESA VERDE BANK CORP | 0 (carve-out) | $0.00 |
| Wexler McGill LLP | 8801 | WEXLER MCGILL LLP | 0 (under floor) | $0.00 |
| **Total** | | | **4 findings** | **$3,770.00** |

## What each entity exercises

### Saul Goodman Holdings LLC — reversal pool

941 Q2 2020. Transactions:
- TC 166 (Failure to File) **$1,500.00** assessed
- TC 167 (Reduced/removed FTF penalty) **−$500.00** reversal
- TC 196 (Interest charged) **$250.00**

The TC 167 partial reversal nets against TC 166 to leave **$1,000.00** claimable on TC 166. TC 196 stays at $250. Both clear the $25 minimum-claim floor.

### Kettleman Tax Group Inc — straightforward case

1120 annual 2021. Transactions:
- TC 186 (Failure to Deposit) **$1,800.00**
- TC 196 (Interest charged) **$720.00**

No reversals. Both findings clear the floor.

### Mesa Verde Bank Corp — carve-out kills the module

1120 annual 2020. Transactions:
- TC 300 (Additional tax via exam) **$25,000.00** ← carve-out trigger
- TC 186 (Failure to Deposit) $3,500.00 ← would otherwise be claimable
- TC 196 (Interest charged) $980.00 ← would otherwise be claimable

The IRS exam (TC 300) takes the whole module out of Kwong-window eligibility — IRS Notice 2022-36 doesn't apply when a deficiency was assessed via examination. Both penalty lines are filtered with carve-out reasons. Entity still shows up in CSV with `$0.00`.

The other module-killing codes are TC 420/424 (exam indicator) and TC 482 (Offer in Compromise accepted).

### Wexler McGill LLP — under min-claim floor

1065 annual 2020. Transactions:
- TC 196 (Interest charged) **$12.50**

Under the $25 minimum-claim threshold (Form 843 prep cost exceeds the recovery). Drop with `min_claim` skip reason. Override with `--min-claim 0` to keep these in the output.

## How these PDFs were made

`generate_examples.py` builds plain-text PDFs via PyMuPDF, one `insert_text()` call per line so that the PDF content stream has explicit per-line text objects (the Go PDF reader needs this to recover newlines on extraction).

All entity names are fictional characters from *Better Call Saul*. All EINs, dollar amounts, and dates are fabricated. The PDFs do not represent any real taxpayer.

To regenerate after editing the script:

```bash
python examples/generate_examples.py
```

Requires `pymupdf` (`pip install pymupdf`).
