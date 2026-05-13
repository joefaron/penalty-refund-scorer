"""Generate synthetic IRS Account Transcript PDFs for end-to-end demos.

Run:
    python examples/generate_examples.py

Writes three text-based PDFs under examples/transcripts/ matching the IRS
Account Transcript layout the scorer parses. All entities, EINs, dollar
amounts, and dates are fictional — characters borrowed from Better Call
Saul. No real taxpayer data.

Requires: pymupdf (`pip install pymupdf`)
"""
import os
import fitz  # pymupdf

OUT = os.path.join(os.path.dirname(__file__), "transcripts")
os.makedirs(OUT, exist_ok=True)


def build_pdf(path: str, modules: list[str]) -> None:
    """Build a PDF, one module per page. Writes each line with a separate
    insert_text() call so the PDF content stream contains explicit per-line
    text objects — ledongthuc/pdf (the Go reader) needs this to recover
    newlines on text extraction. insert_textbox collapses everything into
    one stream which the Go reader can't separate back into lines.
    """
    doc = fitz.open()
    for text in modules:
        page = doc.new_page(width=612, height=792)
        y = 40
        for line in text.splitlines():
            if line == "":
                y += 9
                continue
            page.insert_text(
                (40, y),
                line,
                fontname="cour",
                fontsize=8,
            )
            y += 10
            if y > 780:
                # spill to a new page so transactions never get clipped
                page = doc.new_page(width=612, height=792)
                y = 40
    doc.save(path)
    doc.close()


def transcript(form: str, period_end: str, ein_last4: str, name: str,
               transactions: list[tuple], page_total: int = 1) -> str:
    """Return the text body for ONE transcript module.

    transactions is a list of (tc, description, cycle, date, amount) tuples.
    """
    lines = [
        f"Page 1/{page_total}",
        "This Product Contains Sensitive Taxpayer Data",
        "",
        f"Form {form} Account Transcript",
        "Request Date:",
        "01-15-2026",
        "Response Date:",
        "01-15-2026",
        "Tracking Number:",
        "100000000000",
        "Form Number:",
        form,
        "Report for Tax Period Ending:",
        period_end,
        "Taxpayer Identification Number:",
        f"XX-XXX{ein_last4}",
        name,
        "100 ALBUQUERQUE BLVD",
        "",
        "** Any minus sign shown below signifies a credit amount **",
        "Account balance:",
        "$0.00",
        "Accrued interest:",
        "$0.00",
        "As of:",
        "01-15-2026",
        "Accrued penalty:",
        "$0.00",
        "As of:",
        "01-15-2026",
        "",
        "** Information from the return or as adjusted **",
        "",
        "TRANSACTIONS",
        "CODE",
        "EXPLANATION OF TRANSACTION",
        "CYCLE",
        "DATE",
        "AMOUNT",
    ]
    for tc, desc, cycle, date, amount in transactions:
        sign = "-" if amount < 0 else ""
        amount_str = f"{sign}${abs(amount):,.2f}"
        lines.extend([
            str(tc),
            desc,
            cycle,
            date,
            amount_str,
        ])
    return "\n".join(lines)


# ENTITY 1: Saul Goodman Holdings LLC -- 941 Q2 2020
# One claimable FTF ($1,500), partially reversed -$500 -> $1,000 net claimable.
# Plus interest $250 (under threshold so dropped).
e1 = transcript(
    form="941",
    period_end="06-30-2020",
    ein_last4="0976",
    name="SAUL GOODMAN HOLDINGS LLC",
    transactions=[
        (150, "Tax return filed", "202046", "11-30-2020", 27014.69),
        (650, "Federal tax deposit", "", "08-12-2020", -4002.88),
        (166, "Penalty for late filing", "202046", "11-30-2020", 1500.00),
        (167, "Reduced or removed penalty for late filing", "202112", "03-15-2021", -500.00),
        (196, "Interest charged for late payment", "202046", "11-30-2020", 250.00),
    ],
)
build_pdf(os.path.join(OUT, "saul-goodman-holdings-llc.pdf"), [e1])


# ENTITY 2: Kettleman Tax Group Inc -- 1120 2021 (annual)
# Two claimable findings: TC 186 ($1,800) + TC 196 ($720). Total $2,520.
e2 = transcript(
    form="1120",
    period_end="12-31-2021",
    ein_last4="1138",
    name="KETTLEMAN TAX GROUP INC",
    transactions=[
        (150, "Tax return filed", "202220", "05-15-2022", 50000.00),
        (186, "Penalty for failure to deposit", "202220", "05-15-2022", 1800.00),
        (196, "Interest charged for late payment", "202220", "05-15-2022", 720.00),
    ],
)
build_pdf(os.path.join(OUT, "kettleman-tax-group-inc.pdf"), [e2])


# ENTITY 3: Mesa Verde Bank Corp -- 1120 2020 (carve-out)
# Has TC 300 (additional tax via exam) -> kills the whole module.
# Penalties present should be SKIPPED with carve-out reason.
e3 = transcript(
    form="1120",
    period_end="12-31-2020",
    ein_last4="4422",
    name="MESA VERDE BANK CORP",
    transactions=[
        (150, "Tax return filed", "202118", "05-10-2021", 120000.00),
        (300, "Additional tax assessed by examination", "202240", "10-03-2022", 25000.00),
        (186, "Penalty for failure to deposit", "202118", "05-10-2021", 3500.00),
        (196, "Interest charged for late payment", "202118", "05-10-2021", 980.00),
    ],
)
build_pdf(os.path.join(OUT, "mesa-verde-bank-corp.pdf"), [e3])


# ENTITY 4: Wexler McGill LLP -- 1065 2020 (no findings, all under threshold)
# Single small interest charge under $25 floor.
e4 = transcript(
    form="1065",
    period_end="12-31-2020",
    ein_last4="8801",
    name="WEXLER MCGILL LLP",
    transactions=[
        (150, "Tax return filed", "202112", "03-15-2021", 0.00),
        (196, "Interest charged for late payment", "202112", "03-15-2021", 12.50),
    ],
)
build_pdf(os.path.join(OUT, "wexler-mcgill-llp.pdf"), [e4])


print(f"Wrote {len(os.listdir(OUT))} PDFs to {OUT}")
for f in sorted(os.listdir(OUT)):
    print(f"  - {f}")
