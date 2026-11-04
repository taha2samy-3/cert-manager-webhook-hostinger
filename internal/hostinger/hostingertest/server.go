// Package hostingertest provides an in-memory fake of the Hostinger DNS zone
// API, plus a DNS server answering TXT queries from the same data, so the
// solver and the cert-manager conformance suite can run without credentials.
//
// The fake copies what the real API (developers.hostinger.com) did when it
// was probed on 2026-11-04:
//
//   - a zone is a list of record sets (name, type, ttl, records); there are
//     no record IDs
//   - names are stored relative to the zone and in lower case, with "@" for
//     the apex; a PUT may send full names ("www.example.com",
//     "www.example.com.", "example.com") and they are made relative
//   - "*" is only allowed as the whole first label; an empty name is a 422
//     field error
//   - the TTL must be 60 to 86400; a record set sent without one gets 1800
//   - a record set needs at least one value, and the same value twice is a
//     422
//   - PUT with overwrite=true (the default) replaces the record sets it
//     names; overwrite=false adds the values and sets the TTL, and adding a
//     value that is already there is 422 [DNS:4008]
//   - a PUT is all or nothing: one bad record set fails the whole call
//   - TXT values are stored in quotes, `\\` becomes `\`, and a bare quote
//     inside a value is 422 [DNS:4008]
//   - a CNAME has one value and no other type may share its name
//   - SOA, and NS below the apex, cannot be written (422 [DNS:4005])
//   - DELETE removes whole record sets by name and type; the name must be
//     spelled exactly as stored (relative, lower case), otherwise nothing is
//     deleted and the answer is still 200
//   - a domain that is not in the account is 403 [DNS:4002]
//   - errors are {"message", "correlation_id"}, plus "errors" with the
//     fields of a 422
//   - every answer carries the RateLimit headers (90 requests per minute);
//     the fake never answers 429
package hostingertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const zonePrefix = "/api/dns/v1/zones/"

// rateLimit is the real API's budget per user and minute.
const rateLimit = 90

var validTypes = map[string]bool{
	"A": true, "AAAA": true, "CNAME": true, "ALIAS": true, "MX": true,
	"TXT": true, "NS": true, "SOA": true, "SRV": true, "CAA": true,
}

// Record is one value of a record set. IsDisabled marks a value that was
// turned off in hPanel; the API returns the flag but cannot set it.
type Record struct {
	Content    string `json:"content"`
	IsDisabled bool   `json:"is_disabled"`
}

// RecordSet is a record set as the API stores and returns it.
type RecordSet struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int      `json:"ttl"`
	Records []Record `json:"records"`
}

// Fake is an in-memory Hostinger DNS API. It implements http.Handler and
// can additionally serve its zones over DNS.
type Fake struct {
	Token string

	mu      sync.Mutex
	zones   map[string][]RecordSet
	calls   map[string]int
	corrSeq int
	// requests in the current rate limit window, for the RateLimit headers
	windowStart time.Time
	used        int
}

// New returns a fake API for the given zones (domain names without trailing
// dot). Requests must carry "Authorization: Bearer <token>".
func New(token string, zones ...string) *Fake {
	f := &Fake{Token: token, zones: map[string][]RecordSet{}, calls: map[string]int{}}
	for _, z := range zones {
		f.zones[strings.ToLower(z)] = []RecordSet{}
	}
	return f
}

// Server is a Fake listening on a local httptest server.
type Server struct {
	*Fake
	*httptest.Server
}

// NewServer starts a Fake on a random local port.
func NewServer(token string, zones ...string) *Server {
	f := New(token, zones...)
	return &Server{Fake: f, Server: httptest.NewServer(f)}
}

// TXT returns the unquoted values of the TXT set called name in zone,
// leaving out values that are turned off, like the name servers do.
func (s *Fake) TXT(zone, name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txtLocked(zone, name)
}

// Calls returns how many requests were made with the given HTTP method.
func (s *Fake) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

// Seed stores a record set as is, without the checks of a PUT, replacing a
// set with the same name and type. Use it for data the API itself refuses
// to write, such as a TTL of 50 or values turned off in hPanel.
func (s *Fake) Seed(zone string, set RecordSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sets, ok := s.zones[zone]
	if !ok {
		panic("hostingertest: unknown zone " + zone)
	}
	set.Records = slices.Clone(set.Records)
	if i := indexOf(sets, set.Name, set.Type); i >= 0 {
		sets[i] = set
		return
	}
	s.zones[zone] = append(sets, set)
}

