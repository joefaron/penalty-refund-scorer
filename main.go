// Copyright (c) 2026 Joe Faron, KYD Networks, LLC
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Penalty Refund scorer (Go).
//
// Modes:
//   --root <dir>        Walk subdirs; each is one entity, PDFs inside are
//                       scored, output written to --out (CSV).
//   --json <file>       Run scorer on synthetic transactions JSON. File shape:
//                         [{ "entity": "X", "form": "941", "period": "2020Q1",
//                            "source_file": "...",
//                            "transactions": [{tc,amount,date,description},...] }, ...]
//   --out <file>        Output CSV (default: stdout)
//   --json-out <file>   Optional per-entity findings JSON (parity with Python pilot)

func main() {
	var (
		root    = flag.String("root", "", "PDF root (per-entity subdirs)")
		jsonIn  = flag.String("json", "", "Synthetic transactions JSON input")
		out     = flag.String("out", "", "CSV output path (stdout if empty)")
		jsonOut = flag.String("json-out", "", "Optional per-entity JSON dir")
		minClm  = flag.String("min-claim", "", "Min claim threshold dollars (default 25, 0 = no floor)")
	)
	flag.Parse()

	if *minClm != "" {
		MinClaimThreshold = mustDec(*minClm)
	}
	if env := os.Getenv("COVID_REFUND_MIN_CLAIM"); env != "" && *minClm == "" {
		MinClaimThreshold = mustDec(env)
	}

	if (*root == "" && *jsonIn == "") || (*root != "" && *jsonIn != "") {
		fmt.Fprintln(os.Stderr, "must specify exactly one of --root or --json")
		flag.Usage()
		os.Exit(2)
	}

	// Default output: <root>/penalty-refund-findings.csv (or next to JSON input).
	// Lands the CSV right next to the data so non-CLI users can find it.
	if *out == "" {
		switch {
		case *root != "":
			*out = filepath.Join(*root, "penalty-refund-findings.csv")
		case *jsonIn != "":
			dir := filepath.Dir(*jsonIn)
			base := strings.TrimSuffix(filepath.Base(*jsonIn), filepath.Ext(*jsonIn))
			*out = filepath.Join(dir, base+"-findings.csv")
		}
	}

	var entities []entityResult
	var err error
	if *root != "" {
		entities, err = runPDFRoot(*root)
	} else {
		entities, err = runJSON(*jsonIn)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	writer := os.Stdout
	if *out != "" {
		f, ferr := os.Create(*out)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "create out:", ferr)
			os.Exit(1)
		}
		defer f.Close()
		writer = f
	}
	if werr := writeCSV(writer, entities); werr != nil {
		fmt.Fprintln(os.Stderr, "write csv:", werr)
		os.Exit(1)
	}
	if *out != "" {
		abs, _ := filepath.Abs(*out)
		fmt.Fprintf(os.Stderr, "\nWrote CSV: %s\n  %d entities, %d findings, total $%s\n",
			abs, len(entities), countFindings(entities), ratToFixed(sumTotals(entities)))

		// Write the ingest-summary sidecar alongside the CSV so the user
		// can see per-PDF classification (parsed / skipped / why). Only
		// applies to --root mode; --json input has no PDF classification.
		if len(rootIngest) > 0 {
			summary := strings.TrimSuffix(*out, filepath.Ext(*out)) + "-ingest-summary.txt"
			if serr := writeIngestSummary(summary, rootIngest, entities); serr != nil {
				fmt.Fprintln(os.Stderr, "write summary:", serr)
			} else {
				abs, _ := filepath.Abs(summary)
				fmt.Fprintf(os.Stderr, "Wrote summary: %s\n", abs)
			}
		}
	}

	if *jsonOut != "" {
		if jerr := writePerEntityJSON(*jsonOut, entities); jerr != nil {
			fmt.Fprintln(os.Stderr, "write json:", jerr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "wrote per-entity JSON to %s\n", *jsonOut)
	}
}

type entityResult struct {
	Name                    string    `json:"entity"`
	EinLast4                string    `json:"ein_last4,omitempty"`
	IrsName                 string    `json:"irs_name,omitempty"`
	TranscriptsTotal        int       `json:"transcripts_total"`
	TranscriptsWithFindings int       `json:"transcripts_with_findings"`
	FindingCount            int       `json:"finding_count"`
	OurTotal                *big.Rat  `json:"-"`
	ByTC                    map[int]*big.Rat `json:"-"`
	Skipped                 map[string]int `json:"skipped_summary"`
	Findings                []Finding `json:"findings"`
}

