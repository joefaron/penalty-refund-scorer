// Copyright (c) 2026 Joe Faron, KYD Networks, LLC
// SPDX-License-Identifier: MIT

package main

import (
	"math/big"
	"testing"
	"time"
)

// Unit tests for the scoring engine — synthetic transactions only.
// End-to-end PDF parsing is exercised against the bundled examples in
// examples/transcripts/.

func d(s string) *time.Time {
	t, err := time.Parse("01-02-2006", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func tx(tc int, amt, date string, desc ...string) Transaction {
	t := Transaction{TC: tc, Amount: mustDec(amt)}
	if date != "" {
		t.Date = d(date)
	}
	if len(desc) > 0 {
		t.Description = desc[0]
	}
	return t
}

func TestScore_PaddysProfile_166_167_reversal_pair_filtered_orphan_196_survives(t *testing.T) {
	txs := []Transaction{
		tx(166, "1500.00", "03-15-2021", "FTF penalty"),
		tx(167, "-1500.00", "06-15-2021", "FTF reversal"),
		tx(196, "475.32", "08-20-2021", "Interest"),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 1 || r.Kept[0].TC != 196 {
		t.Fatalf("expected 1 kept TC 196, got %+v", r.Kept)
	}
	if r.Total.Cmp(mustDec("475.32")) != 0 {
		t.Fatalf("expected total 475.32, got %s", ratToFixed(r.Total))
	}
}

func TestScore_LphProfile_clean_kwong_no_reversals(t *testing.T) {
	txs := []Transaction{
		tx(166, "2000.00", "04-10-2021", "FTF"),
		tx(186, "1500.00", "04-10-2021", "FTD"),
		tx(196, "320.00", "04-10-2021", "Interest"),
	}
	r := ScoreTranscript(txs, "1120-S", "2020")
	if len(r.Kept) != 3 {
		t.Fatalf("expected 3 kept, got %d", len(r.Kept))
	}
	if r.Total.Cmp(mustDec("3820.00")) != 0 {
		t.Fatalf("total: %s", ratToFixed(r.Total))
	}
}

func TestScore_OutOfWindowZero(t *testing.T) {
	txs := []Transaction{
		tx(166, "500.00", "01-15-2024", "post-window"),
	}
	r := ScoreTranscript(txs, "1040", "2024")
	if len(r.Kept) != 0 {
		t.Fatalf("expected 0 kept, got %d", len(r.Kept))
	}
}

func TestScore_OR_rule_pre_window_year_in_window_assessment_eligible(t *testing.T) {
	// 1040 for 2017 -> obligation deadline 2018-04-15 (pre-window),
	// but assessment 2020-12-15 IS in window. KB §2 OR-rule: in-window
	// assessment alone makes it eligible.
	txs := []Transaction{tx(166, "750.00", "12-15-2020", "FTF")}
	r := ScoreTranscript(txs, "1040", "2017")
	if len(r.Kept) != 1 {
		t.Fatalf("expected 1 kept, got %d (%+v)", len(r.Kept), r.Skipped)
	}
}

func TestScore_BoundaryWindowOpen(t *testing.T) {
	out := ScoreTranscript([]Transaction{tx(166, "100.00", "01-19-2020", "")}, "1040", "2018")
	in := ScoreTranscript([]Transaction{tx(166, "100.00", "01-20-2020", "")}, "1040", "2018")
	if len(out.Kept) != 0 {
		t.Fatalf("01-19 should be excluded")
	}
	if len(in.Kept) != 1 {
		t.Fatalf("01-20 should be included; skipped=%+v", in.Skipped)
	}
}

func TestScore_BoundaryWindowClose(t *testing.T) {
	in := ScoreTranscript([]Transaction{tx(166, "100.00", "07-10-2023", "")}, "1040", "2024")
	out := ScoreTranscript([]Transaction{tx(166, "100.00", "07-11-2023", "")}, "1040", "2024")
	if len(in.Kept) != 1 {
		t.Fatalf("07-10 should be included; skipped=%+v", in.Skipped)
	}
	if len(out.Kept) != 0 {
		t.Fatalf("07-11 should be excluded")
	}
}

func TestScore_OR_rule_in_window_obligation_post_window_assessment(t *testing.T) {
	// 1120 for 2021 -> obligation 2022-04-15 (in window). Assessment 2024-08-01
	// (post-window). OR-rule: in-window obligation alone makes it eligible.
	txs := []Transaction{tx(186, "2200.00", "08-01-2024", "FTD")}
	r := ScoreTranscript(txs, "1120", "2021")
	if len(r.Kept) != 1 {
		t.Fatalf("expected 1 kept, got %d (%+v)", len(r.Kept), r.Skipped)
	}
}

func TestScore_941_HumanQuarterEndPeriod(t *testing.T) {
	// "Jun. 30, 2021" -> Q2 2021 -> obligation 2021-07-31 (in window)
	txs := []Transaction{tx(166, "800.00", "12-01-2024", "FTF")}
	r := ScoreTranscript(txs, "941", "Jun. 30, 2021")
	if len(r.Kept) != 1 {
		t.Fatalf("expected 1 kept; skipped=%+v", r.Skipped)
	}
}

func TestScore_FormNumberNormalization(t *testing.T) {
	txs := []Transaction{tx(166, "100.00", "08-15-2021", "")}
	r := ScoreTranscript(txs, "Form 1040", "2020")
	if len(r.Kept) != 1 {
		t.Fatalf("'Form 1040' must normalize")
	}
}

func TestScore_TC170_individual_estimated_tax_in_scope(t *testing.T) {
	r := ScoreTranscript([]Transaction{tx(170, "300.00", "06-01-2021", "")}, "1040", "2020")
	if len(r.Kept) != 1 || r.Kept[0].TC != 170 {
		t.Fatalf("TC 170 must be kept")
	}
}

func TestScore_PartialReversal_166_167_nets_to_1000(t *testing.T) {
	txs := []Transaction{
		tx(166, "1500.00", "03-15-2021", ""),
		tx(167, "-500.00", "06-15-2021", ""),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 1 {
		t.Fatalf("expected 1 kept, got %d", len(r.Kept))
	}
	if r.Total.Cmp(mustDec("1000.00")) != 0 {
		t.Fatalf("expected 1000.00 net, got %s", ratToFixed(r.Total))
	}
	if r.Kept[0].ReversalAmount == nil || r.Kept[0].ReversalAmount.Cmp(mustDec("500.00")) != 0 {
		t.Fatalf("reversal amount tracking broken: %+v", r.Kept[0])
	}
}

func TestScore_1041_estate_trust_deadline(t *testing.T) {
	// 1041 for 2020 -> obligation 2021-04-15 (in window)
	r := ScoreTranscript([]Transaction{tx(166, "200.00", "08-01-2024", "")}, "1041", "2020")
	if len(r.Kept) != 1 {
		t.Fatalf("1041 deadline must derive; skipped=%+v", r.Skipped)
	}
}

func TestScore_TC482_OIC_carve_out(t *testing.T) {
	txs := []Transaction{
		tx(166, "1000.00", "03-15-2021", ""),
		tx(482, "0.00", "06-01-2022", "OIC accepted"),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("TC 482 should carve out; got %+v", r.Kept)
	}
}

func TestScore_TC481_OIC_rejected_does_not_carve(t *testing.T) {
	txs := []Transaction{
		tx(166, "1000.00", "03-15-2021", ""),
		tx(481, "0.00", "06-01-2022", "OIC rejected"),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 1 {
		t.Fatalf("TC 481 must NOT carve out; got %+v", r.Kept)
	}
}

func TestScore_TC420_examination_carve_out(t *testing.T) {
	txs := []Transaction{
		tx(166, "1000.00", "03-15-2021", ""),
		tx(420, "0.00", "01-01-2022", "exam"),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("TC 420 should carve")
	}
}

func TestScore_TC300_carve_out(t *testing.T) {
	txs := []Transaction{
		tx(166, "1000.00", "03-15-2021", ""),
		tx(300, "5000.00", "01-01-2022", "additional tax"),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("TC 300 should carve")
	}
}

func TestScore_TC240_241_partial_reversal(t *testing.T) {
	txs := []Transaction{
		tx(240, "10000.00", "03-15-2021", ""),
		tx(241, "-2500.00", "06-15-2021", ""),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 1 {
		t.Fatalf("expected 1 kept")
	}
	if r.Total.Cmp(mustDec("7500.00")) != 0 {
		t.Fatalf("expected 7500.00, got %s", ratToFixed(r.Total))
	}
}

func TestScore_Form940_FUTA_deadline(t *testing.T) {
	// 940 for 2022 -> obligation 2023-01-31 (in window)
	r := ScoreTranscript([]Transaction{tx(166, "300.00", "08-01-2024", "")}, "940", "2022")
	if len(r.Kept) != 1 {
		t.Fatalf("940 deadline must derive")
	}
}

func TestScore_Form990_nonprofit_deadline(t *testing.T) {
	// 990 for 2021 -> obligation 2022-05-15 (in window)
	r := ScoreTranscript([]Transaction{tx(166, "300.00", "08-01-2024", "")}, "990", "2021")
	if len(r.Kept) != 1 {
		t.Fatalf("990 deadline must derive")
	}
}

func TestScore_TC240_241_full_reversal_nets_to_zero(t *testing.T) {
	txs := []Transaction{
		tx(240, "5000.00", "03-15-2021", ""),
		tx(241, "-5000.00", "06-15-2021", ""),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("expected 0 kept (full reversal)")
	}
}

func TestScore_MinClaimThreshold_filters_sub25(t *testing.T) {
	r := ScoreTranscript([]Transaction{tx(166, "23.99", "03-15-2021", "")}, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("sub-$25 must be filtered")
	}
	r2 := ScoreTranscript([]Transaction{tx(166, "25.00", "03-15-2021", "")}, "1040", "2020")
	if len(r2.Kept) != 1 {
		t.Fatalf("$25 exact must pass")
	}
}

func TestScore_MultiAssessmentMultiReversal_GreedyPool(t *testing.T) {
	// Two TC 166 ($1500 + $1000), one TC 167 (-$2000): pool of 2000 should
	// fully consume the first 1500, leaving 500 to consume from the second
	// 1000 (net 500 kept).
	txs := []Transaction{
		tx(166, "1500.00", "03-15-2021", ""),
		tx(166, "1000.00", "06-15-2021", ""),
		tx(167, "-2000.00", "08-01-2021", ""),
	}
	r := ScoreTranscript(txs, "1040", "2020")
	if len(r.Kept) != 1 {
		t.Fatalf("expected 1 kept (second TC 166 partial), got %d: %+v", len(r.Kept), r.Kept)
	}
	if r.Total.Cmp(mustDec("500.00")) != 0 {
		t.Fatalf("expected total 500.00, got %s", ratToFixed(r.Total))
	}
}

func TestScore_Form943_Ag_2022_deadline_in_window(t *testing.T) {
	// 943 for 2022 -> 2023-01-31 (in window)
	r := ScoreTranscript([]Transaction{tx(166, "200.00", "08-01-2024", "")}, "943", "2022")
	if len(r.Kept) != 1 {
		t.Fatalf("943 deadline must derive (skipped=%+v)", r.Skipped)
	}
}

func TestScore_Forms944_1120F_deadlines(t *testing.T) {
	// 944 for 2021 -> 2022-01-31 (in window)
	r := ScoreTranscript([]Transaction{tx(166, "200.00", "08-01-2024", "")}, "944", "2021")
	if len(r.Kept) != 1 {
		t.Fatalf("944 deadline must derive")
	}
	// 1120-F for 2021 -> 2022-04-15 (in window)
	r2 := ScoreTranscript([]Transaction{tx(166, "200.00", "08-01-2024", "")}, "1120-F", "2021")
	if len(r2.Kept) != 1 {
		t.Fatalf("1120-F deadline must derive")
	}
}

func TestScore_TC180_unreversed_claimable(t *testing.T) {
	r := ScoreTranscript([]Transaction{tx(180, "500.00", "03-15-2021", "")}, "941", "2020Q1")
	if len(r.Kept) != 1 || r.Kept[0].TC != 180 {
		t.Fatalf("TC 180 unreversed must be kept")
	}
}

func TestScore_TC180_181_full_reversal(t *testing.T) {
	txs := []Transaction{
		tx(180, "500.00", "03-15-2021", ""),
		tx(181, "-500.00", "06-15-2021", ""),
	}
	r := ScoreTranscript(txs, "941", "2020Q1")
	if len(r.Kept) != 0 {
		t.Fatalf("TC 180 + TC 181 should fully reverse to zero")
	}
}

// The 2026-05-07 KB fix: a TC 181 reversal pool MUST NOT consume TC 186 when
// TC 180 is also present. Without dual-target [180,186] sharing, a TC 180 +
// TC 181 + TC 186 transcript falsely zeroed the TC 186 claim.
func TestScore_TC181_pool_does_not_falsely_consume_TC186(t *testing.T) {
	txs := []Transaction{
		tx(180, "500.00", "03-15-2021", ""),
		tx(181, "-500.00", "06-15-2021", ""),
		tx(186, "1000.00", "03-15-2021", ""),
	}
	r := ScoreTranscript(txs, "941", "2020Q1")
	if len(r.Kept) != 1 || r.Kept[0].TC != 186 {
		t.Fatalf("TC 186 must survive (TC 181 pool consumed by TC 180); got %+v", r.Kept)
	}
	if r.Kept[0].Amount.Cmp(mustDec("1000.00")) != 0 {
		t.Fatalf("TC 186 should be full $1000, not netted; got %s", ratToFixed(r.Kept[0].Amount))
	}
}

func TestScore_NonPositiveAmount_skipped(t *testing.T) {
	// Bare credit (no matching reversal pool) - skipped as non-positive.
	r := ScoreTranscript([]Transaction{tx(166, "-100.00", "03-15-2021", "")}, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("non-positive should be skipped")
	}
}

func TestScore_NonTargetCode_skipped_with_reason(t *testing.T) {
	r := ScoreTranscript([]Transaction{tx(150, "1000.00", "03-15-2021", "")}, "1040", "2020")
	if len(r.Kept) != 0 {
		t.Fatalf("non-target must be skipped")
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Reason != "non-target code" {
		t.Fatalf("expected 'non-target code' skip reason, got %+v", r.Skipped)
	}
}

func TestExtractModuleHeader(t *testing.T) {
	// 941 quarterly: month 06 -> Q2
	form, period, _, _ := extractModuleHeader("Form 941 Account Transcript\nReport for Tax Period Ending: 06-30-2020")
	if form != "941" || period != "2020Q2" {
		t.Fatalf("941 Q2: got (%q, %q)", form, period)
	}
	// 1120S annual: IRS title bar uses "1120S" (no dash) - month 12 -> just year
	form, period, _, _ = extractModuleHeader("Form 1120S Account Transcript\nReport for Tax Period Ending: 12-31-2021")
	if form != "1120S" || period != "2021" {
		t.Fatalf("1120S: got (%q, %q)", form, period)
	}
	// Missing header -> empty
	form, period, _, _ = extractModuleHeader("garbage page text with no markers")
	if form != "" || period != "" {
		t.Fatalf("garbage: got (%q, %q)", form, period)
	}
	// Q1: month 03
	_, period, _, _ = extractModuleHeader("Form 941 Account Transcript\nReport for Tax Period Ending: 03-31-2022")
	if period != "2022Q1" {
		t.Fatalf("Q1: got %q", period)
	}
	// Q4: month 12 on 941
	_, period, _, _ = extractModuleHeader("Form 941 Account Transcript\nReport for Tax Period Ending: 12-31-2022")
	if period != "2022Q4" {
		t.Fatalf("Q4: got %q", period)
	}
	// EIN last-4 + IRS truncated name
	hdr := "Form 941 Account Transcript\nReport for Tax Period Ending: 06-30-2020\nTaxpayer Identification Number:\nXX-XXX0976\nSAUL GOODMAN HOLDINGS LLC\n309 E MAIN ST\n"
	_, _, ein, name := extractModuleHeader(hdr)
	if ein != "0976" {
		t.Fatalf("EIN last4: got %q want %q", ein, "0976")
	}
	if name != "SAUL GOODMAN HOLDINGS LLC" {
		t.Fatalf("IRS name: got %q want %q", name, "SAUL GOODMAN HOLDINGS LLC")
	}
}

func TestHumanizeBundledBasename(t *testing.T) {
	cases := map[string]string{
		"may6-saul-goodman.pdf":            "Saul Goodman",
		"apr30-mesa-verde-corp.pdf":        "Mesa Verde Corp",
		"2026-05-06-kettleman-group.pdf":   "Kettleman Group",
		"some_entity_name.pdf":             "Some Entity Name",
		"PreciousGems.pdf":                 "Preciousgems",
	}
	for in, want := range cases {
		got := humanizeBundledBasename(in)
		if got != want {
			t.Errorf("humanizeBundledBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFilename(t *testing.T) {
	cases := []struct {
		fn, form, period string
	}{
		{"ACTR-941-2020-Q2.pdf", "941", "2020Q2"},
		{"ACTR-1040-2021.pdf", "1040", "2021"},
		{"ACTR-1120S-2022.pdf", "1120S", "2022"},
		{"ACTR-941-2020-Q1 (1).pdf", "941", "2020Q1"},
		{"random.pdf", "", ""},
	}
	for _, c := range cases {
		f, p := ParseFilename(c.fn)
		if f != c.form || p != c.period {
			t.Errorf("ParseFilename(%q) = (%q,%q), want (%q,%q)", c.fn, f, p, c.form, c.period)
		}
	}
}

// Sanity check for big.Rat penny accuracy on assertions that would have
// failed with float64.
func TestRat_PennyAccuracy(t *testing.T) {
	a := mustDec("0.10")
	b := mustDec("0.20")
	sum := new(big.Rat).Add(a, b)
	want := mustDec("0.30")
	if sum.Cmp(want) != 0 {
		t.Fatalf("0.10 + 0.20 != 0.30 (got %s)", ratToFixed(sum))
	}
}
