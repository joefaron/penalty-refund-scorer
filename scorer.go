// Copyright (c) 2026 Joe Faron, KYD Networks, LLC
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Canonical scoring constants: COVID disaster window, target transaction
// codes, reversal-pair mappings, carve-out codes, human-readable labels.

var (
	WindowStart = mustDate(2020, 1, 20)
	WindowEnd   = mustDate(2023, 7, 10)

	TargetTCs = map[int]bool{
		160: true, 166: true, 170: true, 176: true,
		180: true, 186: true, 196: true,
		234: true, 238: true, 240: true, 246: true,
		270: true, 276: true,
	}

	CarveOutTCs = map[int]bool{300: true, 420: true, 424: true, 482: true}

	TCLabels = map[int]string{
		160: "Failure to File (manual)",
		166: "Failure to File",
		170: "Estimated tax (manual)",
		176: "Estimated tax",
		180: "Federal Tax Deposit (manual)",
		186: "Federal Tax Deposit",
		196: "Interest Assessed",
		234: "Daily delinquency",
		238: "Estimated Tax (corp)",
		240: "Miscellaneous",
		246: "Misc Civil Penalty",
		270: "Manual Failure to Pay",
		276: "Failure to Pay",
	}

	// Reversal -> target assessment(s). Single-int = []int{n}. Dual targets share
	// a pool entry (ABATEMENT_REVERSAL_CODES).
	ReversalToTargets = map[int][]int{
		161: {160, 166},
		167: {166},
		171: {176},
		181: {180, 186},
		187: {186},
		191: {190},
		197: {196},
		221: {246},
		241: {240},
		271: {270, 276},
		277: {276},
	}

	annualDue = map[string][2]int{
		"1040": {4, 15}, "1040-NR": {4, 15}, "1040NR": {4, 15},
		"1120": {4, 15}, "1120-S": {3, 15}, "1120S": {3, 15},
		"1120-F": {4, 15}, "1120F": {4, 15},
		"1120-H": {4, 15}, "1120H": {4, 15},
		"1065": {3, 15},
		"940":  {1, 31}, "943": {1, 31}, "944": {1, 31},
		"1041":   {4, 15},
		"990":    {5, 15},
		"990-PF": {5, 15}, "990PF": {5, 15},
		"990-EZ": {5, 15}, "990EZ": {5, 15},
		"990-T": {5, 15}, "990T": {5, 15},
	}

	quarterlyForms = map[string]bool{"941": true, "720": true}

	quarterlyDue = map[int][2]int{
		1: {4, 30}, 2: {7, 31}, 3: {10, 31}, 4: {1, 31},
	}

	quarterEndToNumber = map[[2]int]int{
		{3, 31}: 1, {6, 30}: 2, {9, 30}: 3, {12, 31}: 4,
	}
)

// MinClaimThreshold dollars. Override with COVID_REFUND_MIN_CLAIM env. Default 25.
var MinClaimThreshold = mustDec("25")

// Transaction is the canonical post-parse shape of one IRS transcript
// line: {tc_code, amount, date, ...}. Amount is *big.Rat for exact
// decimal arithmetic (no float drift on penny-level scoring).
type Transaction struct {
	TC          int
	Amount      *big.Rat
	Date        *time.Time // nil if absent / unparseable
	Cycle       string
	DLN         string
	Description string
}

// Finding is a kept transaction post-scoring, plus the entity-level fields
// the CSV needs.
type Finding struct {
	TC             int
	Amount         *big.Rat // post-reversal net
	OriginalAmount *big.Rat // pre-reversal (==Amount when no reversal applied)
	ReversalAmount *big.Rat // 0 when no reversal applied
	Date           *time.Time
	Cycle          string
	DLN            string
	Description    string
	Form           string
	Period         string
	SourceFile     string
}

// SkipReason explains why a transaction was filtered out — human-readable
// strings so CSV/JSON audits stay readable.
type SkipReason struct {
	TC     int
	Reason string
}

type ScoreResult struct {
	Kept    []Finding
	Skipped []SkipReason
	Total   *big.Rat
}

