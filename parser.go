// Copyright (c) 2026 Joe Faron, KYD Networks, LLC
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"
)

// Filename patterns the scorer recognizes, in priority order:
//   1. ACTR-FORM-YEAR[-Qq].pdf     (TaxNow / TDS canonical export naming)
//   2. Account FORM YEAR Q#*.pdf   (alternate IRS portal export, e.g.
//                                   "Account 941 2021 Q1(13).pdf")
// Anything else returns empty - caller treats as bundled-shape and falls
// back to header-based form/period extraction.
var (
	filenameActrRe    = regexp.MustCompile(`(?i)ACTR-(\d{3,4}[A-Z]*)-(\d{4})(?:-Q(\d))?`)
	filenameAccountRe = regexp.MustCompile(`(?i)\bAccount[\s_-]+(\d{3,4}[A-Z]?)[\s_-]+(\d{4})(?:[\s_-]+Q(\d))?`)
)

// ParseFilename pulls (form, period) from an IRS Account Transcript filename.
// Returns ("","") when the filename doesn't fit any recognized convention.
func ParseFilename(fn string) (form, period string) {
	base := filepath.Base(fn)
	for _, re := range []*regexp.Regexp{filenameActrRe, filenameAccountRe} {
		if m := re.FindStringSubmatch(base); m != nil {
			form = strings.ToUpper(m[1])
			year := m[2]
			if len(m) > 3 && m[3] != "" {
				period = year + "Q" + m[3]
			} else {
				period = year
			}
			return
		}
	}
	return "", ""
}

// ParseStatus is the parse outcome for diagnostics; matches the Python pilot.
type ParseStatus int

const (
	ParseOK ParseStatus = iota
	ParseDataNotFound
	ParseNoTransactionsSection
)

func (s ParseStatus) String() string {
	switch s {
	case ParseOK:
		return "ok"
	case ParseDataNotFound:
		return "data_not_found"
	case ParseNoTransactionsSection:
		return "no_transactions_section"
	}
	return "unknown"
}

// ParsedModule is one IRS Account Transcript module pulled out of a (possibly
// bundled) PDF. For per-(form,period) TaxNow PDFs there's exactly one;
// for bundled PDFs (one PDF per entity concatenating every 941/1120/etc
// transcript across years), each module is split out from the "Page 1/N"
// boundary markers in-memory - no filesystem split needed.
type ParsedModule struct {
	Form         string // header-derived ("941", "1120S"); may be empty
	Period       string // header-derived ("2020Q1", "2021"); may be empty
	EinLast4     string // last 4 of EIN ("0976") - IRS transcripts mask the first 5
	IrsName      string // truncated taxpayer name as printed on the transcript
	Transactions []Transaction
	PageStart    int // 1-indexed
	PageEnd      int
}

// IngestResult describes what kind of PDF we got and (for Account
// Transcripts) the parsed modules + IRS-reported accrued penalty.
// Non-Account-Transcript PDFs (Tax Return Transcripts, 941-X amendments,
// filed-return forms, vendor reports, scanned images) are classified by
// Kind so the ingest-summary file can tell the user exactly what got
// ignored and why.
type IngestResult struct {
	Kind           string         // see classifyText for the value list
	Detail         string         // human-readable explanation
	AccruedPenalty string         // IRS "ACCRUED PENALTY:" field when present
	Modules        []ParsedModule // populated only when Kind == "AccountTranscript"
}

