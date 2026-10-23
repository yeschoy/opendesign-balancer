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