func newEntityResult(name string) *entityResult {
	return &entityResult{
		Name:    name,
		OurTotal: new(big.Rat),
		ByTC:    map[int]*big.Rat{},
		Skipped: map[string]int{},
	}
}

func (e *entityResult) absorb(form, period, source string, txs []Transaction) {
	res := ScoreTranscript(txs, form, period)
	for _, s := range res.Skipped {
		e.Skipped[s.Reason]++
	}
	if len(res.Kept) == 0 {
		return
	}
	e.TranscriptsWithFindings++
	for _, f := range res.Kept {
		f.Form = form
		f.Period = period
		f.SourceFile = source
		e.Findings = append(e.Findings, f)
		if e.ByTC[f.TC] == nil {
			e.ByTC[f.TC] = new(big.Rat)
		}
		e.ByTC[f.TC].Add(e.ByTC[f.TC], f.Amount)
		e.OurTotal.Add(e.OurTotal, f.Amount)
		e.FindingCount++
	}
}

// --- PDF mode ---

// runPDFRoot walks the root recursively and produces one entityResult per
// detected entity. Two ingest shapes coexist:
//
//   - TaxNow shape: <Entity>/ACTR-941-2020-Q1.pdf - the parent dir IS the
//     entity, all ACTR-named PDFs in it are that entity's transcripts.
//   - Bundled shape: <basename>.pdf where the filename is NOT ACTR-shaped
//     (multi-transcript per-entity PDFs). The PDF holds every transcript
//     for one entity; its basename (date-prefix stripped, hyphens
//     humanized) becomes the entity name.
//
// Detection is per-file: ACTR-shaped filenames bucket by parent dir;
// non-ACTR filenames bucket by their own basename. A directory mixing
// both shapes splits cleanly: ACTR files form one parent-dir entity,
// each bundled PDF forms its own.
func runPDFRoot(root string) ([]entityResult, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("--root must be a directory: %s", root)
	}

	// dirBucket: TaxNow-shape (ACTR-named) PDFs grouped by parent dir.
	// fileBucket: bundled-shape PDFs - one entity per PDF.
	dirBucket := map[string]*entityBucket{}
	var fileBucket []*entityBucket

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			fmt.Fprintf(os.Stderr, "[warn] walk %s: %v\n", path, werr)
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".pdf") {
			return nil
		}
		dir := filepath.Dir(path)
		fn := d.Name()
		if form, _ := ParseFilename(fn); form != "" {
			b, ok := dirBucket[dir]
			if !ok {
				name := filepath.Base(dir)
				if dir == root {
					name = filepath.Base(root)
				}
				b = &entityBucket{dir: dir, name: name}
				dirBucket[dir] = b
			}
			b.files = append(b.files, fn)
		} else {
			fileBucket = append(fileBucket, &entityBucket{
				dir:   dir,
				name:  humanizeBundledBasename(fn),
				files: []string{fn},
			})
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	if len(dirBucket) == 0 && len(fileBucket) == 0 {
		return nil, fmt.Errorf("no PDFs found under %s (recursive search)", root)
	}

	all := make([]*entityBucket, 0, len(dirBucket)+len(fileBucket))
	for _, b := range dirBucket {
		all = append(all, b)
	}
	all = append(all, fileBucket...)
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })

	var entities []entityResult
	var allEntries []IngestEntry
	for _, b := range all {
		ent, entries, perr := processEntityWithFiles(b.dir, b.name, b.files)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "[warn] %s: %v\n", b.name, perr)
			continue
		}
		allEntries = append(allEntries, entries...)
		// Drop entities with zero recognized Account Transcript modules
		// from the CSV. Their IngestEntry rows still land in the summary
		// file with the kind/detail explaining what they were instead.
		if ent.TranscriptsTotal == 0 {
			continue
		}
		entities = append(entities, *ent)
	}
	rootIngest = allEntries
	return entities, nil
}

// rootIngest carries per-PDF classification info from runPDFRoot to main()
// so the summary sidecar can list every input file. Package-level rather
// than threaded through the writeCSV/writeJSON signatures because it's
// a side-channel for the human-readable output, not part of the scoring
// data the CSV represents.
var rootIngest []IngestEntry

