package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marianina8/imessage-search/internal/demo"
)

func newDemo(t *testing.T) *httptest.Server {
	t.Helper()
	idx, err := demo.Build(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	ts := httptest.NewServer(New(t.TempDir(), idx, true).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestSearch(t *testing.T) {
	ts := newDemo(t)
	var res struct {
		Total   int
		Results []Message
	}
	getJSON(t, ts.URL+"/api/search?q=deposit&sender=Dana+Whitfield", &res)
	if res.Total != 2 || len(res.Results) != 2 || res.Results[0].Sender != "Dana Whitfield" {
		t.Fatalf("%+v", res)
	}
	getJSON(t, ts.URL+"/api/search?q=ceil*&sort=oldest&limit=1", &res)
	if res.Total < 5 || len(res.Results) != 1 || !strings.Contains(strings.ToLower(res.Results[0].Text), "ceiling") {
		t.Fatalf("%+v", res)
	}
	var ctx []Message
	getJSON(t, ts.URL+"/api/context?id=40&n=2", &ctx)
	if len(ctx) != 5 || ctx[2].ID != 40 {
		t.Fatalf("context %+v", ctx)
	}
}

func TestSummarizeNeedsKey(t *testing.T) {
	ts := newDemo(t)
	resp, err := http.Post(ts.URL+"/api/summarize", "application/json", strings.NewReader(`{"ids":[1]}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if !strings.Contains(out["error"].(string), "ANTHROPIC_API_KEY") {
		t.Fatalf("%v", out)
	}
}

func TestCrossOriginBlocked(t *testing.T) {
	ts := newDemo(t)
	req, _ := http.NewRequest("POST", ts.URL+"/api/export", strings.NewReader(`{"ids":[1]}`))
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestExport(t *testing.T) {
	ts := newDemo(t)
	resp, err := http.Post(ts.URL+"/api/export", "application/json",
		strings.NewReader(`{"ids":[45],"context":2,"title":"Deposit / dispute","filters":{"q":"deposit"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Token string
		Count int
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 5 {
		t.Fatalf("count %d", out.Count)
	}
	page, _ := http.Get(ts.URL + "/report/" + out.Token)
	var b strings.Builder
	buf := make([]byte, 1<<16)
	for {
		n, err := page.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(b.String(), "Deposit / dispute") || !strings.Contains(b.String(), `class="msg match"`) {
		t.Fatal("report is missing its title or the matched message")
	}
	csv, _ := http.Get(ts.URL + "/report/" + out.Token + ".csv")
	if cd := csv.Header.Get("Content-Disposition"); cd != `attachment; filename="Deposit  dispute.csv"` {
		t.Fatalf("filename %q", cd)
	}
}

func TestFTSQuery(t *testing.T) {
	cases := map[[2]string]string{
		{`dinner friday`, "all"}:    `"dinner" AND "friday"`,
		{`pay* "exact one"`, "any"}: `"pay"* OR "exact one"`,
		{`he said "hi`, "phrase"}:   `"he said  hi"`,
		{`""  *  a"b`, "all"}:       `"ab"`,
	}
	for in, want := range cases {
		if got := ftsQuery(in[0], in[1]); got != want {
			t.Errorf("%q %s: got %s want %s", in[0], in[1], got, want)
		}
	}
}
