package functions

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type deploymentPart struct {
	name, filename string
	content        []byte
}

func deploymentRequest(t *testing.T, parts ...deploymentPart) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, item := range parts {
		var part io.Writer
		var err error
		if item.filename != "" {
			part, err = writer.CreateFormFile(item.name, item.filename)
		} else {
			part, err = writer.CreateFormField(item.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(item.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/deployments", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func TestMultipartCodeAndConfigOnlyAdmission(t *testing.T) {
	zip := archiveFor(t, "index.mjs")
	r := deploymentRequest(t, deploymentPart{name: "zip", filename: "function.zip", content: zip}, deploymentPart{name: "runtime", content: []byte(Runtime)}, deploymentPart{name: "environment", content: []byte(`{"TOKEN":"private-fixture","DELETE":""}`)})
	input, err := ParseDeploymentRequest(r)
	if err != nil || !bytes.Equal(input.Archive, zip) || input.Runtime != Runtime || input.Environment["TOKEN"] != "private-fixture" || len(input.Environment) != 2 {
		t.Fatal("valid code upload failed", err)
	}
	config, err := ParseDeploymentRequest(deploymentRequest(t, deploymentPart{name: "environment", content: []byte(`{}`)}))
	if err != nil || config.Environment == nil || len(config.Archive) != 0 {
		t.Fatal("configuration-only request failed", err)
	}
	code, err := ParseDeploymentRequest(deploymentRequest(t, deploymentPart{name: "zip", content: zip}))
	if err != nil || code.Environment != nil {
		t.Fatal("omitted environment lost preservation semantics", err)
	}
}

func TestMultipartRejectsAmbiguousPrivateInput(t *testing.T) {
	for _, parts := range [][]deploymentPart{
		{},
		{{name: "runtime", content: []byte(Runtime)}, {name: "runtime", content: []byte(Runtime)}},
		{{name: "environment[KEY]", content: []byte("private-fixture")}},
		{{name: "environment", filename: "env.json", content: []byte(`{}`)}},
		{{name: "environment", content: []byte(`{"KEY":"private-fixture","KEY":"other"}`)}},
		{{name: "environment", content: []byte(`{"KEY":null}`)}},
		{{name: "environment", content: []byte(`{"KEY":12}`)}},
		{{name: "environment", content: []byte(`{"KEY":"x"} {}`)}},
		{{name: "environment", content: []byte(`null`)}},
		{{name: "environment", content: []byte(`{"DATABASE_URL":"private-fixture"}`)}},
		{{name: "environment", content: []byte{'{', '"', 'K', '"', ':', '"', 255, '"', '}'}}},
		{{name: "zip", content: []byte("private-fixture-invalid")}},
		{{name: "runtime", content: []byte("nodejs22")}},
	} {
		if _, err := ParseDeploymentRequest(deploymentRequest(t, parts...)); err == nil || strings.Contains(err.Error(), "private-fixture") {
			t.Fatal("ambiguous input accepted or secret disclosed")
		}
	}
}

func TestMultipartWireAndActualBodyBounds(t *testing.T) {
	r := deploymentRequest(t, deploymentPart{name: "runtime", content: []byte(Runtime)})
	r.ContentLength = MaxDeploymentRequestBytes + 1
	if _, err := ParseDeploymentRequest(r); err == nil {
		t.Fatal("oversized declared body accepted")
	}
	r = deploymentRequest(t, deploymentPart{name: "runtime", content: []byte(Runtime)})
	r.ContentLength = -1
	r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", MaxDeploymentRequestBytes+1)))
	if _, err := ParseDeploymentRequest(r); err == nil {
		t.Fatal("unbounded chunked body accepted")
	}
	for _, contentType := range []string{"application/json", "multipart/form-data", "multipart/form-data; boundary=x; charset=utf-8"} {
		r = deploymentRequest(t, deploymentPart{name: "runtime", content: []byte(Runtime)})
		r.Header.Set("Content-Type", contentType)
		if _, err := ParseDeploymentRequest(r); err == nil {
			t.Fatal("invalid MIME framing accepted")
		}
	}
}
