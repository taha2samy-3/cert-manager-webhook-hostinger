package hostingertest

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// These tests pin the fake to what the real Hostinger API did when it was
// probed on 2026-11-04. If the real API turns out to behave differently,
// change the fake and these tests together.

const (
	token    = "test-token"
	zoneName = "example.com"
)

func newFake(t *testing.T) *Server {
	t.Helper()
	s := NewServer(token, zoneName)
	t.Cleanup(s.Close)
	return s
}

func call(t *testing.T, s *Server, method, path, body string) (int, string, http.Header) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func put(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	code, b, _ := call(t, s, http.MethodPut, zonePrefix+zoneName, body)
	return code, b
}

func del(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	code, b, _ := call(t, s, http.MethodDelete, zonePrefix+zoneName, body)
	return code, b
}

func set(s *Server, name, typ string) *RecordSet {
	for _, rs := range s.RecordSets(zoneName) {
		if rs.Name == name && rs.Type == typ {
			return &rs
		}
	}
	return nil
}

func contents(rs *RecordSet) []string {
	if rs == nil {
		return nil
	}
	var out []string
	for _, r := range rs.Records {
		out = append(out, r.Content)
	}
	return out
}

func TestGet(t *testing.T) {
	s := newFake(t)
	code, body, h := call(t, s, http.MethodGet, zonePrefix+zoneName, "")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty zone: %d %s", code, body)
	}
	if h.Get("RateLimit-Policy") != `"api";q=90;w=60` || !strings.HasPrefix(h.Get("RateLimit"), `"api";r=89;t=`) ||
		h.Get("X-RateLimit-Limit") != "90" || h.Get("X-RateLimit-Remaining") != "89" {
		t.Errorf("rate limit headers: %v", h)
	}

	s.Seed(zoneName, RecordSet{Name: "@", Type: "A", TTL: 50, Records: []Record{{Content: "192.0.2.1"}}})
	_, body, _ = call(t, s, http.MethodGet, zonePrefix+zoneName, "")
	var sets []RecordSet
	if err := json.Unmarshal([]byte(body), &sets); err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].TTL != 50 || !strings.Contains(body, `"is_disabled":false`) {
		t.Errorf("GET = %s", body)
	}
}

func TestAccountAndAuth(t *testing.T) {
	s := newFake(t)
	code, b, _ := call(t, s, http.MethodGet, zonePrefix+"other.org", "")
	if code != http.StatusForbidden || !strings.Contains(b, "[DNS:4002] Customer does not own other.org domain") || !strings.Contains(b, `"correlation_id"`) {
		t.Errorf("domain outside the account: %d %s", code, b)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL+zonePrefix+zoneName, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b2), `"message":"Unauthenticated."`) {
		t.Errorf("bad token: %d %s", resp.StatusCode, b2)
	}
}

