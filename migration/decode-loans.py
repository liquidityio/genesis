#!/usr/bin/env python3
"""
decode-loans.py — Extract structured loan data from Substrate SCALE-encoded storage.

The karus_loan pallet stores loans as SCALE-encoded structs. The raw export
contains partial ASCII in the 'ascii' field. This script parses those ASCII
fragments into structured JSON suitable for LoanRegistry.importLoan() calls.

Output: genesis/loans-decoded.json
"""

import json
import re
import sys

def parse_loan_ascii(ascii_data: str, idx: int) -> dict:
    """Parse the ASCII preview of a SCALE-encoded loan record."""
    loan = {
        "id": idx,
        "raw_len": 0,
        "status": "Unknown",
        "dealer_name": "",
        "dealer_state": "",
        "vehicle_make": "",
        "vehicle_model": "",
        "vehicle_year": 0,
        "vehicle_mileage": 0,
        "vehicle_value": 0.0,
        "vin_masked": "",
        "loan_amount": 0.0,
        "down_payment": 0.0,
        "apr_pct": 0.0,
        "term_months": 0,
        "monthly_payment": 0.0,
        "credit_score_range": "",
        "approval_date": "",
        "decision": "",
        "karus_rating": "",
        "applicant_ref": "",
    }

    # Status
    if "Complete" in ascii_data:
        loan["status"] = "Complete"
    elif "Declined" in ascii_data:
        loan["status"] = "Declined"
    elif "Approved" in ascii_data:
        loan["status"] = "Approved"

    # Dealer name + state (pattern: NAME STATE)
    dealer_match = re.search(r'([A-Z][A-Z0-9 ]+(?:CORP|LLC|INC|MOTORS|AUTO|CHOICE)(?:\s+\w+)?)\s+([A-Z]{2})', ascii_data)
    if dealer_match:
        loan["dealer_name"] = dealer_match.group(1).strip()
        loan["dealer_state"] = dealer_match.group(2)

    # Simplici direct
    if "simplici-direct" in ascii_data.lower() or "Simplici" in ascii_data:
        loan["dealer_name"] = loan["dealer_name"] or "Simplici"

    # Vehicle
    make_match = re.search(r'(ALFA ROMEO|HONDA|HYUNDAI|DODGE|TOYOTA|FORD|BMW|CHEVROLET|NISSAN|SUBARU|KIA|MAZDA|VOLKSWAGEN|LEXUS|ACURA|INFINITI|AUDI|MERCEDES|JEEP|GMC|RAM|CHRYSLER|BUICK|CADILLAC|LINCOLN|VOLVO|LAND ROVER|JAGUAR|PORSCHE|MINI|FIAT|GENESIS|MITSUBISHI|MASERATI)', ascii_data, re.IGNORECASE)
    if make_match:
        loan["vehicle_make"] = make_match.group(1).upper()

    model_patterns = [
        r'(GIULIA\s+TI)', r'(CR-V)', r'(ELANTRA)', r'(JOURNEY)', r'(ODYSSEY)',
        r'(HR-V)', r'(CIVIC)', r'(ACCORD)', r'(CAMRY)', r'(COROLLA)',
        r'(RAV4)', r'(ROGUE)', r'(ALTIMA)', r'(SENTRA)', r'(OUTBACK)',
    ]
    for pat in model_patterns:
        m = re.search(pat, ascii_data, re.IGNORECASE)
        if m:
            loan["vehicle_model"] = m.group(1).upper()
            break

    # Year (4-digit near vehicle)
    year_match = re.search(r'(\d{4})\.0', ascii_data)
    if year_match:
        yr = int(year_match.group(1))
        if 2000 <= yr <= 2030:
            loan["vehicle_year"] = yr

    # VIN (masked)
    vin_match = re.search(r'(\*{5,}[\dA-Z]{4})', ascii_data)
    if vin_match:
        loan["vin_masked"] = vin_match.group(1)

    # Loan amounts (float patterns)
    amounts = re.findall(r'(\d+\.\d+)', ascii_data)
    if len(amounts) >= 2:
        loan["loan_amount"] = float(amounts[0])
        loan["down_payment"] = float(amounts[1])

    # Vehicle value
    value_match = re.findall(r'(\d{4,6}\.\d+)', ascii_data)
    for v in value_match:
        fv = float(v)
        if 5000 < fv < 100000 and fv != loan["loan_amount"]:
            loan["vehicle_value"] = fv
            break

    # APR (look for 0.XXXX pattern near term)
    apr_matches = re.findall(r'0\.(\d{2,4})\s', ascii_data)
    for a in apr_matches:
        pct = float(f"0.{a}") * 100
        if 3 < pct < 30:
            loan["apr_pct"] = round(pct, 2)
            break

    # Term months
    term_match = re.search(r'(\d{2,3})\s+\d+\s+(?:ALFA|HONDA|HYUNDAI|DODGE)', ascii_data)
    if not term_match:
        term_match = re.search(r'(\d{2})\.\d\s', ascii_data)
    if term_match:
        t = int(float(term_match.group(1)))
        if t in (12, 24, 36, 48, 60, 72, 75, 84):
            loan["term_months"] = t

    # Credit score range
    score_match = re.search(r'(\d{3})-(\d{3})', ascii_data)
    if score_match:
        loan["credit_score_range"] = f"{score_match.group(1)}-{score_match.group(2)}"

    # Approval date
    date_match = re.search(r'(2026-\d{2}-\d{2})', ascii_data)
    if date_match:
        loan["approval_date"] = date_match.group(1)

    # Decision
    if "APPROVED" in ascii_data:
        loan["decision"] = "APPROVED"
    elif "DECLINED" in ascii_data:
        loan["decision"] = "DECLINED"

    # Karus rating
    if "Karus Option" in ascii_data:
        loan["karus_rating"] = "Karus Option"

    # Applicant ref (hex)
    ref_match = re.search(r'([0-9a-f]{24,})-([A-Z0-9]{6})', ascii_data)
    if ref_match:
        loan["applicant_ref"] = f"{ref_match.group(1)}-{ref_match.group(2)}"

    return loan


