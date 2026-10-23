package aigateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const inputTestPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aCb0AAAAASUVORK5CYII="

func TestAIInputMediaNativeRepresentations(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\nExplicit synthetic protocol document\n%%EOF\n"))
	cases := []struct {
		protocol string
		body     map[string]any
	}{
		{"chat_completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + inputTestPNG}}, map[string]any{"type": "file", "file": map[string]any{"filename": "probe.pdf", "file_data": "data:application/pdf;base64," + pdf}}}}}}},
		{Responses, map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_file", "filename": "probe.pdf", "file_data": "data:application/pdf;base64," + pdf}}}}}},
		{Messages, map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": pdf}}}}}}},
		{Gemini, map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "application/pdf", "data": pdf}}}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			cfg := nativeConfig("http://127.0.0.1:9000")
			cfg.Routes[0].InputModalities = []string{"text", "image", "pdf"}
			tc.body["model"] = "alias"
			switch tc.protocol {
			case Messages, "chat_completions":
				tc.body["max_tokens"] = 8
			case Responses:
				tc.body["max_output_tokens"] = 8
			case Gemini:
				tc.body["generationConfig"] = map[string]any{"maxOutputTokens": 8}
			}
			raw, _ := json.Marshal(tc.body)
			if _, _, err := DecodeNative(raw, cfg, tc.protocol, "", false); err != nil {
				t.Fatal("valid native media rejected", err)
			}
			cfg.Routes[0].InputModalities = []string{"text"}
			if _, _, err := DecodeNative(raw, cfg, tc.protocol, "", false); err == nil {
				t.Fatal("model input capability bypassed")
			}
		})
	}
}

func TestAIInputMediaRejectsMalformedAndRemoteInputs(t *testing.T) {
	cfg := nativeConfig("http://127.0.0.1:9000")
	cfg.Routes[0].InputModalities = []string{"text", "image", "pdf"}
	for _, raw := range []string{
		`{"model":"alias","input":[{"type":"input_image","image_url":"https://example.com/private.png"}]}`,
		`{"model":"alias","input":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}`,
		`{"model":"alias","input":[{"type":"input_file","file_id":"file-supplier-shared-account"}]}`,
		`{"model":"alias","input":[{"type":"input_file","filename":"x.pdf","file_data":"data:application/pdf;base64,JVBERi0=","file_url":"http://127.0.0.1/private.pdf"}]}`,
		`{"model":"alias","input":[{"type":"input_image","image_url":"data:image/png;base64,iVBO\nRw=="}]}`,
	} {
		var body map[string]any
		if json.Unmarshal([]byte(raw), &body) != nil {
			t.Fatal("fixture JSON")
		}
		body["max_output_tokens"] = 8
		encoded, _ := json.Marshal(body)
		if _, _, err := DecodeNative(encoded, cfg, Responses, "", false); err == nil {
			t.Fatal("unsafe inline input accepted")
		}
	}
	raw := `{"model":"alias","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"http://127.0.0.1/private"}}]}],"max_tokens":8}`
	if _, _, err := DecodeNative([]byte(raw), cfg, Messages, "", false); err == nil {
		t.Fatal("native remote source accepted")
	}
	if err := ValidateInputMedia("image/png", []byte("%PDF-1.7\n")); err == nil {
		t.Fatal("declared MIME did not match bytes")
	}
	for _, m := range [][]string{{"pdf"}, {"text", "text"}, {"text", "video"}} {
		if ValidateInputModalities([]string{Messages}, m) == nil {
			t.Fatal("invalid capability allowed", m)
		}
	}
	if ValidateInputModalities([]string{Gemini}, []string{"text", "image", "pdf", "audio", "video"}) != nil {
		t.Fatal("explicit native capabilities rejected")
	}
}

func TestAIInputMediaDecodedLimitsAndLegacyDefaults(t *testing.T) {
	cfg := nativeConfig("http://127.0.0.1:9000")
	if !cfg.Routes[0].permitsInput("image") || cfg.Routes[0].permitsInput("pdf") {
		t.Fatal("legacy input defaults changed")
	}
	cfg.Routes[0].InputModalities = []string{"text", "pdf", "image"}
	data := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{'x'}, MaxInputAssetBytes-len("%PDF-1.7\n"))...)
	input := func(data []byte, n int) []byte {
		items := []any{}
		for i := 0; i < n; i++ {
			items = append(items, map[string]any{"type": "input_file", "filename": "limit.pdf", "file_data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(data)})
		}
		raw, _ := json.Marshal(map[string]any{"model": "alias", "max_output_tokens": 8, "input": []any{map[string]any{"role": "user", "content": items}}})
		return raw
	}
	if _, _, err := DecodeNative(input(data, 1), cfg, Responses, "", false); err != nil {
		t.Fatal("one MiB inline input was counted as prose", err)
	}
	if _, _, err := DecodeNative(input(append(data, 'x'), 1), cfg, Responses, "", false); err == nil {
		t.Fatal("oversized decoded input accepted")
	}
	if _, _, err := DecodeNative(input(data[:600000], 2), cfg, Responses, "", false); err == nil {
		t.Fatal("combined decoded input cap bypassed")
	}
	if _, _, err := DecodeNative(input(data[:32], 9), cfg, Responses, "", false); err == nil {
		t.Fatal("attachment count cap bypassed")
	}
	raw, _ := json.Marshal(map[string]any{"model": "alias", "max_output_tokens": 8, "input": strings.Repeat("t", 16001)})
	if _, _, err := DecodeNative(raw, cfg, Responses, "", false); err == nil {
		t.Fatal("text cap bypassed after binary body expansion")
	}
}