// IngestPDF opens a PDF once, extracts every page's text, classifies it,
// and (when it's an Account Transcript) splits into modules + scores them.
// Replaces back-to-back ParsePDFModules + classify-text calls that would
// have re-opened the same file. Resilient to per-page parse failures: if
// some pages error out (filled IRS forms with non-standard encoding often
// trip ledongthuc/pdf), we continue with the pages that did parse, then
// fall back to filename-based classification when nothing extracted.
func IngestPDF(path string) (IngestResult, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		// Couldn't even open it. Try filename-based classification as
		// a last resort so the user gets a useful message rather than
		// a raw library error.
		if k, d := classifyFilename(path); k != "" {
			return IngestResult{Kind: k, Detail: d + " (PDF library couldn't open the file)"}, nil
		}
		return IngestResult{
			Kind:   "Unparseable",
			Detail: fmt.Sprintf("PDF library could not open: %v", err),
		}, nil
	}
	defer f.Close()

	var pages []pageTextEntry
	totalChars := 0
	pageErrs := 0
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		txt, perr := p.GetPlainText(nil)
		if perr != nil {
			pageErrs++
			continue
		}
		pages = append(pages, pageTextEntry{idx: i, text: txt})
		totalChars += len(strings.TrimSpace(txt))
	}

	if totalChars == 0 {
		// No text extracted - either image-only (scanned) or a filled
		// form ledongthuc/pdf can't decode. Filename hints often
		// disambiguate these for the user.
		if k, d := classifyFilename(path); k != "" {
			suffix := " (no extractable text)"
			if pageErrs > 0 {
				suffix = fmt.Sprintf(" (%d page(s) failed PDF text extraction)", pageErrs)
			}
			return IngestResult{Kind: k, Detail: d + suffix}, nil
		}
		return IngestResult{
			Kind:   "ScannedImageOnly",
			Detail: "image-only PDF (no extractable text); needs OCR before scoring",
		}, nil
	}

	var sb strings.Builder
	for _, p := range pages {
		sb.WriteString(p.text)
	}
	full := sb.String()

	// Account Transcript path: must have BOTH the title and a TRANSACTIONS
	// section. "Account Transcript" alone matches Tax Return Transcripts
	// in some layouts, so we additionally require the transactions header.
	hasAcctTitle := strings.Contains(full, "Account Transcript") || strings.Contains(full, "ACCOUNT TRANSCRIPT")
	hasTxnHeader := strings.Contains(full, "TRANSACTIONS")
	if hasAcctTitle && hasTxnHeader {
		modules := splitModulesFromPages(pages)
		if len(modules) > 0 {
			return IngestResult{
				Kind:           "AccountTranscript",
				Modules:        modules,
				AccruedPenalty: extractAccruedPenalty(full),
			}, nil
		}
		// Has the title + section but no parseable modules - rare layout
		return IngestResult{
			Kind:   "AccountTranscriptUnparseable",
			Detail: "Account Transcript title found but no transactions parsed (layout variant)",
		}, nil
	}

	kind, detail := classifyNonTranscript(full)
	return IngestResult{Kind: kind, Detail: detail}, nil
}

type pageTextEntry struct {
	idx  int
	text string
}

// splitModulesFromPages walks page texts, splits at "Page 1/N" boundaries,
// parses each module's TRANSACTIONS section + header. Extracted from
// ParsePDFModules so IngestPDF can reuse it without re-opening the file.
func splitModulesFromPages(pages []pageTextEntry) []ParsedModule {
	if len(pages) == 0 {
		return nil
	}
	var starts []int
	for i, p := range pages {
		if pageOneOfNRe.MatchString(p.text) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		starts = []int{0}
	}
	var modules []ParsedModule
	for i, s := range starts {
		e := len(pages) - 1
		if i+1 < len(starts) {
			e = starts[i+1] - 1
		}
		var sb strings.Builder
		for k := s; k <= e; k++ {
			sb.WriteString(pages[k].text)
		}
		text := sb.String()
		if strings.Contains(text, "Requested data not found") ||
			strings.Contains(text, "No record of return filed") {
			continue
		}
		txs, status := parseTransactionsFromText(text)
		if status != ParseOK {
			continue
		}
		form, period, einLast4, irsName := extractModuleHeader(pages[s].text)
		modules = append(modules, ParsedModule{
			Form:         form,
			Period:       period,
			EinLast4:     einLast4,
			IrsName:      irsName,
			Transactions: txs,
			PageStart:    pages[s].idx,
			PageEnd:      pages[e].idx,
		})
	}
	return modules
}

