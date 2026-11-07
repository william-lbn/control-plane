package control

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type genericConfigureTransport func(*http.Request) (*http.Response, error)

func (f genericConfigureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDSGAGenericConfigureOnlyOriginalEmptyInstanceAndNoUnknownReplay(t *testing.T) {
	for _, state := range []string{"empty", "init", "configuration_pending", "configuration", "running", "failed", "terminated", "unknown"} {
		t.Run(state, func(t *testing.T) {
			posts, checks := 0, 0
			client := &http.Client{Transport: genericConfigureTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "127.0.0.1:3080" || r.Header.Get("Authorization") != "Bearer original-token" {
					t.Fatal("target or original credential changed")
				}
				body := "{\"status\":\"" + state + "\"}"
				if r.Method == http.MethodPost {
					posts++
					raw, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/configure" || string(raw) != "{\"spec\":{}}" || r.Header.Get("Content-Type") != "application/json" {
						t.Fatal("native immutable bytes changed")
					}
					return nil, io.ErrUnexpectedEOF // 接受后的丢回复不能在同一次调用自动重发。
				}
				if r.URL.Path != "/status" {
					t.Fatal("unexpected native route")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			err := configureOriginalNative(context.Background(), client, "http://127.0.0.1:3080", "original-token", []byte("{\"spec\":{}}"), func() error { checks++; return nil })
			valid := state == "empty" || state == "init" || state == "configuration_pending" || state == "configuration" || state == "running"
			if (err == nil) != valid || posts > 1 || (posts == 1) != (state == "empty") || checks != posts {
				t.Fatal("unknown/terminal state or replay admitted", err, posts, checks)
			}
		})
	}
}
func TestDSGAGenericConfigureRechecksLeaseBeforeAnyPost(t *testing.T) {
	posts := 0
	expired := errors.New("original lease expired")
	client := &http.Client{Transport: genericConfigureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"empty"}`))}, nil
	})}
	err := configureOriginalNative(context.Background(), client, "http://127.0.0.1:3080", "original-token", []byte("{}"), func() error { return expired })
	if !errors.Is(err, expired) || posts != 0 {
		t.Fatal("expired creator submitted native config")
	}
}
