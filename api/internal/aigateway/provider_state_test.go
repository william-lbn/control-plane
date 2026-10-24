package aigateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderStateOptInAndReferenceBoundary(t *testing.T) {
	c := nativeConfig("http://127.0.0.1:9090")
	body := `{"model":"alias","input":"next","max_output_tokens":16,"store":true,"previous_response_id":"nrp_aaaaaaaaaaaaaaaaaaaaaaaa"}`
	if _, _, _, err := DecodeNativeProviderState([]byte(body), c); err == nil {
		t.Fatal("Supplier retention was enabled without an operator grant")
	}
	c.Routes[0].AllowProviderState = true
	q, _, intent, err := DecodeNativeProviderState([]byte(body), c)
	if err != nil || !intent.Store || !q.stateRequested || intent.Previous != "nrp_aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatal("Approved public state intent rejected", err)
	}
	if _, _, err := DecodeNative([]byte(body), c, Responses, "", false); err == nil {
		t.Fatal("Original stateless decoder was widened")
	}
	for _, input := range []string{
		`"previous_response_id":"resp_other_account"`,
		`"conversation":"conv_other_branch"`,
		`"conversation":{"id":"ncv_aaaaaaaaaaaaaaaaaaaaaaaa","extra":"unexpected"}`,
		`"previous_response_id":"nrp_aaaaaaaaaaaaaaaaaaaaaaaa","conversation":"ncv_bbbbbbbbbbbbbbbbbbbbbbbb"`,
		`"input":[{"type":"item_reference","id":"provider-item"}]`,
		`"input":[{"role":"user","content":[{"type":"input_file","file_id":"provider-file"}]}]`,
		`"metadata":{"_NeOn_binding":"forged"}`,
		`"metadata":{"key":12}`,
		`"store":"true"`,
	} {
		raw := `{"model":"alias","input":"next","max_output_tokens":16,` + input + `}`
		if _, _, _, err := DecodeNativeProviderState([]byte(raw), c); err == nil {
			t.Fatal("Unsafe provider-state input accepted", input)
		}
	}
	// Null explicitly starts a fresh response. It does not require retention.
	c.Routes[0].AllowProviderState = false
	if q, _, intent, err := DecodeNativeProviderState([]byte(`{"model":"alias","input":"fresh","max_output_tokens":16,"previous_response_id":null,"store":false}`), c); err != nil || intent.Requested() || q.stateRequested {
		t.Fatal("Null continuation changed legacy stateless behavior", err)
	}
}

func TestProviderStateNeedsDurableBindingBeforeDispatch(t *testing.T) {
	calls := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer peer.Close()
	c := nativeConfig(peer.URL)
	c.Routes[0].AllowProviderState = true
	q, route, _, err := DecodeNativeProviderState([]byte(`{"model":"alias","input":"owned","max_output_tokens":16,"store":true}`), c)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	out := ForwardNative(context.Background(), w, q, route, time.Second)
	if calls != 0 || w.Code != 503 || out.Dispatched {
		t.Fatal("Unbound request reached supplier")
	}
}