// classifyFilename labels a PDF based on the filename alone - used when the
// PDF library can't extract text or fails entirely. Returns empty strings
// when the filename has no recognizable hint. Patterns mirror the in-text
// classifier so users get consistent labels regardless of which path fired.
func classifyFilename(path string) (kind, detail string) {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case strings.Contains(base, "941x") || strings.Contains(base, "941-x"):
		return "Form941X", "Form 941-X amendment (filed return, not a transcript)"
	case strings.Contains(base, "form 941 original") || strings.Contains(base, "941_original"):
		return "Form941Original", "filed Form 941 return (not a transcript)"
	case strings.Contains(base, "tax-return-transcript") || strings.Contains(base, "tax_return_transcript"):
		return "TaxReturnTranscript", "Tax Return Transcript (different IRS doc; no penalty assessment data)"
	case strings.Contains(base, "_report_"):
		return "VendorReport", "vendor / lender summary report (not IRS data)"
	}
	return "", ""
}

// classifyNonTranscript labels a PDF that didn't match the Account Transcript
// shape. Markers checked in priority order so "Tax Return Transcript" doesn't
// trip the 941-X branch when both strings happen to appear in the same doc.
func classifyNonTranscript(text string) (kind, detail string) {
	switch {
	case strings.Contains(text, "Tax Return Transcript"):
		return "TaxReturnTranscript",
			"Tax Return Transcript (different IRS doc; no penalty assessment data)"
	case strings.Contains(text, "Form 941-X") || strings.Contains(text, "941-X:") ||
		strings.Contains(text, "Adjusted Employer"):
		return "Form941X",
			"Form 941-X amendment (filed return, not a transcript)"
	case strings.Contains(text, "Report for this Quarter") ||
		strings.Contains(text, "Employer's QUARTERLY Federal Tax Return"):
		return "Form941Original",
			"filed Form 941 return (not a transcript)"
	case strings.Contains(text, "Lender:"):
		return "VendorReport",
			"vendor / lender summary report (not IRS data)"
	}
	return "Unrecognized",
		"no recognized IRS Account Transcript markers"
}

// extractAccruedPenalty pulls the IRS-reported "ACCRUED PENALTY:" header
// field. Returns "" if absent (some layouts omit it). Useful as an
// independent CPA cross-check against the scored finding total.
func extractAccruedPenalty(text string) string {
	idx := strings.Index(text, "ACCRUED PENALTY:")
	if idx < 0 {
		return ""
	}
	tail := text[idx+len("ACCRUED PENALTY:"):]
	for _, line := range strings.Split(tail, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "$") {
			return line
		}
		break
	}
	return ""
}

// ParsePDFModules opens a PDF and returns one ParsedModule per IRS module
// inside. Boundary logic:
//   - "Page 1 / N" header marks the start of each module's first page
//   - "Form X Account Transcript" line on that page yields form number
//   - "Report for Tax Period Ending: MM-DD-YYYY" yields period (Q1-Q4 for
//     quarterly forms, just YYYY for annual)
//
// Falls back to single-module mode (one module spanning all pages) when no
// boundary markers are found - typical for TaxNow per-(form,period) PDFs.
// Caller should derive form/period from the filename via ParseFilename for
// single-module results when header parsing returns empty strings.
func ParsePDFModules(path string) ([]ParsedModule, ParseStatus, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, ParseStatus(0), fmt.Errorf("open pdf %s: %w", path, err)
	}
	defer f.Close()

	type pageT struct {
		idx  int
		text string
	}
	var pages []pageT
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		txt, perr := p.GetPlainText(nil)
		if perr != nil {
			return nil, ParseStatus(0), fmt.Errorf("text page %d %s: %w", i, path, perr)
		}
		pages = append(pages, pageT{idx: i, text: txt})
	}
	if len(pages) == 0 {
		return nil, ParseNoTransactionsSection, nil
	}

	var starts []int
	for i, p := range pages {
		if pageOneOfNRe.MatchString(p.text) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		starts = []int{0} // single module spans all pages
	}

	var modules []ParsedModule
	for i, s := range starts {
		e := len(pages) - 1
		if i+1 < len(starts) {
			e = starts[i+1] - 1
		}
		var sb strings.Builder
		for k := s; k <= e; k++ {
			sb.WriteString(pages[k].text)
		}
		text := sb.String()

		if strings.Contains(text, "Requested data not found") ||
			strings.Contains(text, "No record of return filed") {
			continue
		}
		txs, status := parseTransactionsFromText(text)
		if status != ParseOK {
			continue
		}
		form, period, einLast4, irsName := extractModuleHeader(pages[s].text)
		modules = append(modules, ParsedModule{
			Form:         form,
			Period:       period,
			EinLast4:     einLast4,
			IrsName:      irsName,
			Transactions: txs,
			PageStart:    pages[s].idx,
			PageEnd:      pages[e].idx,
		})
	}
	if len(modules) == 0 {
		return nil, ParseDataNotFound, nil
	}
	return modules, ParseOK, nil
}