// RecordSets returns a copy of the record sets stored in zone.
func (s *Fake) RecordSets(zone string) []RecordSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSets(s.zones[zone])
}

func (s *Fake) txtLocked(zone, name string) []string {
	var out []string
	for _, rs := range s.zones[zone] {
		if rs.Type == "TXT" && rs.Name == name {
			for _, r := range rs.Records {
				if !r.IsDisabled {
					out = append(out, strings.Trim(r.Content, `"`))
				}
			}
		}
	}
	return out
}

func (s *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimitHeadersLocked(w.Header())

	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		s.errorLocked(w, http.StatusUnauthorized, "Unauthenticated.", nil)
		return
	}
	zone, ok := strings.CutPrefix(r.URL.Path, zonePrefix)
	if !ok || zone == "" || strings.Contains(zone, "/") {
		s.errorLocked(w, http.StatusNotFound, "Not found", nil)
		return
	}
	s.calls[r.Method]++

	zone = strings.ToLower(zone)
	if _, exists := s.zones[zone]; !exists {
		s.errorLocked(w, http.StatusForbidden, fmt.Sprintf("[DNS:4002] Customer does not own %s domain", zone), nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, cloneSets(s.zones[zone]))
	case http.MethodPut:
		s.updateLocked(w, r, zone)
	case http.MethodDelete:
		s.deleteLocked(w, r, zone)
	default:
		s.errorLocked(w, http.StatusMethodNotAllowed, "method not allowed", nil)
	}
}