func TestProviderStateJSONReceiptFailureRetainsUsageAndHidesRemoteID(t *testing.T) {
	proof := strings.Repeat("a", 64)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		if body["store"] != true || body["metadata"].(map[string]any)["_neon_binding"] != proof {
			t.Error("Supplier did not receive explicit retention proof")
		}
		io.WriteString(w, `{"id":"resp_private_id","object":"response","status":"completed","output":[],"metadata":{"_neon_binding":"`+proof+`"},"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`)
	}))
	defer peer.Close()
	c := nativeConfig(peer.URL)
	c.Routes[0].AllowProviderState = true
	for _, fail := range []bool{false, true} {
		q, route, intent, err := DecodeNativeProviderState([]byte(`{"model":"alias","input":"owned","max_output_tokens":16,"store":true}`), c)
		if err != nil {
			t.Fatal(err)
		}
		commits := 0
		q, err = q.WithProviderState(ProviderStateBinding{Intent: intent, ResponseID: "nrp_bbbbbbbbbbbbbbbbbbbbbbbb", Proof: proof, CommitResponse: func(id, status string) error {
			commits++
			if id != "resp_private_id" || status != "completed" {
				t.Error("Wrong provider receipt")
			}
			if fail {
				return errors.New("synthetic metadata outage")
			}
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		out := ForwardNative(context.Background(), w, q, route, time.Second)
		if commits != 1 || out.Usage == nil || out.Usage.TotalTokens != 8 || !out.Dispatched {
			t.Fatal("Confirmed cost or durable receipt was lost")
		}
		if strings.Contains(w.Body.String(), "resp_private_id") || strings.Contains(w.Body.String(), proof) {
			t.Fatal("Private supplier identity escaped")
		}
		if fail && w.Code != 502 || !fail && (w.Code != 200 || !strings.Contains(w.Body.String(), "nrp_bbbbbbbbbbbbbbbbbbbbbbbb")) {
			t.Fatal("Receipt publication boundary failed")
		}
	}
}

func TestProviderStateStreamCommitsBeforePublishingAndRejectsIdentitySwap(t *testing.T) {
	proof := strings.Repeat("b", 64)
	for _, swapped := range []bool{false, true} {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: response.created\ndata: "+`{"type":"response.created","response":{"id":"resp_original","status":"in_progress","metadata":{"_neon_binding":"`+proof+`"}}}`+"\n\n")
			id := "resp_original"
			if swapped {
				id = "resp_unrelated"
			}
			io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"`+id+`","status":"completed","metadata":{"_neon_binding":"`+proof+`"},"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`+"\n\n")
		}))
		c := nativeConfig(peer.URL)
		c.Routes[0].AllowProviderState = true
		q, route, intent, err := DecodeNativeProviderState([]byte(`{"model":"alias","input":"owned","max_output_tokens":16,"store":true,"stream":true}`), c)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		commits := 0
		q, err = q.WithProviderState(ProviderStateBinding{Intent: intent, ResponseID: "nrp_cccccccccccccccccccccccc", Proof: proof, CommitResponse: func(id, status string) error {
			commits++
			if id != "resp_original" || commits == 1 && w.Body.Len() != 0 {
				t.Error("Uncommitted supplier state was published")
			}
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		out := ForwardNative(context.Background(), w, q, route, time.Second)
		peer.Close()
		body := w.Body.String()
		if strings.Contains(body, "resp_original") || strings.Contains(body, "resp_unrelated") || strings.Contains(body, proof) || !strings.Contains(body, "nrp_cccccccccccccccccccccccc") {
			t.Fatal("Stream exposed supplier identity or proof")
		}
		if swapped && (commits != 1 || out.Usage != nil || !strings.Contains(body, "provider_stream_incomplete")) || !swapped && (commits != 2 || out.Status != "completed" || out.Usage == nil) {
			t.Fatal("Stream state transition was not bound to the original response")
		}
	}
}

func TestProviderStateUnprovenResponseCannotConfirmCostOrPublish(t *testing.T) {
	proof := strings.Repeat("c", 64)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"resp_unproven","object":"response","status":"completed","output":[],"metadata":{"_neon_binding":"another-request"},"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`)
	}))
	defer peer.Close()
	c := nativeConfig(peer.URL)
	c.Routes[0].AllowProviderState = true
	q, route, intent, err := DecodeNativeProviderState([]byte(`{"model":"alias","input":"owned","max_output_tokens":16,"store":true}`), c)
	if err != nil {
		t.Fatal(err)
	}
	commits := 0
	q, err = q.WithProviderState(ProviderStateBinding{Intent: intent, ResponseID: "nrp_dddddddddddddddddddddddd", Proof: proof, CommitResponse: func(id, status string) error { commits++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	out := ForwardNative(context.Background(), w, q, route, time.Second)
	if w.Code != 502 || !out.Dispatched || out.Usage != nil || commits != 0 || strings.Contains(w.Body.String(), "resp_unproven") {
		t.Fatal("Unproven supplier response was attributed to the current request")
	}
}