// ParsePDFToTransactions is the legacy single-module entry point. Concatenates
// every page and parses one TRANSACTIONS section. Kept for callers that don't
// need module splitting (the test suite, and any single-module sanity check).
// Production code paths should use ParsePDFModules so bundled PDFs work.
func ParsePDFToTransactions(path string) ([]Transaction, ParseStatus, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, ParseStatus(0), fmt.Errorf("open pdf %s: %w", path, err)
	}
	defer f.Close()

	var sb strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		txt, perr := p.GetPlainText(nil)
		if perr != nil {
			return nil, ParseStatus(0), fmt.Errorf("text page %d %s: %w", i, path, perr)
		}
		sb.WriteString(txt)
	}
	text := sb.String()

	if strings.Contains(text, "Requested data not found") || strings.Contains(text, "No record of return filed") {
		return nil, ParseDataNotFound, nil
	}
	txs, status := parseTransactionsFromText(text)
	return txs, status, nil
}

// parseTransactionsFromText is the shared body: walks the TRANSACTIONS
// section line-by-line, collects TC blocks until amount line, resolves
// metadata (cycle/dln/dates/desc) per transaction. Used by both the
// single-module and bundled paths.
func parseTransactionsFromText(text string) ([]Transaction, ParseStatus) {
	idx := strings.Index(text, "TRANSACTIONS")
	if idx < 0 {
		return nil, ParseNoTransactionsSection
	}
	body := text[idx:]
	rawLines := strings.Split(body, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, l := range rawLines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		lines = append(lines, l)
	}

	type bucket struct {
		tc    int
		lines []string
		amt   *big.Rat
		hasAm bool
	}
	var buckets []*bucket
	var cur *bucket

	for _, line := range lines {
		switch line {
		case "TRANSACTIONS", "CODE", "EXPLANATION OF TRANSACTION", "CYCLE", "DATE", "AMOUNT":
			continue
		}
		if strings.HasPrefix(line, "Page ") {
			continue
		}
		if line == "This Product Contains Sensitive Taxpayer Data" {
			continue
		}

		if a, ok := matchAmount(line); ok && cur != nil {
			cur.amt = a
			cur.hasAm = true
			cur = nil
			continue
		}
		if tc, ok := matchTC(line); ok {
			cur = &bucket{tc: tc}
			buckets = append(buckets, cur)
			continue
		}
		if cur == nil {
			continue
		}
		cur.lines = append(cur.lines, line)
	}

	out := make([]Transaction, 0, len(buckets))
	for _, b := range buckets {
		if !b.hasAm {
			continue
		}
		tx := Transaction{TC: b.tc, Amount: b.amt}
		extractTxMeta(&tx, b.lines)
		out = append(out, tx)
	}
	return out, ParseOK
}