type entityBucket struct {
	dir   string
	name  string
	files []string
}

// humanizeBundledBasename strips the .pdf extension, leading date prefixes
// like "may6-" or "2026-05-06-", and converts hyphens/underscores to
// spaces, then title-cases. Matches the bash-side splitter behavior in
// bin/covid-pilot-bundle (lines 56-59) so a may6 dump scored via Go
// produces the same entity slugs the Drive bundle expects.
func humanizeBundledBasename(fn string) string {
	name := strings.TrimSuffix(fn, filepath.Ext(fn))
	name = bundledDatePrefixRe.ReplaceAllString(name, "")
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	parts := strings.Fields(name)
	for i, p := range parts {
		if len(p) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, " ")
}

var bundledDatePrefixRe = regexp.MustCompile(`(?i)^((jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)\d+|\d{4}-\d{2}-\d{2})-`)

func processEntityWithFiles(dir, entityName string, pdfs []string) (*entityResult, []IngestEntry, error) {
	if len(pdfs) == 0 {
		return nil, nil, fmt.Errorf("no PDFs")
	}
	sort.Strings(pdfs)

	// Two ingest shapes:
	//  - TaxNow shape: one ACTR-FORM-YEAR[-Q].pdf per (form, period). Filename
	//    carries form/period; PDF holds one module. Dedupe to the unsuffixed
	//    canonical filename per (form, period) before parsing.
	//  - Bundled shape: one PDF per entity concatenating every transcript
	//    (no ACTR filename, no per-period dedupe possible). Each page-1
	//    boundary becomes a separate module; (form, period) are read from
	//    the per-module header lines.
	taxNow := map[string]string{}
	bundled := []string{}
	for _, fn := range pdfs {
		form, period := ParseFilename(fn)
		if form == "" {
			bundled = append(bundled, fn)
			continue
		}
		key := form + "|" + period
		if _, ok := taxNow[key]; !ok {
			taxNow[key] = fn
		}
	}
	if len(taxNow) == 0 && len(bundled) == 0 {
		return nil, nil, fmt.Errorf("no usable PDFs in %s", dir)
	}

	ent := newEntityResult(entityName)
	var entries []IngestEntry
	if len(taxNow) > 0 {
		keys := make([]string, 0, len(taxNow))
		for k := range taxNow {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fn := taxNow[k]
			entries = append(entries, absorbModulesFromFile(ent, filepath.Join(dir, fn), fn, true))
		}
	}
	for _, fn := range bundled {
		entries = append(entries, absorbModulesFromFile(ent, filepath.Join(dir, fn), fn, false))
	}
	return ent, entries, nil
}

// absorbModulesFromFile parses a PDF (single-module or bundled) and feeds
// every recognized Account Transcript module to the entityResult. Returns
// an IngestEntry describing what was found - the caller aggregates these
// into the ingest-summary sidecar so the user can see exactly which input
// PDFs got scored, which got skipped, and why. filenameHasFormPeriod=true
// means the filename matches a recognized convention and overrides any
// header-derived form/period (filename is more reliable in that case).
func absorbModulesFromFile(ent *entityResult, path, fn string, filenameHasFormPeriod bool) IngestEntry {
	entry := IngestEntry{Filename: fn, Path: path}
	res, perr := IngestPDF(path)
	if perr != nil {
		ent.Skipped["parse_error: "+perr.Error()]++
		entry.Kind = "ParseError"
		entry.Detail = perr.Error()
		return entry
	}
	entry.Kind = res.Kind
	entry.Detail = res.Detail
	entry.AccruedPenalty = res.AccruedPenalty

	if res.Kind != "AccountTranscript" {
		ent.Skipped[res.Kind]++
		return entry
	}

	fnForm, fnPeriod := ParseFilename(fn)
	for _, m := range res.Modules {
		form, period := m.Form, m.Period
		if filenameHasFormPeriod && fnForm != "" {
			form, period = fnForm, fnPeriod
		}
		if form == "" || period == "" {
			ent.Skipped[fmt.Sprintf("module pages %d-%d: no form/period (filename '%s' also unparseable)", m.PageStart, m.PageEnd, fn)]++
			continue
		}
		ent.TranscriptsTotal++
		entry.Modules++
		entry.Transactions += len(m.Transactions)
		// Capture form/period for single-module PDFs (most common).
		if entry.Form == "" {
			entry.Form, entry.Period = form, period
		}
		// EIN + IRS truncated name: first non-empty wins. Real-world
		// transcripts within one entity carry the same EIN/name, so the
		// per-entity field is stable across modules.
		if ent.EinLast4 == "" && m.EinLast4 != "" {
			ent.EinLast4 = m.EinLast4
		}
		if ent.IrsName == "" && m.IrsName != "" {
			ent.IrsName = m.IrsName
		}
		ent.absorb(form, period, fn, m.Transactions)
	}
	// Findings count + total: read off the entity AFTER absorb. The entity
	// accumulates across ALL its files, so we capture per-file delta by
	// snapshotting before/after - but here we just record the per-module
	// transaction count; per-file findings flow into the summary via
	// matching the entity rows.
	return entry
}

