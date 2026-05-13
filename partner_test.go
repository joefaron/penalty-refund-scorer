// Copyright (c) 2026 Joe Faron, KYD Networks, LLC
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/csv"
	"math/big"
	"strings"
	"testing"
)

func TestRebucketByEin(t *testing.T) {
	// Two entities with the same EIN should merge; a third with a different
	// EIN stays separate; one with no EIN goes to unknown_ein.
	entities := []entityResult{
		{
			Name: "Acme A", EinLast4: "1234",
			TranscriptsTotal: 2, TranscriptsWithFindings: 1, FindingCount: 1,
			OurTotal: big.NewRat(1000, 1),
			ByTC:     map[int]*big.Rat{166: big.NewRat(1000, 1)},
			Skipped:  map[string]int{"reason A": 1},
		},
		{
			Name: "Acme B", EinLast4: "1234",
			TranscriptsTotal: 3, TranscriptsWithFindings: 2, FindingCount: 2,
			OurTotal: big.NewRat(500, 1),
			ByTC:     map[int]*big.Rat{166: big.NewRat(500, 1), 196: big.NewRat(100, 1)},
			Skipped:  map[string]int{"reason A": 1, "reason B": 1},
		},
		{
			Name: "Other Inc", EinLast4: "9999",
			TranscriptsTotal: 1, TranscriptsWithFindings: 1, FindingCount: 1,
			OurTotal: big.NewRat(2000, 1),
			ByTC:     map[int]*big.Rat{186: big.NewRat(2000, 1)},
			Skipped:  map[string]int{},
		},
		{
			Name: "Mystery", EinLast4: "",
			TranscriptsTotal: 1, TranscriptsWithFindings: 0, FindingCount: 0,
			OurTotal: new(big.Rat),
			ByTC:     map[int]*big.Rat{},
			Skipped:  map[string]int{},
		},
	}
	out := rebucketByEin(entities)
	if len(out) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(out))
	}

	var merged, other, unknown *entityResult
	for i := range out {
		switch out[i].Name {
		case "1234":
			merged = &out[i]
		case "9999":
			other = &out[i]
		case "unknown_ein":
			unknown = &out[i]
		}
	}
	if merged == nil || other == nil || unknown == nil {
		t.Fatalf("missing bucket: %+v", out)
	}
	if merged.TranscriptsTotal != 5 || merged.FindingCount != 3 {
		t.Errorf("merge totals wrong: %+v", merged)
	}
	if merged.OurTotal.Cmp(big.NewRat(1500, 1)) != 0 {
		t.Errorf("merged total: got %s want 1500", merged.OurTotal.String())
	}
	if merged.ByTC[166].Cmp(big.NewRat(1500, 1)) != 0 {
		t.Errorf("ByTC[166] merge: got %s want 1500", merged.ByTC[166].String())
	}
	if merged.ByTC[196].Cmp(big.NewRat(100, 1)) != 0 {
		t.Errorf("ByTC[196] merge: got %s want 100", merged.ByTC[196].String())
	}
	if merged.Skipped["reason A"] != 2 || merged.Skipped["reason B"] != 1 {
		t.Errorf("skipped merge: %+v", merged.Skipped)
	}
	if other.OurTotal.Cmp(big.NewRat(2000, 1)) != 0 {
		t.Errorf("non-merged entity altered: %+v", other)
	}
}