// Module-boundary + header regexes. Two header layouts in the wild:
//   - Title-style (bundled multi-transcript export): "Form 941 Account
//     Transcript" / "Report for Tax Period Ending: MM-DD-YYYY".
//   - Field-style (IRS portal single-module export): "FORM NUMBER:\n941"
//     and "TAX PERIOD:\nMar. 31, 2021" with each label/value on its own
//     line. extractModuleHeader tries both.
var (
	tcRe                 = regexp.MustCompile(`^\d{3}$`)
	amountRe             = regexp.MustCompile(`^(-?)\$([\d,]+\.\d{2})$`)
	cycleRe              = regexp.MustCompile(`\b(\d{6})\b`)
	cycleStartRe         = regexp.MustCompile(`^\d{6}\b`)
	dlnRe                = regexp.MustCompile(`\b(\d{5}-\d{3}-\d{5}-\d)\b`)
	dateRe               = regexp.MustCompile(`\b(\d{2}-\d{2}-\d{4})\b`)
	leadDigRe            = regexp.MustCompile(`^[\d\-\$\.]`)
	pageOneOfNRe         = regexp.MustCompile(`(?m)^\s*Page\s+1\s*/\s*\d+`)
	formAcctTranscriptRe = regexp.MustCompile(`(?i)Form\s+(\d{3,4}[A-Z]?)\s+Account\s+Transcript`)
	taxPeriodEndingRe    = regexp.MustCompile(`Report for Tax Period Ending:\s*\n?\s*(\d{2})-(\d{2})-(\d{4})`)
	formNumberFieldRe    = regexp.MustCompile(`(?i)FORM\s+NUMBER\s*:\s*\n?\s*(\d{3,4}[A-Z]?)`)
	taxPeriodFieldRe     = regexp.MustCompile(`(?i)TAX\s+PERIOD\s*:\s*\n?\s*([A-Za-z]+)\.?\s*(\d{1,2}),?\s*(\d{4})`)
	// IRS transcripts mask the first 5 digits of the EIN:
	//   "Taxpayer Identification Number:\nXX-XXX0976"
	// or the alternate field layout:
	//   "TAXPAYER IDENTIFICATION NUMBER:\nXX-XXX0976"
	taxpayerIdRe = regexp.MustCompile(`(?i)Taxpayer\s+Identification\s+Number\s*:\s*\n?\s*X{2}-?X{3}(\d{4})`)
	// First non-blank line right after the masked EIN is the truncated
	// taxpayer name as printed on the transcript (e.g. "AER CRAF IN").
	taxpayerNameAfterEinRe = regexp.MustCompile(`(?i)Taxpayer\s+Identification\s+Number\s*:\s*\n?\s*X{2}-?X{3}\d{4}\s*\n([A-Z0-9 &\.,\-]+?)\s*\n`)
)

// monthAbbrevToInt resolves "Jan", "Mar.", "September" etc. to 1-12.
// Returns 0 on no match.
func monthAbbrevToInt(s string) int {
	switch strings.ToLower(s)[:3] {
	case "jan":
		return 1
	case "feb":
		return 2
	case "mar":
		return 3
	case "apr":
		return 4
	case "may":
		return 5
	case "jun":
		return 6
	case "jul":
		return 7
	case "aug":
		return 8
	case "sep":
		return 9
	case "oct":
		return 10
	case "nov":
		return 11
	case "dec":
		return 12
	}
	return 0
}

// QuarterlyFormSet identifies forms that need a quarter (Q1-Q4) appended
// to their period. Only 941 today; 720 also files quarterly but real-world
// bundled-PDF pulls have only ever shown 941 in this position.
var QuarterlyFormSet = map[string]bool{"941": true}

