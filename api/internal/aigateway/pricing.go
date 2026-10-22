package aigateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Rates are integer micro-USD per million tokens. A request pins this entire
// versioned tariff before transmission, including conservative input capacity.
type Price struct {
	Version        string `json:"version"`
	Currency       string `json:"currency"`
	MaxInputTokens int64  `json:"max_input_tokens"`
	InputRate      int64  `json:"input_microusd_per_million_tokens"`
	OutputRate     int64  `json:"output_microusd_per_million_tokens"`
	CacheReadRate  int64  `json:"cache_read_microusd_per_million_tokens"`
	CacheWriteRate int64  `json:"cache_write_microusd_per_million_tokens"`
}

func (p *Price) UnmarshalJSON(raw []byte) error {
	type plain Price
	var value plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid explicit AI tariff")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return errors.New("invalid explicit AI tariff")
	}
	for _, name := range []string{"version", "currency", "max_input_tokens", "input_microusd_per_million_tokens", "output_microusd_per_million_tokens", "cache_read_microusd_per_million_tokens", "cache_write_microusd_per_million_tokens"} {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			return errors.New("all AI tariff rates must be explicit")
		}
	}
	*p = Price(value)
	return p.Validate()
}

func (p Price) Validate() error {
	if !modelID.MatchString(p.Version) || p.Currency != "USD" || p.MaxInputTokens < 1 || p.MaxInputTokens > 1000000 {
		return errors.New("invalid AI tariff")
	}
	for _, rate := range []int64{p.InputRate, p.OutputRate, p.CacheReadRate, p.CacheWriteRate} {
		if rate < 0 || rate > 1000000000 {
			return errors.New("invalid AI tariff")
		}
	}
	return nil
}
func (p Price) Cost(u Usage) (int64, error) {
	if p.Validate() != nil || u.PromptTokens < 0 || u.PromptTokens > 10000000 || u.CompletionTokens < 0 || u.CompletionTokens > 10000000 || u.TotalTokens < 0 || u.TotalTokens != u.PromptTokens+u.CompletionTokens || u.TotalTokens > 10000000 || u.CacheReadTokens < 0 || u.CacheReadTokens > 10000000 || u.CacheWriteTokens < 0 || u.CacheWriteTokens > 10000000 || u.CacheReadTokens+u.CacheWriteTokens > u.PromptTokens {
		return 0, errors.New("invalid priced usage")
	}
	n := (u.PromptTokens-u.CacheReadTokens-u.CacheWriteTokens)*p.InputRate + u.CompletionTokens*p.OutputRate + u.CacheReadTokens*p.CacheReadRate + u.CacheWriteTokens*p.CacheWriteRate
	return (n + 999999) / 1000000, nil
}
func (p Price) Reserve(maxOutput int) (int64, error) {
	rate := max(p.InputRate, p.CacheReadRate, p.CacheWriteRate)
	if p.Validate() != nil || maxOutput < 0 || maxOutput > 4096 {
		return 0, errors.New("invalid AI reservation")
	}
	return (p.MaxInputTokens*rate + int64(maxOutput)*p.OutputRate + 999999) / 1000000, nil
}