func TestApplyHMACToEntities(t *testing.T) {
	entities := []entityResult{
		{
			Name: "Acme Inc", EinLast4: "1234", IrsName: "ACME INC LLC",
			OurTotal: big.NewRat(1000, 1),
			Findings: []Finding{{SourceFile: "acme-inc-941.pdf"}},
		},
		{
			Name: "Mystery", EinLast4: "", IrsName: "",
			OurTotal: new(big.Rat),
			Findings: []Finding{},
		},
	}
	applyHMACToEntities(entities, "shared-secret-2026")

	// Identifying fields must be cleared / replaced.
	for _, e := range entities {
		if e.IrsName != "" {
			t.Errorf("IrsName not cleared: %q", e.IrsName)
		}
		if e.EinLast4 != "" {
			t.Errorf("EinLast4 not cleared: %q", e.EinLast4)
		}
		if len(e.Name) != 16 {
			t.Errorf("entity hash wrong length: %q (len %d)", e.Name, len(e.Name))
		}
	}
	// Determinism: same key + same EIN → same hash.
	again := []entityResult{{Name: "Acme Inc", EinLast4: "1234"}}
	applyHMACToEntities(again, "shared-secret-2026")
	if again[0].Name != entities[0].Name {
		t.Errorf("HMAC not deterministic: %q vs %q", again[0].Name, entities[0].Name)
	}
	// Different keys produce different hashes (catch a copy-paste bug
	// that ignores the key arg).
	other := []entityResult{{Name: "Acme Inc", EinLast4: "1234"}}
	applyHMACToEntities(other, "different-secret")
	if other[0].Name == entities[0].Name {
		t.Errorf("HMAC ignored the key: both produced %q", other[0].Name)
	}
	// Source filename also hashed.
	if entities[0].Findings[0].SourceFile == "acme-inc-941.pdf" {
		t.Errorf("source filename not hashed: %q", entities[0].Findings[0].SourceFile)
	}
	if len(entities[0].Findings[0].SourceFile) != 16 {
		t.Errorf("source-file hash wrong length: %q", entities[0].Findings[0].SourceFile)
	}
	// Empty-EIN entity falls back to hashing the original Name.
	if entities[1].Name == "" {
		t.Errorf("empty-EIN entity got empty hash")
	}
}

func TestBandFor(t *testing.T) {
	cases := []struct {
		dollars string
		want    string
	}{
		{"0", "$0"},
		{"1", "$1-$4,999"},
		{"4999.99", "$1-$4,999"},
		{"5000", "$5,000-$24,999"},
		{"24999.99", "$5,000-$24,999"},
		{"25000", "$25,000-$99,999"},
		{"99999.99", "$25,000-$99,999"},
		{"100000", "$100,000+"},
		{"1500000", "$100,000+"},
	}
	for _, c := range cases {
		r, _ := new(big.Rat).SetString(c.dollars)
		got := bandFor(r)
		if got != c.want {
			t.Errorf("bandFor(%s) = %q, want %q", c.dollars, got, c.want)
		}
	}
	if bandFor(nil) != "$0" {
		t.Errorf("nil should band to $0")
	}
}

func TestWriteBandedCSV(t *testing.T) {
	entities := []entityResult{
		{Name: "small", OurTotal: big.NewRat(150, 1), TranscriptsTotal: 1, FindingCount: 1, TranscriptsWithFindings: 1},
		{Name: "big", OurTotal: big.NewRat(250000, 1), TranscriptsTotal: 5, FindingCount: 8, TranscriptsWithFindings: 4},
		{Name: "zero", OurTotal: new(big.Rat), TranscriptsTotal: 1},
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	if err := writeBandedCSV(cw, entities); err != nil {
		t.Fatal(err)
	}
	cw.Flush()
	out := buf.String()

	if !strings.Contains(out, "entity,transcripts,with_findings,finding_count,claimable_band") {
		t.Errorf("missing header: %q", out)
	}
	// "big" should appear before "small" (sorted desc by amount).
	bigIdx := strings.Index(out, "big,")
	smallIdx := strings.Index(out, "small,")
	if bigIdx < 0 || smallIdx < 0 || bigIdx > smallIdx {
		t.Errorf("ordering wrong:\n%s", out)
	}
	if !strings.Contains(out, "$100,000+") {
		t.Errorf("missing high band: %q", out)
	}
	if !strings.Contains(out, "$1-$4,999") {
		t.Errorf("missing low band: %q", out)
	}
	if !strings.Contains(out, "$0") {
		t.Errorf("missing zero band: %q", out)
	}
	// No per-finding columns leaked through.
	if strings.Contains(out, "finding_form") || strings.Contains(out, "by_tc") {
		t.Errorf("banded CSV leaked detail columns: %q", out)
	}
}
