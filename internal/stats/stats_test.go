package stats

import "testing"

func TestPerModelAndHitRateFields(t *testing.T) {
	s := New()
	in, cached := int64(100), int64(80)
	s.Record(Sample{AccountID: "a", Model: "m1", Result: "success", Input: &in, Cached: &cached, Prompt: 100, Currency: "CNY", Cost: 0.5})
	s.Record(Sample{AccountID: "a", Model: "m1", Result: "failed"})
	s.Record(Sample{AccountID: "a", Model: "m2", Result: "success", Currency: "USD", Cost: 1})
	sum := s.Summary()
	m1 := sum.Models["m1"]
	if m1.Requests != 2 || m1.Prompt != 100 || m1.Cached != 80 || m1.CostCNY != 0.5 || sum.Models["m2"].CostUSD != 1 {
		t.Fatalf("models = %+v", sum.Models)
	}
	if sum.Total.Requests != 3 || sum.Total.CostCNY != 0.5 {
		t.Fatalf("total = %+v", sum.Total)
	}
	s2 := New()
	s2.Import(s.Export())
	if s2.Summary().Models["m1"].Requests != 2 {
		t.Fatal("per-model stats lost across export/import")
	}
	for i := 0; i < 3*maxModels; i++ {
		s.Record(Sample{Model: string(rune('A'+i%26)) + string(rune('a'+i/26)), Result: "success"})
	}
	if n := len(s.Summary().Models); n > maxModels {
		t.Fatalf("%d model rings", n)
	}
}

func TestImportBackfillsPromptForOldBuckets(t *testing.T) {
	s := New()
	m := s.now().Unix() / 60
	s.Import(Export{Global: []Bucket{{Minute: m, Requests: 1, Input: 1000, Cached: 900}, {Minute: m - 1, Requests: 1, Input: 500, Cached: 100, Prompt: 500}}})
	tot := s.Summary().Total
	if tot.Prompt != 1500 || tot.Cached != 1000 {
		t.Fatalf("total = %+v", tot)
	}
}

func TestSummaryPerProvider(t *testing.T) {
	s := New()
	s.Record(Sample{AccountID: "o", Provider: "opendesign", Model: "m", Result: "success"})
	s.Record(Sample{AccountID: "r", Provider: "river", Model: "x", Result: "success"})
	s.Record(Sample{AccountID: "r", Provider: "river", Model: "x", Result: "failed"})
	if got := s.Summary().Total.Requests; got != 3 {
		t.Fatalf("all = %d", got)
	}
	r := s.SummaryFor("river")
	if r.Total.Requests != 2 || len(r.Accounts) != 1 || r.Models["x"].Requests != 2 || r.Models["m"].Requests != 0 {
		t.Fatalf("river = %+v", r)
	}
	if s.SummaryFor("opendesign").Total.Requests != 1 || s.SummaryFor("none").Total.Requests != 0 {
		t.Fatal("provider split wrong")
	}
	s2 := New()
	s2.Import(s.Export())
	if s2.SummaryFor("river").Total.Requests != 2 {
		t.Fatal("provider stats lost across export/import")
	}
	// metadata written before providers existed: all of it was OpenDesign
	old := s.Export()
	old.Providers = nil
	s3 := New()
	s3.Import(old)
	if s3.SummaryFor("opendesign").Total.Requests != 3 || s3.SummaryFor("river").Total.Requests != 0 {
		t.Fatal("legacy stats not attributed to opendesign")
	}
}
