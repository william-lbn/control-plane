package functions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"unicode/utf8"
)

// Budget includes escaped JSON (one Unicode escape may use six wire bytes),
// three multipart headers and the bounded ZIP. Never spill user code or env
// credentials into the API host's temporary directory.
const MaxDeploymentRequestBytes = MaxArchiveBytes + (256 << 10)
const maxEnvironmentJSONBytes = 6*MaxEnvironmentBytes + (8 << 10)

// ParseDeploymentRequest follows the branch Functions multipart wire contract:
// zip, runtime and a single JSON environment part. Authentication, project /
// branch authorization, If-Match and quota admission belong to the API caller.
// Parsing validates bytes but never imports, bundles or executes customer code.
func ParseDeploymentRequest(r *http.Request) (DeploymentInput, error) {
	var input DeploymentInput
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || len(parameters) != 1 || parameters["boundary"] == "" || len(parameters["boundary"]) > 70 {
		return input, errors.New("Functions deployment requires bounded multipart/form-data")
	}
	if r.Body == nil || r.ContentLength > MaxDeploymentRequestBytes {
		return input, errors.New("Functions deployment request exceeds size limit")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxDeploymentRequestBytes+1))
	if err != nil || len(body) > MaxDeploymentRequestBytes {
		return input, errors.New("Functions deployment request cannot be read within bounds")
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	seen := make(map[string]bool)
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return DeploymentInput{}, errors.New("invalid Functions multipart framing")
		}
		name := part.FormName()
		if seen[name] || (name != "zip" && name != "runtime" && name != "environment") || part.Header.Get("Content-Transfer-Encoding") != "" || len(part.FileName()) > 255 || (name != "zip" && part.FileName() != "") {
			_ = part.Close()
			return DeploymentInput{}, errors.New("unknown, duplicate or encoded Functions multipart field")
		}
		seen[name] = true
		limit := 64
		if name == "zip" {
			limit = MaxArchiveBytes
		} else if name == "environment" {
			limit = maxEnvironmentJSONBytes
		}
		content, err := io.ReadAll(io.LimitReader(part, int64(limit)+1))
		_ = part.Close()
		if err != nil || len(content) > limit {
			return DeploymentInput{}, errors.New("Functions multipart field exceeds bounds")
		}
		switch name {
		case "zip":
			if _, err := ValidateBundle(content); err != nil {
				return DeploymentInput{}, err
			}
			input.Archive = content
		case "runtime":
			if string(content) != Runtime {
				return DeploymentInput{}, errors.New("only nodejs24 Functions runtime is supported")
			}
			input.Runtime = Runtime
		case "environment":
			input.Environment, err = parseEnvironmentPatch(content)
			if err != nil {
				return DeploymentInput{}, err
			}
		}
	}
	if len(seen) == 0 {
		return DeploymentInput{}, errors.New("Functions deployment requires code or configuration")
	}
	return input, nil
}

// Decode tokens instead of unmarshalling a map: duplicate keys and non-string
// values must not silently change the signed idempotency request semantics.
func parseEnvironmentPatch(content []byte) (map[string]string, error) {
	invalid := errors.New("environment must be one bounded JSON object of unique string keys and values")
	if !utf8.Valid(content) {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, invalid
	}
	environment := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || len(environment) >= 32 {
			return nil, invalid
		}
		if _, exists := environment[name]; exists {
			return nil, invalid
		}
		var value string
		// json.Unmarshal into string accepts null as zero value. Token type
		// checking keeps null distinct from the intentional empty-string delete.
		token, err = decoder.Token()
		value, ok = token.(string)
		if err != nil || !ok {
			return nil, invalid
		}
		environment[name] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, invalid
	}
	if _, err = decoder.Token(); err != io.EOF || ValidateEnvironment(environment) != nil {
		return nil, invalid
	}
	return environment, nil
}
