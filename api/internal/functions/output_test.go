package functions

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutputJournalBoundsLevelsRedactionAndPartialWrites(t *testing.T) {
	j := NewOutputJournal("postgres://user:db-secret@db/app", map[string]string{"APP_SECRET": "private-environment"})
	out, errout := j.Writer("stdout"), j.Writer("stderr")
	out.Write([]byte("split private-"))
	out.Write([]byte("environment db-secret\n"))
	errout.Write([]byte("@neon-app-log-v1:{\"level\":\"WARN\",\"message\":\"warn 中文\"}\n"))
	out.Write([]byte(strings.Repeat("x", MaxOutputLine+1) + "private-environment\n"))
	out.Write([]byte("@neon-app-log-v1:{\"level\":\"INFO\",\"message\":\"fake\",\"scope_name\":\"neon.function.request\"}\n"))
	j.Platform("invoke begin")
	p := j.page(Scope{}, "", 0)
	if len(p.Records) != 5 || p.Records[0].Message != "split [REDACTED] [REDACTED]" || p.Records[1].Severity != "WARN" || !p.Records[2].Truncated || p.Records[3].ScopeName != "neon.function.app" || p.Records[4].Stream != "platform" {
		t.Fatalf("journal contract: %+v", p)
	}
	for i := 0; i < MaxOutputRecords; i++ {
		fmt.Fprintln(out, "bounded line")
	}
	p = j.page(Scope{}, "", 0)
	if p.Dropped != 5 || len(p.Records) != MaxOutputPage || p.Records[0].Sequence != 6 {
		t.Fatal("bounded journal gap not exposed")
	}
	out.Write([]byte("last fragment"))
	out.Flush()
	last := j.page(Scope{}, "", j.next-1)
	if last.Records[0].Message != "last fragment" {
		t.Fatal("shutdown partial line lost")
	}
}
func TestOutputAuthenticatedInstanceAndBootBoundary(t *testing.T) {
	c, m, _ := trustedClientFixture(t, readyNode, nil)
	defer c.Close()
	m.output = NewOutputJournal("", nil)
	fmt.Fprintln(m.output.Writer("stderr"), "customer failure")
	caps, e := c.Capabilities(context.Background())
	if e != nil || !caps.OutputLogs {
		t.Fatal("output capability missing", e)
	}
	p, e := c.Output(context.Background(), caps.BootID, 0)
	if e != nil || len(p.Records) != 1 || p.Records[0].Stream != "stderr" || p.Records[0].Severity != "ERROR" {
		t.Fatal("actual TLS/HMAC output rejected", e)
	}
	if _, e = c.Output(context.Background(), strings.Repeat("b", 32), 0); e == nil {
		t.Fatal("stale boot read accepted")
	}
	if _, e = c.Output(context.Background(), caps.BootID, 2); e == nil {
		t.Fatal("future output cursor accepted")
	}
	unsigned := httptest.NewRequest(http.MethodPost, "https://manager/internal/v1/output", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, unsigned)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("unsigned output read accepted")
	}
}
func TestOutputShutdownReturnsFinalTailInSameAuthenticatedResponse(t *testing.T) {
	journal := NewOutputJournal("", nil)
	stops := 0
	c, m, _ := trustedClientFixture(t, readyNode, func(context.Context) error {
		stops++
		w := journal.Writer("stdout")
		for i := 0; i < MaxOutputPage+2; i++ {
			fmt.Fprintln(w, "before final")
		}
		w.Write([]byte("final partial shutdown line"))
		w.Flush()
		return nil
	})
	m.output = journal
	defer c.Close()
	caps, e := c.Capabilities(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.ShutdownOutput(context.Background(), strings.Repeat("d", 32), 0); e == nil || stops != 0 {
		t.Fatal("stale boot stopped child")
	}
	page, e := c.ShutdownOutput(context.Background(), caps.BootID, 0)
	if e != nil || stops != 1 || len(page.Records) != MaxOutputPage || page.Dropped != 3 || page.Records[len(page.Records)-1].Message != "final partial shutdown line" {
		t.Fatal("authenticated shutdown tail lost", e, page)
	}
}