// ScoreTranscript runs the full scoring pipeline on one module's transactions:
// reversal-pool netting, Kwong-window filter, carve-out detection, min-claim
// floor. formNumber/period are pre-normalized (caller's responsibility);
// normalizeFormNumber is re-run here as a defense-in-depth (idempotent on
// already-clean input).
func ScoreTranscript(txs []Transaction, formNumber, period string) ScoreResult {
	form := normalizeFormNumber(formNumber)
	pool := buildReversalPool(txs)
	carveTC := carveOutModuleTC(txs)

	res := ScoreResult{Total: new(big.Rat)}
	for _, tx := range txs {
		if !TargetTCs[tx.TC] {
			res.Skipped = append(res.Skipped, SkipReason{tx.TC, "non-target code"})
			continue
		}
		if carveTC != 0 {
			res.Skipped = append(res.Skipped, SkipReason{tx.TC,
				fmt.Sprintf("module carve-out (TC %d present); KB §3 not Kwong-eligible, requires manual CPA review", carveTC)})
			continue
		}
		if !kwongEligibleFor(tx, form, period) {
			res.Skipped = append(res.Skipped, SkipReason{tx.TC, kwongSkipReasonFor(tx, form, period)})
			continue
		}
		if tx.Amount.Sign() <= 0 {
			res.Skipped = append(res.Skipped, SkipReason{tx.TC, "non-positive amount (credit)"})
			continue
		}

		consumed, net := consumeReversals(pool[tx.TC], new(big.Rat).Set(tx.Amount))
		if net.Sign() <= 0 {
			res.Skipped = append(res.Skipped, SkipReason{tx.TC, "reversed by paired abatement TC"})
			continue
		}
		if net.Cmp(MinClaimThreshold) < 0 {
			res.Skipped = append(res.Skipped, SkipReason{tx.TC,
				fmt.Sprintf("below min claim threshold ($%s); net=$%s",
					ratToFixed(MinClaimThreshold), ratToFixed(net))})
			continue
		}

		f := Finding{
			TC:          tx.TC,
			Amount:      net,
			Date:        tx.Date,
			Cycle:       tx.Cycle,
			DLN:         tx.DLN,
			Description: tx.Description,
		}
		if consumed.Sign() > 0 {
			f.OriginalAmount = new(big.Rat).Set(tx.Amount)
			f.ReversalAmount = consumed
		}
		res.Kept = append(res.Kept, f)
		res.Total.Add(res.Total, net)
	}
	return res
}

// carveOutModuleTC returns the carve-out TC found in the list, or 0.
func carveOutModuleTC(txs []Transaction) int {
	for _, tx := range txs {
		if CarveOutTCs[tx.TC] {
			return tx.TC
		}
	}
	return 0
}

// poolEntry is shared across multiple pool keys for dual-target reversals
// (e.g. TC 161 -> [160,166]). Pointer identity guarantees a single $X
// reversal can't be double-consumed.
type poolEntry struct{ remaining *big.Rat }

// buildReversalPool keys negative reversal-TC amounts by the assessment TCs
// they target. One entry is shared across both target keys for dual-target
// codes (161, 181, 271) so a single $X reversal can't double-consume.
func buildReversalPool(txs []Transaction) map[int][]*poolEntry {
	pool := map[int][]*poolEntry{}
	for _, tx := range txs {
		targets, ok := ReversalToTargets[tx.TC]
		if !ok {
			continue
		}
		if tx.Amount.Sign() >= 0 {
			continue
		}
		entry := &poolEntry{remaining: ratAbs(tx.Amount)}
		for _, tc := range targets {
			pool[tc] = append(pool[tc], entry)
		}
	}
	return pool
}

// consumeReversals greedily consumes pool entries until the assessment is
// covered or the pool is exhausted. Returns (totalConsumed, net).
func consumeReversals(pool []*poolEntry, amount *big.Rat) (*big.Rat, *big.Rat) {
	consumed := new(big.Rat)
	net := new(big.Rat).Set(amount)
	if len(pool) == 0 {
		return consumed, net
	}
	for _, entry := range pool {
		if entry.remaining.Sign() <= 0 {
			continue
		}
		if net.Sign() <= 0 {
			break
		}
		take := new(big.Rat)
		if entry.remaining.Cmp(net) < 0 {
			take.Set(entry.remaining)
		} else {
			take.Set(net)
		}
		entry.remaining.Sub(entry.remaining, take)
		consumed.Add(consumed, take)
		net.Sub(net, take)
	}
	return consumed, net
}

