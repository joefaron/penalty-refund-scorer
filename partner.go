// Copyright (c) 2026 Joe Faron, KYD Networks, LLC
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
)

// rebucketByEin merges entities that share an EIN last-4 into a single
// entity. Findings, by-TC totals, transcript counts, and skipped-reason
// counts are all rolled up. Entities with no extractable EIN go into an
// "unknown_ein" bucket so nothing silently disappears.
//
// Use case: partner archives where every PDF lives in a flat directory and
// the folder/filename can't be relied on to bucket. The EIN inside the
// transcript header is the truth.
func rebucketByEin(entities []entityResult) []entityResult {
	byKey := map[string]*entityResult{}
	keys := []string{} // preserve first-seen order
	for i := range entities {
		e := entities[i]
		key := e.EinLast4
		if key == "" {
			key = "unknown_ein"
		}
		if existing, ok := byKey[key]; ok {
			mergeInto(existing, e)
			continue
		}
		// First sighting for this key — keep a copy under the new key.
		// Name becomes the EIN-last-4 (or "unknown_ein") so the output is
		// consistent regardless of source folder naming.
		copy := e
		copy.Name = key
		byKey[key] = &copy
		keys = append(keys, key)
	}
	out := make([]entityResult, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}

// mergeInto folds src's totals into dst in place. Used by rebucketByEin
// when two PDFs from different folders turn out to share an EIN.
func mergeInto(dst *entityResult, src entityResult) {
	dst.TranscriptsTotal += src.TranscriptsTotal
	dst.TranscriptsWithFindings += src.TranscriptsWithFindings
	dst.FindingCount += src.FindingCount
	if dst.OurTotal == nil {
		dst.OurTotal = new(big.Rat)
	}
	if src.OurTotal != nil {
		dst.OurTotal.Add(dst.OurTotal, src.OurTotal)
	}
	if dst.ByTC == nil {
		dst.ByTC = map[int]*big.Rat{}
	}
	for tc, amt := range src.ByTC {
		if dst.ByTC[tc] == nil {
			dst.ByTC[tc] = new(big.Rat)
		}
		dst.ByTC[tc].Add(dst.ByTC[tc], amt)
	}
	if dst.Skipped == nil {
		dst.Skipped = map[string]int{}
	}
	for reason, count := range src.Skipped {
		dst.Skipped[reason] += count
	}
	dst.Findings = append(dst.Findings, src.Findings...)
	// Preserve EIN + IRS name from the first sighting if dst lacks them
	// (src may have populated either after a later parse).
	if dst.EinLast4 == "" {
		dst.EinLast4 = src.EinLast4
	}
	if dst.IrsName == "" {
		dst.IrsName = src.IrsName
	}
}

// applyHMACToEntities replaces every identifying field with an HMAC-SHA256
// hash of the EIN (or the original entity name when EIN is absent). Used
// for partner-handoff workflows where the partner runs the scorer and the
// resulting CSV travels back to us without raw PII.
//
// Output: entity = 16-hex-char HMAC; ein_last4 = ""; irs_name = "";
// every finding's source PDF filename is also hashed.
func applyHMACToEntities(entities []entityResult, key string) {
	for i := range entities {
		e := &entities[i]
		identifier := e.EinLast4
		if identifier == "" {
			identifier = e.Name
		}
		e.Name = hmacHex(key, identifier)
		e.EinLast4 = ""
		e.IrsName = ""
		for j := range e.Findings {
			if e.Findings[j].SourceFile != "" {
				e.Findings[j].SourceFile = hmacHex(key, e.Findings[j].SourceFile)
			}
		}
	}
}

// hmacHex returns the first 16 hex characters of HMAC-SHA256(key, message).
// 64 bits is more than enough collision-resistance for partner handoffs
// where the population is bounded by a single book of business (thousands,
// not billions), and the short prefix keeps CSVs readable.
func hmacHex(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// bandFor maps a claimable-dollar total to a coarse band label.
//
//   0           -> "$0"
//   $1-$4,999   -> "$1-$4,999"
//   $5k-$25k    -> "$5,000-$24,999"
//   $25k-$100k  -> "$25,000-$99,999"
//   $100k+      -> "$100,000+"
//
// Used when --bands is set so partner CSVs convey rough ranking without
// disclosing exact dollar amounts.
func bandFor(total *big.Rat) string {
	if total == nil {
		return "$0"
	}
	zero := new(big.Rat)
	if total.Cmp(zero) == 0 {
		return "$0"
	}
	d5k, _ := new(big.Rat).SetString("5000")
	d25k, _ := new(big.Rat).SetString("25000")
	d100k, _ := new(big.Rat).SetString("100000")
	switch {
	case total.Cmp(d5k) < 0:
		return "$1-$4,999"
	case total.Cmp(d25k) < 0:
		return "$5,000-$24,999"
	case total.Cmp(d100k) < 0:
		return "$25,000-$99,999"
	default:
		return "$100,000+"
	}
}

// writeBandedCSV emits one row per entity, no per-finding rows, our_total
// replaced by a band label. Columns are tailored for partner handoff:
// stable entity identifier, summary counts, band label, finding count.
func writeBandedCSV(cw *csv.Writer, entities []entityResult) error {
	headers := []string{
		"entity",
		"transcripts",
		"with_findings",
		"finding_count",
		"claimable_band",
	}
	if err := cw.Write(headers); err != nil {
		return err
	}
	// Sort entities by claimable amount descending so the partner's CSV is
	// pre-prioritised. Bands collapse fine-grained ordering but within-band
	// ordering still helps when the band cap is high (e.g. $100k+).
	sort.SliceStable(entities, func(i, j int) bool {
		ai := entities[i].OurTotal
		aj := entities[j].OurTotal
		if ai == nil {
			ai = new(big.Rat)
		}
		if aj == nil {
			aj = new(big.Rat)
		}
		return ai.Cmp(aj) > 0
	})
	for _, e := range entities {
		row := []string{
			e.Name,
			fmt.Sprintf("%d", e.TranscriptsTotal),
			fmt.Sprintf("%d", e.TranscriptsWithFindings),
			fmt.Sprintf("%d", e.FindingCount),
			bandFor(e.OurTotal),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	return cw.Error()
}