// extractModuleHeader reads the first-page text of a module and returns
// the form number + period. Tries the title-style layout first ("Form 941
// Account Transcript" + "Report for Tax Period Ending: MM-DD-YYYY"); falls
// back to the field-style layout ("FORM NUMBER:\n941" + "TAX PERIOD:\nMar.
// 31, 2021"). Either may be empty when the page lacks a recognized header;
// caller should then fall back to filename-derived values.
func extractModuleHeader(firstPageText string) (form, period, einLast4, irsName string) {
	month, year := 0, 0

	if mf := formAcctTranscriptRe.FindStringSubmatch(firstPageText); mf != nil {
		form = strings.ToUpper(strings.ReplaceAll(mf[1], "-", ""))
	} else if mf := formNumberFieldRe.FindStringSubmatch(firstPageText); mf != nil {
		form = strings.ToUpper(strings.ReplaceAll(mf[1], "-", ""))
	}

	if mp := taxPeriodEndingRe.FindStringSubmatch(firstPageText); mp != nil {
		fmt.Sscanf(mp[1], "%d", &month)
		fmt.Sscanf(mp[3], "%d", &year)
	} else if mp := taxPeriodFieldRe.FindStringSubmatch(firstPageText); mp != nil {
		month = monthAbbrevToInt(mp[1])
		fmt.Sscanf(mp[3], "%d", &year)
	}

	if year > 0 {
		if QuarterlyFormSet[form] && month > 0 {
			q := (month-1)/3 + 1
			period = fmt.Sprintf("%dQ%d", year, q)
		} else {
			period = fmt.Sprintf("%d", year)
		}
	}

	if mE := taxpayerIdRe.FindStringSubmatch(firstPageText); mE != nil {
		einLast4 = mE[1]
	}
	if mN := taxpayerNameAfterEinRe.FindStringSubmatch(firstPageText); mN != nil {
		irsName = strings.TrimSpace(mN[1])
	}
	return
}

func matchTC(line string) (int, bool) {
	if !tcRe.MatchString(line) {
		return 0, false
	}
	var n int
	fmt.Sscanf(line, "%d", &n)
	return n, true
}

func matchAmount(line string) (*big.Rat, bool) {
	m := amountRe.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	num := strings.ReplaceAll(m[2], ",", "")
	r := new(big.Rat)
	if _, ok := r.SetString(num); !ok {
		return nil, false
	}
	if m[1] == "-" {
		r.Neg(r)
	}
	return r, true
}

// extractTxMeta walks the metadata lines collected between a TC and its
// amount, pulling cycle/dln/dates/description in the same order Python does.
// Real assessment date = LAST date encountered (sits next to amount in the
// original IRS layout); future dates (year >= 2030) are interest-projection
// markers and are skipped from the "real" pool but still considered as
// fallback if no real date found.
func extractTxMeta(tx *Transaction, lines []string) {
	var dates []string
	var realDates []string
	var descParts []string

	for _, l := range lines {
		if md := dlnRe.FindStringSubmatch(l); md != nil && tx.DLN == "" {
			tx.DLN = md[1]
		}
		// Cycle: 6-digit number at the START of a metadata line.
		// Python check: re.match(r'^\d{6}\b', l) AND search-cycle hit.
		if tx.Cycle == "" && cycleStartRe.MatchString(l) {
			if mc := cycleRe.FindStringSubmatch(l); mc != nil {
				tx.Cycle = mc[1]
			}
		}
		if dts := dateRe.FindAllStringSubmatch(l, -1); len(dts) > 0 {
			for _, d := range dts {
				if d[1] == "00-00-0000" {
					continue
				}
				dates = append(dates, d[1])
				if year(d[1]) < 2030 {
					realDates = append(realDates, d[1])
				}
			}
		}
		// description = lines that are pure prose (no leading digit-ish run)
		if !leadDigRe.MatchString(l) && l != "00-00-0000" {
			descParts = append(descParts, l)
		}
	}

	tx.Description = strings.TrimSpace(strings.Join(descParts, " "))

	var pickStr string
	switch {
	case len(realDates) > 0:
		pickStr = realDates[len(realDates)-1]
	case len(dates) > 0:
		pickStr = dates[len(dates)-1]
	}
	if pickStr != "" {
		if d, err := time.Parse("01-02-2006", pickStr); err == nil {
			tx.Date = &d
		}
	}
}

func year(mmDDYYYY string) int {
	parts := strings.Split(mmDDYYYY, "-")
	if len(parts) != 3 {
		return 0
	}
	var y int
	fmt.Sscanf(parts[2], "%d", &y)
	return y
}