// rateLimitHeadersLocked counts the request and sets the headers the real
// API sends with every answer.
func (s *Fake) rateLimitHeadersLocked(h http.Header) {
	now := time.Now()
	if now.Sub(s.windowStart) >= time.Minute {
		s.windowStart, s.used = now, 0
	}
	s.used++
	left := max(0, rateLimit-s.used)
	reset := max(1, int((time.Minute - now.Sub(s.windowStart)).Seconds()))
	h.Set("RateLimit-Policy", fmt.Sprintf(`"api";q=%d;w=60`, rateLimit))
	h.Set("RateLimit", fmt.Sprintf(`"api";r=%d;t=%d`, left, reset))
	h.Set("X-RateLimit-Limit", strconv.Itoa(rateLimit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(left))
}

type updateItem struct {
	Name    *string `json:"name"`
	Type    string  `json:"type"`
	TTL     *int    `json:"ttl"`
	Records []struct {
		Content *string `json:"content"`
	} `json:"records"`
}

func (s *Fake) updateLocked(w http.ResponseWriter, r *http.Request, zone string) {
	var req struct {
		Overwrite *bool        `json:"overwrite"`
		Zone      []updateItem `json:"zone"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.fieldErrorsLocked(w, map[string][]string{"zone": {"The zone field is required."}})
		return
	}
	if fields := checkUpdateFields(req.Zone); len(fields) > 0 {
		s.fieldErrorsLocked(w, fields)
		return
	}
	overwrite := req.Overwrite == nil || *req.Overwrite // API default is true

	// Work on a copy so that one bad record set leaves the zone unchanged.
	next := cloneSets(s.zones[zone])
	for _, in := range req.Zone {
		name, ok := relativeName(*in.Name, zone)
		if !ok {
			s.errorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] DNS record validation error : Invalid RRset name "+fqdnOf(name, zone), nil)
			return
		}
		ttl := 1800
		if in.TTL != nil {
			ttl = *in.TTL
		}
		if ttl < 60 {
			s.errorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] Resource record TTL must be at least 60 seconds!", nil)
			return
		}
		if ttl > 86400 {
			s.errorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] Resource record TTL cannot exceed 86400 seconds!", nil)
			return
		}
		if in.Type == "SOA" || (in.Type == "NS" && name != "@") {
			s.errorLocked(w, http.StatusUnprocessableEntity, "[DNS:4005] Resource record type is invalid, unsupported or immutable! "+
				"Supported types: A, AAAA, ALIAS, CNAME, MX, TXT, SRV, CAA.", nil)
			return
		}
		var values []Record
		for _, rec := range in.Records {
			content, ok := normalizeContent(in.Type, *rec.Content)
			if !ok {
				s.errorLocked(w, http.StatusUnprocessableEntity, "[DNS:4008] DNS resource record is not valid or conflicts with another resource record", nil)
				return
			}
			if hasContent(values, content) {
				s.errorLocked(w, http.StatusUnprocessableEntity, fmt.Sprintf(
					"[DNS:4005] DNS record validation error : Duplicate record in RRset %s IN %s with content %s", fqdnOf(name, zone), in.Type, content), nil)
				return
			}
			values = append(values, Record{Content: content})
		}

		idx := indexOf(next, name, in.Type)
		switch {
		case idx < 0:
			next = append(next, RecordSet{Name: name, Type: in.Type, TTL: ttl, Records: values})
		case overwrite:
			next[idx] = RecordSet{Name: name, Type: in.Type, TTL: ttl, Records: values}
		default:
			for _, v := range values {
				if hasContent(next[idx].Records, v.Content) {
					s.errorLocked(w, http.StatusUnprocessableEntity, "[DNS:4008] DNS resource record is not valid or conflicts with another resource record", nil)
					return
				}
			}
			next[idx].Records = append(next[idx].Records, values...)
			next[idx].TTL = ttl
		}
	}
	if msg := checkCNAMEs(next, zone); msg != "" {
		s.errorLocked(w, http.StatusUnprocessableEntity, msg, nil)
		return
	}
	s.zones[zone] = next
	writeJSON(w, http.StatusOK, map[string]string{"message": "Request accepted"})
}

func checkUpdateFields(items []updateItem) map[string][]string {
	fields := map[string][]string{}
	if len(items) == 0 {
		fields["zone"] = []string{"The zone field is required."}
	}
	for i, it := range items {
		p := "zone." + strconv.Itoa(i)
		if it.Name == nil || *it.Name == "" {
			fields[p+".name"] = []string{"The " + p + ".name field is required."}
		}
		if it.Type == "" {
			fields[p+".type"] = []string{"The " + p + ".type field is required."}
		} else if !validTypes[it.Type] {
			fields[p+".type"] = []string{"The selected " + p + ".type is invalid."}
		}
		if len(it.Records) == 0 {
			fields[p+".records"] = []string{"The " + p + ".records field is required."}
		}
		for j, rec := range it.Records {
			if rec.Content == nil || *rec.Content == "" {
				k := p + ".records." + strconv.Itoa(j) + ".content"
				fields[k] = []string{"The " + k + " field is required."}
			}
		}
	}
	return fields
}

// checkCNAMEs enforces the rules that involve several record sets.
func checkCNAMEs(sets []RecordSet, zone string) string {
	for i, set := range sets {
		if set.Type != "CNAME" {
			continue
		}
		fqdn := fqdnOf(set.Name, zone)
		if len(set.Records) > 1 {
			return fmt.Sprintf("[DNS:4005] DNS record validation error : RRset %s IN CNAME has more than one record", fqdn)
		}
		for j, other := range sets {
			if j != i && other.Name == set.Name {
				return fmt.Sprintf("[DNS:4005] DNS record validation error : RRset %s IN CNAME must not be used with any other type on the same name.", fqdn)
			}
		}
	}
	return ""
}

func (s *Fake) deleteLocked(w http.ResponseWriter, r *http.Request, zone string) {
	var req struct {
		Filters []struct {
			Name *string `json:"name"`
			Type string  `json:"type"`
		} `json:"filters"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	fields := map[string][]string{}
	if len(req.Filters) == 0 {
		fields["filters"] = []string{"The filters field is required."}
	}
	for i, f := range req.Filters {
		p := "filters." + strconv.Itoa(i)
		if f.Name == nil || *f.Name == "" {
			fields[p+".name"] = []string{"The " + p + ".name field is required."}
		}
		if !validTypes[f.Type] {
			fields[p+".type"] = []string{"The selected " + p + ".type is invalid."}
		}
	}
	if len(fields) > 0 {
		s.fieldErrorsLocked(w, fields)
		return
	}
	sets := s.zones[zone]
	for _, f := range req.Filters {
		// The real API matches the stored name exactly: "WWW" or
		// "www.example.com" delete nothing, and the answer is still 200.
		if idx := indexOf(sets, *f.Name, f.Type); idx >= 0 {
			sets = slices.Delete(sets, idx, idx+1)
		}
	}
	s.zones[zone] = sets
	writeJSON(w, http.StatusOK, map[string]string{"message": "Request accepted"})
}

func indexOf(sets []RecordSet, name, typ string) int {
	return slices.IndexFunc(sets, func(rs RecordSet) bool { return rs.Name == name && rs.Type == typ })
}

func hasContent(records []Record, content string) bool {
	return slices.ContainsFunc(records, func(r Record) bool { return r.Content == content })
}

func cloneSets(in []RecordSet) []RecordSet {
	out := make([]RecordSet, 0, len(in))
	for _, rs := range in {
		rs.Records = slices.Clone(rs.Records)
		out = append(out, rs)
	}
	return out
}

// relativeName turns a name from a PUT body into the stored form. It
// returns false for a name the API refuses.
func relativeName(name, zone string) (string, bool) {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	switch {
	case n == "@" || n == zone:
		return "@", true
	case strings.HasSuffix(n, "."+zone):
		n = strings.TrimSuffix(n, "."+zone)
	}
	for i, label := range strings.Split(n, ".") {
		if label == "*" && i == 0 {
			continue
		}
		if label == "" || len(label) > 63 || strings.ContainsFunc(label, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_'
		}) {
			return n, false
		}
	}
	return n, true
}

func fqdnOf(name, zone string) string {
	if name == "@" {
		return zone
	}
	return name + "." + zone
}

// normalizeContent rewrites a value the way the real API stores it, and
// reports whether the API accepts it. Only TXT is modelled in detail; this
// webhook writes nothing else.
func normalizeContent(typ, content string) (string, bool) {
	content = strings.TrimSpace(content)
	if typ != "TXT" {
		return content, true
	}
	if !quoted(content) {
		content = `"` + content + `"`
	}
	if !validTXT(content) {
		return "", false
	}
	return strings.ReplaceAll(content, `\\`, `\`), true
}

func quoted(s string) bool { return len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' }

// validTXT accepts one or more quoted strings separated by spaces, where a
// quote inside a string is escaped with a backslash.
func validTXT(c string) bool {
	in := false
	for i := 0; i < len(c); i++ {
		switch ch := c[i]; {
		case ch == '\\' && in:
			i++
		case ch == '"':
			in = !in
		case !in && ch != ' ':
			return false
		}
	}
	return !in
}

// fieldErrorsLocked answers 422 the way the API reports invalid fields: the
// first message, a count of the others, and every field in "errors".
func (s *Fake) fieldErrorsLocked(w http.ResponseWriter, fields map[string][]string) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	msg := fields[keys[0]][0]
	switch n := len(keys) - 1; {
	case n == 1:
		msg += " (and 1 more error)"
	case n > 1:
		msg += fmt.Sprintf(" (and %d more errors)", n)
	}
	s.errorLocked(w, http.StatusUnprocessableEntity, msg, fields)
}

func (s *Fake) errorLocked(w http.ResponseWriter, status int, msg string, fields map[string][]string) {
	s.corrSeq++
	body := map[string]any{"message": msg, "correlation_id": fmt.Sprintf("fake-%08d", s.corrSeq)}
	if fields != nil {
		body["errors"] = fields
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// StartDNS serves the zones over UDP on a random 127.0.0.1 port and returns
// the listen address and a shutdown function.
func (s *Fake) StartDNS() (string, func() error, error) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &dns.Server{PacketConn: pc, Handler: s}
	go func() { _ = srv.ActivateAndServe() }()
	return pc.LocalAddr().String(), srv.Shutdown, nil
}

// ServeDNS answers SOA queries for zone apexes and TXT queries from the fake
// zone data. Every name under a fake zone carries the zone SOA in the
// authority section, which is how cert-manager discovers the zone.
func (s *Fake) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true

	s.mu.Lock()
	for _, q := range req.Question {
		fqdn := strings.ToLower(strings.TrimSuffix(q.Name, "."))
		zone, name, ok := s.findZoneLocked(fqdn)
		if !ok {
			msg.Rcode = dns.RcodeRefused
			continue
		}
		switch {
		case q.Qtype == dns.TypeSOA && name == "@":
			msg.Answer = append(msg.Answer, soa(zone))
		case q.Qtype == dns.TypeTXT:
			for _, v := range s.txtLocked(zone, name) {
				msg.Answer = append(msg.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
					Txt: []string{v},
				})
			}
		}
		if len(msg.Answer) == 0 {
			msg.Ns = append(msg.Ns, soa(zone))
		}
	}
	s.mu.Unlock()

	_ = w.WriteMsg(msg)
}

func (s *Fake) findZoneLocked(fqdn string) (zone, name string, ok bool) {
	for z := range s.zones {
		if fqdn == z {
			return z, "@", true
		}
		if n, found := strings.CutSuffix(fqdn, "."+z); found {
			return z, n, true
		}
	}
	return "", "", false
}

func soa(zone string) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: dns.Fqdn(zone), Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
		Ns:      "ns1." + dns.Fqdn(zone),
		Mbox:    "hostmaster." + dns.Fqdn(zone),
		Serial:  1,
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  60,
	}
}