def to_import_args(loan: dict) -> dict:
    """Convert decoded loan to LoanRegistry.importLoan() args."""
    status_map = {
        "Complete": 8,     # Servicing
        "Approved": 2,     # Approved
        "Declined": 3,     # Declined
        "Funded": 5,       # Funded
        "Unknown": 0,      # Applied
    }

    app_data = json.dumps({
        "dealer": loan["dealer_name"],
        "dealer_state": loan["dealer_state"],
        "vehicle": f"{loan['vehicle_year']} {loan['vehicle_make']} {loan['vehicle_model']}",
        "vin": loan["vin_masked"],
        "vehicle_value": loan["vehicle_value"],
        "apr": loan["apr_pct"],
        "term_months": loan["term_months"],
        "credit_score": loan["credit_score_range"],
        "approval_date": loan["approval_date"],
        "karus_rating": loan["karus_rating"],
    })

    return {
        "creator": "0x0000000000000000000000000000000000000000",  # will be set to admin
        "status": status_map.get(loan["status"], 0),
        "applicationData": app_data,
        "loanAmount": int(loan["loan_amount"] * 1e18),
        "remainingBalance": int(loan["loan_amount"] * 1e18),  # assume full balance
        "paymentsMade": 0,
    }


def main():
    with open("genesis/lqdty-evm-import.json") as f:
        data = json.load(f)

    loans_raw = data.get("loans", [])
    print(f"Decoding {len(loans_raw)} loan records...")

    decoded = []
    import_args = []
    for i, loan in enumerate(loans_raw):
        ascii_data = loan.get("ascii", loan.get("asciiPreview", ""))
        d = parse_loan_ascii(ascii_data, i)
        d["raw_len"] = loan.get("len", 0)
        decoded.append(d)
        import_args.append(to_import_args(d))
        print(f"  Loan {i}: {d['vehicle_year']} {d['vehicle_make']} {d['vehicle_model']}, "
              f"${d['loan_amount']:,.2f} @ {d['apr_pct']}% / {d['term_months']}mo, "
              f"status={d['status']}, score={d['credit_score_range']}")

    output = {
        "source": "Substrate karus_loan pallet",
        "count": len(decoded),
        "decoded": decoded,
        "importArgs": import_args,
    }

    outpath = "genesis/loans-decoded.json"
    with open(outpath, "w") as f:
        json.dump(output, f, indent=2)
    print(f"\nWrote {outpath}")


if __name__ == "__main__":
    main()
