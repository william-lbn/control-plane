package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/william-lbn/control-plane/api/internal/aigateway"
)

// Expand only this gateway's exact-branch handles. Vendor account IDs and remote
// fetch URLs remain subject to the native protocol's stateless-input rejection.
func (s *server) resolveAIInputs(ctx context.Context, project, branch string, raw []byte, protocol string) ([]byte, []string, error) {
	invalid := func() error {
		return providerProblem(422, "invalid_input_file_reference", "Use a live gateway file from this exact branch and a supported native content item")
	}
	if len(raw) > aigateway.MaxBodyBytes {
		return nil, nil, invalid()
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, nil, invalid()
	}
	ids := map[string]bool{}
	count, total := 0, 0
	load := func(id, kind string) (aiInputFile, string, error) {
		if !aiInputID.MatchString(id) {
			return aiInputFile{}, "", invalid()
		}
		file, err := s.loadAIInput(ctx, id, project, branch)
		if isNoRows(err) {
			return aiInputFile{}, "", invalid()
		}
		if err != nil {
			return aiInputFile{}, "", err
		}
		if kind != "" && aigateway.InputMediaKind(file.ContentType) != kind {
			return aiInputFile{}, "", invalid()
		}
		data, err := s.aiInputOpen(file)
		if err != nil {
			return aiInputFile{}, "", err
		}
		defer clear(data)
		count++
		total += len(data)
		if count > aigateway.MaxInputAssets || total > aigateway.MaxInputAssetBytes {
			return aiInputFile{}, "", invalid()
		}
		if aigateway.ValidateInputMedia(file.ContentType, data) != nil {
			return aiInputFile{}, "", invalid()
		}
		ids[id] = true
		return file, base64.StdEncoding.EncodeToString(data), nil
	}
	fields := func(m map[string]any, names string) bool {
		for key := range m {
			if !strings.Contains(" "+names+" ", " "+key+" ") {
				return false
			}
		}
		return true
	}
	if protocol == "" {
		protocol = "chat_completions"
	}
	var walk func(any, int) error
	walk = func(value any, depth int) error {
		if depth > 24 {
			return invalid()
		}
		switch v := value.(type) {
		case []any:
			if len(v) > 256 {
				return invalid()
			}
			for _, child := range v {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			if id, ok := v["file_id"].(string); ok && protocol == aigateway.Responses {
				if v["type"] == "input_file" {
					if !fields(v, "type file_id detail") {
						return invalid()
					}
					f, data, err := load(id, "pdf")
					if err != nil {
						return err
					}
					delete(v, "file_id")
					v["file_data"] = "data:" + f.ContentType + ";base64," + data
					v["filename"] = f.Filename
				} else if v["type"] == "input_image" {
					if !fields(v, "type file_id detail") {
						return invalid()
					}
					f, data, err := load(id, "image")
					if err != nil {
						return err
					}
					delete(v, "file_id")
					v["image_url"] = "data:" + f.ContentType + ";base64," + data
				}
			}
			if source, ok := v["source"].(map[string]any); ok && source["type"] == "file" && (v["type"] == "image" || v["type"] == "document") {
				if protocol != aigateway.Messages || !fields(source, "type file_id") {
					return invalid()
				}
				id, ok := source["file_id"].(string)
				if !ok {
					return invalid()
				}
				kind := "image"
				if v["type"] == "document" {
					kind = "pdf"
				}
				f, data, err := load(id, kind)
				if err != nil {
					return err
				}
				v["source"] = map[string]any{"type": "base64", "media_type": f.ContentType, "data": data}
			}
			if file, ok := v["file"].(map[string]any); ok && file["file_id"] != nil {
				if protocol != "chat_completions" || v["type"] != "file" || !fields(file, "file_id") {
					return invalid()
				}
				id, ok := file["file_id"].(string)
				if !ok {
					return invalid()
				}
				f, data, err := load(id, "pdf")
				if err != nil {
					return err
				}
				v["file"] = map[string]any{"filename": f.Filename, "file_data": "data:" + f.ContentType + ";base64," + data}
			}
			if audio, ok := v["input_audio"].(map[string]any); ok && audio["file_id"] != nil {
				if protocol != "chat_completions" || v["type"] != "input_audio" || !fields(audio, "file_id") {
					return invalid()
				}
				id, ok := audio["file_id"].(string)
				if !ok {
					return invalid()
				}
				f, data, err := load(id, "audio")
				if err != nil {
					return err
				}
				format := "wav"
				if f.ContentType == "audio/mpeg" {
					format = "mp3"
				}
				v["input_audio"] = map[string]any{"data": data, "format": format}
			}
			if input, ok := v["fileData"].(map[string]any); ok {
				if protocol != aigateway.Gemini || !fields(input, "fileUri mimeType") {
					return invalid()
				}
				uri, ok := input["fileUri"].(string)
				if !ok || !strings.HasPrefix(uri, "neon-file:") {
					return invalid()
				}
				f, data, err := load(strings.TrimPrefix(uri, "neon-file:"), "")
				if err != nil {
					return err
				}
				if hint, exists := input["mimeType"]; exists && hint != f.ContentType {
					return invalid()
				}
				delete(v, "fileData")
				v["inlineData"] = map[string]any{"mimeType": f.ContentType, "data": data}
			}
			for key, child := range v {
				if key == "image_url" {
					uri, ok := child.(string)
					image, mapOK := child.(map[string]any)
					if mapOK {
						uri, ok = image["url"].(string)
					}
					if ok && strings.HasPrefix(uri, "neon-file:") {
						if protocol != "chat_completions" && protocol != aigateway.Responses {
							return invalid()
						}
						f, data, err := load(strings.TrimPrefix(uri, "neon-file:"), "image")
						if err != nil {
							return err
						}
						uri = "data:" + f.ContentType + ";base64," + data
						if mapOK {
							image["url"] = uri
						} else {
							v[key] = uri
						}
						child = v[key]
					}
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(body, 0); err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return raw, nil, nil
	}
	expanded, err := json.Marshal(body)
	if err != nil || len(expanded) > aigateway.MaxBodyBytes {
		return nil, nil, invalid()
	}
	references := make([]string, 0, len(ids))
	for id := range ids {
		references = append(references, id)
	}
	sort.Strings(references)
	return expanded, references, nil
}