// IngestEntry records what one input PDF was classified as and (if it
// scored) what came out. Used to write the per-PDF ingest summary.
type IngestEntry struct {
	Filename       string
	Path           string
	Kind           string // matches IngestResult.Kind
	Detail         string
	AccruedPenalty string
	Form           string
	Period         string
	Modules        int
	Transactions   int
	Findings       int      // filled in after scoring (via matching)
	Total          *big.Rat // filled in after scoring (via matching)
}

// --- JSON mode (synthetic mock data) ---

type synthInput struct {
	Entity       string             `json:"entity"`
	Form         string             `json:"form"`
	Period       string             `json:"period"`
	SourceFile   string             `json:"source_file"`
	Transactions []synthTransaction `json:"transactions"`
}

type synthTransaction struct {
	TC          int    `json:"tc"`
	Amount      string `json:"amount"`
	Date        string `json:"date"` // MM-DD-YYYY or empty
	Cycle       string `json:"cycle"`
	DLN         string `json:"dln"`
	Description string `json:"description"`
}

func runJSON(path string) ([]entityResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inputs []synthInput
	if jerr := json.Unmarshal(raw, &inputs); jerr != nil {
		return nil, fmt.Errorf("parse %s: %w", path, jerr)
	}

	byEntity := map[string]*entityResult{}
	for _, in := range inputs {
		ent, ok := byEntity[in.Entity]
		if !ok {
			ent = newEntityResult(in.Entity)
			byEntity[in.Entity] = ent
		}
		ent.TranscriptsTotal++
		txs := make([]Transaction, 0, len(in.Transactions))
		for _, t := range in.Transactions {
			tx := Transaction{
				TC:          t.TC,
				Description: t.Description,
				Cycle:       t.Cycle,
				DLN:         t.DLN,
				Amount:      mustDec(t.Amount),
			}
			if t.Date != "" && t.Date != "00-00-0000" {
				if dt, derr := parseAnyDate(t.Date); derr == nil {
					tx.Date = &dt
				}
			}
			if tx.Amount == nil {
				tx.Amount = new(big.Rat)
			}
			txs = append(txs, tx)
		}
		ent.absorb(in.Form, in.Period, in.SourceFile, txs)
	}

	keys := make([]string, 0, len(byEntity))
	for k := range byEntity {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]entityResult, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byEntity[k])
	}
	return out, nil
}

// parseAnyDate accepts MM-DD-YYYY (canonical), MM/DD/YYYY, YYYY-MM-DD.
// Returns the first format that parses cleanly, matching the canonical
// parse_date helper (prompt-drift tolerance).
func parseAnyDate(s string) (time.Time, error) {
	for _, fmtStr := range []string{"01-02-2006", "01/02/2006", "2006-01-02"} {
		if dt, err := time.Parse(fmtStr, s); err == nil {
			return dt, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", s)
}

// --- CSV output ---

// writeCSV emits one row per finding (with per-entity totals repeated), plus
// 0-finding entities get one summary row with empty finding fields. Mirrors
// pilot_comparison_detailed.csv columns.
func writeCSV(w io.Writer, entities []entityResult) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	headers := []string{
		"entity", "ein_last4", "irs_name",
		"transcripts", "with_findings",
		"our_total", "by_tc",
		"finding_idx", "finding_form", "finding_period",
		"finding_tc", "finding_label", "finding_amount",
		"finding_date", "finding_source_pdf",
	}
	if err := cw.Write(headers); err != nil {
		return err
	}

	for _, e := range entities {
		base := []string{
			e.Name,
			e.EinLast4,
			e.IrsName,
			fmt.Sprintf("%d", e.TranscriptsTotal),
			fmt.Sprintf("%d", e.TranscriptsWithFindings),
			ratToFixed(e.OurTotal),
			byTCJSON(e.ByTC),
		}
		if len(e.Findings) == 0 {
			cw.Write(append(base, "", "", "", "", "", "", "", ""))
			continue
		}
		// sort findings by amount desc to mirror build_detailed_comparison.py
		sorted := make([]Finding, len(e.Findings))
		copy(sorted, e.Findings)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].Amount.Cmp(sorted[j].Amount) > 0
		})
		for i, f := range sorted {
			row := append([]string{}, base...)
			label := TCLabels[f.TC]
			date := ""
			if f.Date != nil {
				date = f.Date.Format("01-02-2006")
			}
			row = append(row,
				fmt.Sprintf("%d", i+1),
				f.Form, f.Period,
				fmt.Sprintf("%d", f.TC),
				label,
				ratToFixed(f.Amount),
				date,
				f.SourceFile,
			)
			cw.Write(row)
		}
	}
	return cw.Error()
}

