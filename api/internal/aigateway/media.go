package aigateway

import (
	"bytes"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"strings"
)

// These are input limits, independent of output limits or vendor account quotas.
const MaxInputAssetBytes = 1 << 20
const MaxInputAssets = 8

var errInputMedia = errors.New("unsupported, malformed or oversized inline input")

func InputMediaKind(contentType string) string {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "image"
	case "application/pdf":
		return "pdf"
	case "audio/wav", "audio/mpeg":
		return "audio"
	case "video/mp4":
		return "video"
	}
	return ""
}

// Check the declared media type and its binary signature. The gateway does not
// execute, render or claim to validate every page/frame of a customer document.
func ValidateInputMedia(contentType string, data []byte) error {
	canonical, params, err := mime.ParseMediaType(contentType)
	if err != nil || canonical != contentType || len(params) != 0 || InputMediaKind(canonical) == "" || len(data) == 0 || len(data) > MaxInputAssetBytes {
		return errInputMedia
	}
	detected := http.DetectContentType(data)
	valid := detected == canonical
	if canonical == "application/pdf" {
		valid = bytes.HasPrefix(data, []byte("%PDF-"))
	}
	if canonical == "audio/wav" {
		valid = len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE"
	}
	if !valid {
		return errInputMedia
	}
	return nil
}

func ValidateInputModalities(protocols, modalities []string) error {
	seen := map[string]bool{}
	has := func(protocol string) bool {
		if len(protocols) == 0 {
			return protocol == "chat_completions"
		}
		for _, p := range protocols {
			if p == protocol {
				return true
			}
		}
		return false
	}
	for _, kind := range modalities {
		if seen[kind] {
			return errInputMedia
		}
		seen[kind] = true
		switch kind {
		case "text":
		case "image", "pdf":
			if !has("chat_completions") && !has(Responses) && !has(Messages) && !has(Gemini) {
				return errInputMedia
			}
		case "audio":
			if !has("chat_completions") && !has(Gemini) {
				return errInputMedia
			}
		case "video":
			if !has(Gemini) {
				return errInputMedia
			}
		default:
			return errInputMedia
		}
	}
	if len(modalities) > 0 && !seen["text"] {
		return errInputMedia
	}
	return nil
}

func (r Route) InputTypes() []string {
	if len(r.InputModalities) > 0 {
		return append([]string(nil), r.InputModalities...)
	}
	if r.Supports("chat_completions") || r.Supports(Responses) || r.Supports(Messages) || r.Supports(Gemini) {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

func (r Route) permitsInput(kind string) bool {
	for _, value := range r.InputTypes() {
		if value == kind {
			return true
		}
	}
	return false
}

// Validate native inline representations after local file references have been
// expanded. Supplier file IDs and remote source URLs are never fetched here.
func validateNativeMedia(body any, protocol string, route Route) error {
	count, total := 0, 0
	add := func(contentType, encoded string) error {
		if strings.ContainsAny(encoded, "\r\n\t ") || len(encoded) > base64.StdEncoding.EncodedLen(MaxInputAssetBytes) {
			return errInputMedia
		}
		data, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || ValidateInputMedia(contentType, data) != nil || !route.permitsInput(InputMediaKind(contentType)) {
			return errInputMedia
		}
		count++
		total += len(data)
		if count > MaxInputAssets || total > MaxInputAssetBytes {
			return errInputMedia
		}
		kind := InputMediaKind(contentType)
		if kind == "audio" && protocol != "chat_completions" && protocol != Gemini || kind == "video" && protocol != Gemini {
			return errInputMedia
		}
		return nil
	}
	dataURI := func(value any, expected string) error {
		raw, ok := value.(string)
		if !ok {
			return errInputMedia
		}
		header, data, ok := strings.Cut(raw, ",")
		if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
			return errInputMedia
		}
		contentType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		if InputMediaKind(contentType) != expected {
			return errInputMedia
		}
		return add(contentType, data)
	}
	var walk func(any, int) error
	walk = func(v any, depth int) error {
		if depth > 24 {
			return errInputMedia
		}
		switch x := v.(type) {
		case []any:
			for _, child := range x {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			if x["type"] == "image" || x["type"] == "document" {
				source, ok := x["source"].(map[string]any)
				if !ok || source["type"] != "base64" || !nativeFields(source, "type media_type data") {
					return errInputMedia
				}
				contentType, ok := source["media_type"].(string)
				encoded, yes := source["data"].(string)
				expected := "image"
				if x["type"] == "document" {
					expected = "pdf"
				}
				if !ok || !yes || InputMediaKind(contentType) != expected {
					return errInputMedia
				}
				if err := add(contentType, encoded); err != nil {
					return err
				}
			}
			for key, value := range x {
				switch key {
				case "image_url":
					if image, ok := value.(map[string]any); ok {
						if !nativeFields(image, "url detail") {
							return errInputMedia
						}
						value = image["url"]
					}
					if err := dataURI(value, "image"); err != nil {
						return err
					}
				case "file_data":
					if err := dataURI(value, "pdf"); err != nil {
						return err
					}
				case "inlineData", "inline_data":
					inline, ok := value.(map[string]any)
					if !ok || !nativeFields(inline, "mimeType mime_type data") {
						return errInputMedia
					}
					contentType, ok := inline["mimeType"].(string)
					if !ok {
						contentType, ok = inline["mime_type"].(string)
					} else if inline["mime_type"] != nil {
						return errInputMedia
					}
					encoded, yes := inline["data"].(string)
					if protocol != Gemini || !ok || !yes {
						return errInputMedia
					}
					if err := add(contentType, encoded); err != nil {
						return err
					}
				case "input_audio":
					audio, ok := value.(map[string]any)
					if !ok || protocol != "chat_completions" || !nativeFields(audio, "data format") {
						return errInputMedia
					}
					encoded, ok := audio["data"].(string)
					format, yes := audio["format"].(string)
					contentType := "audio/" + format
					if format == "mp3" {
						contentType = "audio/mpeg"
					}
					if !ok || !yes || (format != "mp3" && format != "wav") {
						return errInputMedia
					}
					if err := add(contentType, encoded); err != nil {
						return err
					}
				}
				if err := walk(value, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(body, 0)
}

// Bound text independently of decoded binary. Tool schemas/arguments remain
// bounded by the full JSON body size and existing nesting/array limits.
func nativeTextBytes(v any) int {
	total := 0
	switch x := v.(type) {
	case string:
		total = len(x)
	case []any:
		for _, value := range x {
			total += nativeTextBytes(value)
		}
	case map[string]any:
		for key, value := range x {
			if key == "data" || key == "file_data" || key == "image_url" || key == "inlineData" || key == "inline_data" || key == "input_audio" {
				continue
			}
			total += nativeTextBytes(value)
		}
	}
	return total
}