// kwongEligibleFor: assessment date in window OR obligation due date in window.
func kwongEligibleFor(tx Transaction, form, period string) bool {
	if tx.Date != nil && inWindow(*tx.Date) {
		return true
	}
	due := computeObligationDueDate(form, period)
	if due == nil {
		return false
	}
	return inWindow(*due)
}

func kwongSkipReasonFor(tx Transaction, form, period string) string {
	due := computeObligationDueDate(form, period)
	dateStr := "nil"
	if tx.Date != nil {
		dateStr = tx.Date.Format("2006-01-02")
	}
	if due != nil {
		return fmt.Sprintf("neither assessment date %s nor obligation deadline %s inside Kwong window (%s..%s)",
			dateStr, due.Format("2006-01-02"),
			WindowStart.Format("2006-01-02"), WindowEnd.Format("2006-01-02"))
	}
	return fmt.Sprintf("assessment date %s outside Kwong window and no obligation due date derivable", dateStr)
}

func inWindow(d time.Time) bool {
	return !d.Before(WindowStart) && !d.After(WindowEnd)
}

// computeObligationDueDate computes the original IRS due date for a
// (form, period) pair. Returns nil for non-derivable forms/periods.
func computeObligationDueDate(form, period string) *time.Time {
	form = normalizeFormNumber(form)
	year := extractYear(period)
	if year == 0 {
		return nil
	}
	if md, ok := annualDue[form]; ok {
		d := mustDate(year+1, time.Month(md[0]), md[1])
		return &d
	}
	if quarterlyForms[form] {
		q := quarterFromPeriod(period)
		if q == 0 {
			return nil
		}
		md := quarterlyDue[q]
		yr := year
		if q == 4 {
			yr = year + 1
		}
		d := mustDate(yr, time.Month(md[0]), md[1])
		return &d
	}
	return nil
}

var yearRe = regexp.MustCompile(`(20\d{2})`)
var quarterRe = regexp.MustCompile(`Q([1-4])`)

// dateRe matches "Mar. 31, 2021" / "Jun 30 2021" / "March 31, 2021".
var humanDateRe = regexp.MustCompile(`(?i)(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z\.]*\s+(\d{1,2}),?\s+(20\d{2})`)

func extractYear(period string) int {
	m := yearRe.FindStringSubmatch(period)
	if len(m) != 2 {
		return 0
	}
	var y int
	fmt.Sscanf(m[1], "%d", &y)
	return y
}

func quarterFromPeriod(period string) int {
	if m := quarterRe.FindStringSubmatch(period); len(m) == 2 {
		var q int
		fmt.Sscanf(m[1], "%d", &q)
		return q
	}
	// "Mar. 31, 2021" / "Jun 30 2021" / "Sep. 30 2021" / "Dec 31, 2021"
	if m := humanDateRe.FindStringSubmatch(period); len(m) == 4 {
		mon := monthFromName(m[1])
		var day, yr int
		fmt.Sscanf(m[2], "%d", &day)
		fmt.Sscanf(m[3], "%d", &yr)
		_ = yr
		return quarterEndToNumber[[2]int{int(mon), day}]
	}
	return 0
}

func monthFromName(s string) time.Month {
	switch strings.ToLower(s) {
	case "jan":
		return time.January
	case "feb":
		return time.February
	case "mar":
		return time.March
	case "apr":
		return time.April
	case "may":
		return time.May
	case "jun":
		return time.June
	case "jul":
		return time.July
	case "aug":
		return time.August
	case "sep":
		return time.September
	case "oct":
		return time.October
	case "nov":
		return time.November
	case "dec":
		return time.December
	}
	return 0
}

// normalizeFormNumber strips "Form " prefix + whitespace, upcases.
func normalizeFormNumber(s string) string {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	return strings.TrimPrefix(s, "FORM")
}

// --- decimal helpers (big.Rat-based) ---

func mustDec(s string) *big.Rat {
	r := new(big.Rat)
	if _, ok := r.SetString(s); !ok {
		panic("bad decimal: " + s)
	}
	return r
}

func ratAbs(r *big.Rat) *big.Rat {
	out := new(big.Rat).Set(r)
	out.Abs(out)
	return out
}

// ratToFixed renders the rational with 2 decimal places. Input already has
// 2dp precision (penny-level), so this is exact — no banker's rounding.
func ratToFixed(r *big.Rat) string {
	if r == nil {
		return "0.00"
	}
	return r.FloatString(2)
}

func mustDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