func byTCJSON(m map[int]*big.Rat) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%d":"%s"`, k, ratToFixed(m[k]))
	}
	b.WriteString("}")
	return b.String()
}

// writeIngestSummary writes a human-readable per-PDF classification report
// alongside the CSV. Lists every input PDF as either:
//   - parsed Account Transcript with form/period + finding count + total
//   - skipped (Tax Return Transcript / 941-X / filed return / vendor / scan)
// Plus an aggregate footer. Lets CPAs see what got eaten and what got
// ignored without having to read stderr or open per-entity JSONs.
func writeIngestSummary(path string, entries []IngestEntry, entities []entityResult) error {
	// Build a per-(filename) findings + total map by walking the scored
	// findings. One file may produce findings under multiple form-period
	// rows (rare) so sum across.
	perFileFindings := map[string]int{}
	perFileTotal := map[string]*big.Rat{}
	for _, e := range entities {
		for _, f := range e.Findings {
			perFileFindings[f.SourceFile]++
			if perFileTotal[f.SourceFile] == nil {
				perFileTotal[f.SourceFile] = new(big.Rat)
			}
			perFileTotal[f.SourceFile].Add(perFileTotal[f.SourceFile], f.Amount)
		}
	}

	var parsed, skipped []IngestEntry
	for _, e := range entries {
		if e.Kind == "AccountTranscript" {
			e.Findings = perFileFindings[e.Filename]
			if t := perFileTotal[e.Filename]; t != nil {
				e.Total = t
			}
			parsed = append(parsed, e)
		} else {
			skipped = append(skipped, e)
		}
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].Filename < parsed[j].Filename })
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Filename < skipped[j].Filename })

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	grandTotal := sumTotals(entities)
	fmt.Fprintln(f, "PENALTY REFUND SCORER - INGEST SUMMARY")
	fmt.Fprintln(f, "======================================")
	fmt.Fprintf(f, "Files scanned: %d\n", len(entries))
	fmt.Fprintf(f, "  Account Transcripts parsed: %d\n", len(parsed))
	fmt.Fprintf(f, "  Non-transcript files skipped: %d\n", len(skipped))
	fmt.Fprintf(f, "Total claimable across all entities: $%s\n", ratToFixed(grandTotal))
	fmt.Fprintln(f)

	if len(parsed) > 0 {
		fmt.Fprintln(f, "ACCOUNT TRANSCRIPTS PARSED (eligible for penalty refund analysis)")
		fmt.Fprintln(f, "-----------------------------------------------------------------")
		for _, e := range parsed {
			label := fmt.Sprintf("%s %s", e.Form, e.Period)
			if e.Modules > 1 {
				label = fmt.Sprintf("%d modules in one PDF", e.Modules)
			}
			total := "$0.00"
			if e.Total != nil {
				total = "$" + ratToFixed(e.Total)
			}
			fmt.Fprintf(f, "  [OK] %s\n", e.Filename)
			fmt.Fprintf(f, "       %s | %d transactions parsed | %d penalty findings | claimable %s\n",
				label, e.Transactions, e.Findings, total)
			if e.AccruedPenalty != "" {
				fmt.Fprintf(f, "       IRS-reported ACCRUED PENALTY field: %s\n", e.AccruedPenalty)
			}
			fmt.Fprintln(f)
		}
	}

	if len(skipped) > 0 {
		fmt.Fprintln(f, "NON-TRANSCRIPT FILES SKIPPED (no penalty data to score)")
		fmt.Fprintln(f, "-------------------------------------------------------")
		fmt.Fprintln(f, "  These PDFs are not IRS Account Transcripts and were not scored.")
		fmt.Fprintln(f, "  If any of these SHOULD have been a transcript, the file may be")
		fmt.Fprintln(f, "  the wrong export type from TaxNow/IRS - re-pull as Account Transcript.")
		fmt.Fprintln(f)
		// Group by Kind for readability
		byKind := map[string][]IngestEntry{}
		var kinds []string
		for _, e := range skipped {
			if _, ok := byKind[e.Kind]; !ok {
				kinds = append(kinds, e.Kind)
			}
			byKind[e.Kind] = append(byKind[e.Kind], e)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			es := byKind[k]
			fmt.Fprintf(f, "  %s (%d file(s)): %s\n", kindLabel(k), len(es), es[0].Detail)
			for _, e := range es {
				fmt.Fprintf(f, "    - %s\n", e.Filename)
			}
			fmt.Fprintln(f)
		}
	}

	fmt.Fprintln(f, "----")
	fmt.Fprintln(f, "Generated by penalty-refund-scorer (Go).")
	fmt.Fprintln(f, "For questions or unexpected results, ping Joe with this file + the source PDFs.")
	return nil
}

