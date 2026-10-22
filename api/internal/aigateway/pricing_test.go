package aigateway

import (
	"encoding/json"
	"math"
	"testing"
)

func TestAIPricingIntegerReservationAndRounding(t *testing.T) {
	var missing Price
	if json.Unmarshal([]byte(`{"version":"missing","currency":"USD","max_input_tokens":10}`), &missing) == nil {
		t.Fatal("omitted rates silently became free")
	}
	p := Price{Version: "tariff-1", Currency: "USD", MaxInputTokens: 10, InputRate: 1000000, OutputRate: 2000000, CacheReadRate: 100000, CacheWriteRate: 1500000}
	if n, e := p.Reserve(8); e != nil || n != 31 {
		t.Fatalf("worst category reservation %d %v", n, e)
	}
	if n, e := p.Cost(Usage{PromptTokens: 6, CompletionTokens: 2, TotalTokens: 8, CacheReadTokens: 2, CacheWriteTokens: 1}); e != nil || n != 9 {
		t.Fatalf("exact integer tariff %d %v", n, e)
	}
	p.InputRate = 1
	p.OutputRate = 1
	p.CacheWriteRate = 1
	p.CacheReadRate = 1
	if n, e := p.Cost(Usage{PromptTokens: 1, TotalTokens: 1}); e != nil || n != 1 {
		t.Fatal("fractional micro-USD rounded down")
	}
	for _, u := range []Usage{{PromptTokens: math.MaxInt64, CompletionTokens: math.MaxInt64, TotalTokens: -2}, {PromptTokens: 1, CompletionTokens: 1, TotalTokens: 3}, {PromptTokens: 1, TotalTokens: 1, CacheReadTokens: 2}} {
		if _, e := p.Cost(u); e == nil {
			t.Fatal("invalid or overflowing usage admitted")
		}
	}
}