func TestNames(t *testing.T) {
	s := newFake(t)
	for _, n := range []string{"full.example.com", "dot.example.com.", "UPPER", "*.wild", "_acme-challenge.sub", "example.com"} {
		if code, b := put(t, s, `{"zone":[{"name":"`+n+`","type":"TXT","ttl":300,"records":[{"content":"x"}]}]}`); code != http.StatusOK {
			t.Fatalf("name %q: %d %s", n, code, b)
		}
	}
	for _, want := range []string{"full", "dot", "upper", "*.wild", "_acme-challenge.sub", "@"} {
		if set(s, want, "TXT") == nil {
			t.Errorf("no record set stored as %q: %+v", want, s.RecordSets(zoneName))
		}
	}
	code, b := put(t, s, `{"zone":[{"name":"a.*.mid","type":"TXT","ttl":300,"records":[{"content":"x"}]}]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(b, "[DNS:4005] DNS record validation error : Invalid RRset name a.*.mid.example.com") {
		t.Errorf("* in the middle: %d %s", code, b)
	}
	code, b = put(t, s, `{"zone":[{"name":"","type":"TXT","ttl":300,"records":[{"content":"x"}]}]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(b, `"zone.0.name":["The zone.0.name field is required."]`) {
		t.Errorf("empty name: %d %s", code, b)
	}
}

func TestTTL(t *testing.T) {
	s := newFake(t)
	for ttl, want := range map[string]string{"59": "at least 60 seconds", "86401": "cannot exceed 86400 seconds"} {
		code, b := put(t, s, `{"zone":[{"name":"t","type":"TXT","ttl":`+ttl+`,"records":[{"content":"x"}]}]}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(b, want) {
			t.Errorf("ttl %s: %d %s", ttl, code, b)
		}
	}
	for _, ttl := range []string{"60", "86400"} {
		if code, b := put(t, s, `{"zone":[{"name":"t","type":"TXT","ttl":`+ttl+`,"records":[{"content":"x"}]}]}`); code != http.StatusOK {
			t.Errorf("ttl %s: %d %s", ttl, code, b)
		}
	}
	put(t, s, `{"zone":[{"name":"nottl","type":"TXT","records":[{"content":"x"}]}]}`)
	if rs := set(s, "nottl", "TXT"); rs == nil || rs.TTL != 1800 {
		t.Errorf("a missing TTL must become 1800: %+v", rs)
	}
}

func TestTXTContent(t *testing.T) {
	s := newFake(t)
	if code, b := put(t, s, `{"zone":[{"name":"txt","type":"TXT","ttl":300,"records":[{"content":"plain"},{"content":"\"quoted\""},{"content":"\"a\" \"b\""},{"content":"\"x\\\\y\""}]}]}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, b)
	}
	want := []string{`"plain"`, `"quoted"`, `"a" "b"`, `"x\y"`}
	if got := contents(set(s, "txt", "TXT")); !slices.Equal(got, want) {
		t.Errorf("stored %q, want %q", got, want)
	}
	if got := s.TXT(zoneName, "txt"); got[0] != "plain" {
		t.Errorf("TXT() must return unquoted values: %q", got)
	}
	code, b := put(t, s, `{"zone":[{"name":"bad","type":"TXT","ttl":300,"records":[{"content":"say \"hi\""}]}]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(b, "[DNS:4008]") {
		t.Errorf("bare quote inside a TXT value: %d %s", code, b)
	}
}

func TestRecordSetRules(t *testing.T) {
	s := newFake(t)
	s.Seed(zoneName, RecordSet{Name: "www", Type: "CNAME", TTL: 300, Records: []Record{{Content: "example.com."}}})
	cases := map[string]string{
		`{"zone":[{"name":"d","type":"TXT","ttl":300,"records":[{"content":"v"},{"content":"\"v\""}]}]}`:                         `Duplicate record in RRset d.example.com IN TXT with content \"v\"`,
		`{"zone":[{"name":"e","type":"TXT","ttl":300,"records":[]}]}`:                                                            "The zone.0.records field is required.",
		`{"zone":[{"name":"e","type":"txt","ttl":300,"records":[{"content":"v"}]}]}`:                                             "The selected zone.0.type is invalid.",
		`{"zone":[{"name":"www","type":"TXT","ttl":300,"records":[{"content":"v"}]}]}`:                                           "must not be used with any other type on the same name",
		`{"zone":[{"name":"c","type":"CNAME","ttl":300,"records":[{"content":"a.example.net."},{"content":"b.example.net."}]}]}`: "has more than one record",
		`{"zone":[{"name":"sub","type":"NS","ttl":300,"records":[{"content":"ns1.example.net."}]}]}`:                             "Resource record type is invalid, unsupported or immutable!",
		`{"zone":[{"name":"@","type":"SOA","ttl":300,"records":[{"content":"a. b. 1 2 3 4 5"}]}]}`:                               "Resource record type is invalid, unsupported or immutable!",
		`{"zone":[]}`: "The zone field is required.",
	}
	for body, want := range cases {
		if code, b := put(t, s, body); code != http.StatusUnprocessableEntity || !strings.Contains(b, want) {
			t.Errorf("%s\n got %d %s\nwant 422 with %q", body, code, b, want)
		}
	}
}

func TestPutIsAllOrNothing(t *testing.T) {
	s := newFake(t)
	code, _ := put(t, s, `{"zone":[{"name":"good","type":"TXT","ttl":300,"records":[{"content":"v"}]},{"name":"bad","type":"TXT","ttl":30,"records":[{"content":"v"}]}]}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d", code)
	}
	if set(s, "good", "TXT") != nil {
		t.Fatal("a refused PUT must not write any of its record sets")
	}
}

func TestOverwrite(t *testing.T) {
	s := newFake(t)
	put(t, s, `{"overwrite":true,"zone":[{"name":"o","type":"TXT","ttl":300,"records":[{"content":"a"},{"content":"b"}]}]}`)
	put(t, s, `{"overwrite":false,"zone":[{"name":"o","type":"TXT","ttl":600,"records":[{"content":"c"}]}]}`)
	if rs := set(s, "o", "TXT"); rs == nil || len(rs.Records) != 3 || rs.TTL != 600 {
		t.Fatalf("overwrite=false must append and set the TTL: %+v", rs)
	}
	for _, v := range []string{`c`, `\"c\"`} {
		if code, b := put(t, s, `{"overwrite":false,"zone":[{"name":"o","type":"TXT","ttl":600,"records":[{"content":"`+v+`"}]}]}`); code != http.StatusUnprocessableEntity || !strings.Contains(b, "[DNS:4008]") {
			t.Errorf("appending an existing value %s: %d %s", v, code, b)
		}
	}
	put(t, s, `{"zone":[{"name":"o","type":"TXT","ttl":300,"records":[{"content":"z"}]}]}`)
	if got := contents(set(s, "o", "TXT")); !slices.Equal(got, []string{`"z"`}) {
		t.Fatalf("overwrite must default to true and replace the set: %q", got)
	}
}

func TestDeleteMatchesTheStoredNameOnly(t *testing.T) {
	s := newFake(t)
	put(t, s, `{"zone":[{"name":"keep","type":"TXT","ttl":300,"records":[{"content":"v"}]}]}`)
	for _, n := range []string{"keep.example.com", "KEEP", "keep.example.com.", "missing"} {
		if code, b := del(t, s, `{"filters":[{"name":"`+n+`","type":"TXT"}]}`); code != http.StatusOK {
			t.Fatalf("%s: %d %s", n, code, b)
		}
	}
	if set(s, "keep", "TXT") == nil {
		t.Fatal("only the exact stored name may delete a record set")
	}
	if code, b := del(t, s, `{"filters":[{"name":"keep","type":"TXT"}]}`); code != http.StatusOK || set(s, "keep", "TXT") != nil {
		t.Fatalf("exact name must delete: %d %s", code, b)
	}
	if code, b := del(t, s, `{"filters":[]}`); code != http.StatusUnprocessableEntity || !strings.Contains(b, "The filters field is required.") {
		t.Errorf("empty filters: %d %s", code, b)
	}
	if code, b := del(t, s, `{"filters":[{"name":"x","type":"txt"}]}`); code != http.StatusUnprocessableEntity || !strings.Contains(b, "filters.0.type is invalid") {
		t.Errorf("lower case type: %d %s", code, b)
	}
}

func TestDisabledValuesAreNotServed(t *testing.T) {
	s := newFake(t)
	s.Seed(zoneName, RecordSet{Name: "_acme-challenge", Type: "TXT", TTL: 60, Records: []Record{
		{Content: `"on"`}, {Content: `"off"`, IsDisabled: true},
	}})
	if got := s.TXT(zoneName, "_acme-challenge"); !slices.Equal(got, []string{"on"}) {
		t.Fatalf("TXT = %q, want [on]", got)
	}
}