func kindLabel(kind string) string {
	switch kind {
	case "TaxReturnTranscript":
		return "Tax Return Transcript"
	case "Form941X":
		return "Form 941-X amendment"
	case "Form941Original":
		return "Filed Form 941 (original return)"
	case "VendorReport":
		return "Vendor / lender report"
	case "ScannedImageOnly":
		return "Scanned image-only PDF"
	case "AccountTranscriptUnparseable":
		return "Account Transcript (layout variant, not parsed)"
	case "Unrecognized":
		return "Unrecognized PDF"
	case "ParseError":
		return "Parse error"
	}
	return kind
}

// --- per-entity JSON output (pilot parity) ---

func writePerEntityJSON(dir string, entities []entityResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range entities {
		out := struct {
			Entity                  string                 `json:"entity"`
			TranscriptsTotal        int                    `json:"transcripts_total"`
			TranscriptsWithFindings int                    `json:"transcripts_with_findings"`
			FindingCount            int                    `json:"finding_count"`
			OurTotal                string                 `json:"our_total"`
			ByTC                    map[string]string      `json:"by_tc"`
			SkippedSummary          map[string]int         `json:"skipped_summary"`
			Findings                []map[string]any       `json:"findings"`
		}{
			Entity:                  e.Name,
			TranscriptsTotal:        e.TranscriptsTotal,
			TranscriptsWithFindings: e.TranscriptsWithFindings,
			FindingCount:            e.FindingCount,
			OurTotal:                ratToFixed(e.OurTotal),
			SkippedSummary:          e.Skipped,
			ByTC:                    map[string]string{},
		}
		keys := make([]int, 0, len(e.ByTC))
		for k := range e.ByTC {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			out.ByTC[fmt.Sprintf("%d", k)] = ratToFixed(e.ByTC[k])
		}
		for _, f := range e.Findings {
			date := ""
			if f.Date != nil {
				date = f.Date.Format("01-02-2006")
			}
			out.Findings = append(out.Findings, map[string]any{
				"tc":          f.TC,
				"description": f.Description,
				"cycle":       f.Cycle,
				"dln":         f.DLN,
				"date":        date,
				"amount":      ratToFixed(f.Amount),
				"source_file": f.SourceFile,
				"form":        f.Form,
				"period":      f.Period,
			})
		}

		safeName := strings.ReplaceAll(e.Name, "/", "_")
		path := filepath.Join(dir, safeName+".json")
		buf, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		if werr := os.WriteFile(path, buf, 0o644); werr != nil {
			return werr
		}
	}
	return nil
}

// --- helpers ---

func countFindings(entities []entityResult) int {
	n := 0
	for _, e := range entities {
		n += e.FindingCount
	}
	return n
}

func sumTotals(entities []entityResult) *big.Rat {
	out := new(big.Rat)
	for _, e := range entities {
		if e.OurTotal != nil {
			out.Add(out, e.OurTotal)
		}
	}
	return out
}
