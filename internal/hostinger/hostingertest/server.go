// Package hostingertest provides an in-memory fake of the Hostinger DNS zone
// API, plus a DNS server answering TXT queries from the same data, so the
// solver and the cert-manager conformance suite can run without credentials.
package hostingertest

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

const zonePrefix = "/api/dns/v1/zones/"

type record struct {
	Content string `json:"content"`
}

type recordSet struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int      `json:"ttl,omitempty"`
	Records []record `json:"records"`
}

// Fake is an in-memory Hostinger DNS API. It implements http.Handler and
// can additionally serve its zones over DNS.
type Fake struct {
	Token string

	mu    sync.Mutex
	zones map[string][]recordSet
	calls map[string]int
}

// New returns a fake API for the given zones (domain names without trailing
// dot). Requests must carry "Authorization: Bearer <token>".
func New(token string, zones ...string) *Fake {
	f := &Fake{Token: token, zones: map[string][]recordSet{}, calls: map[string]int{}}
	for _, z := range zones {
		f.zones[strings.ToLower(z)] = nil
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

// TXT returns the unquoted values of the TXT set called name in zone.
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

func (s *Fake) txtLocked(zone, name string) []string {
	var out []string
	for _, rs := range s.zones[zone] {
		if rs.Type == "TXT" && rs.Name == name {
			for _, r := range rs.Records {
				out = append(out, strings.Trim(r.Content, `"`))
			}
		}
	}
	return out
}

func (s *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
		return
	}
	zone, ok := strings.CutPrefix(r.URL.Path, zonePrefix)
	if !ok || strings.Contains(zone, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[r.Method]++

	sets, exists := s.zones[zone]
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Zone not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Like the real API, TXT contents come back wrapped in quotes.
		out := make([]recordSet, 0, len(sets))
		for _, rs := range sets {
			cp := rs
			cp.Records = nil
			for _, rec := range rs.Records {
				if rs.Type == "TXT" {
					rec.Content = `"` + rec.Content + `"`
				}
				cp.Records = append(cp.Records, rec)
			}
			out = append(out, cp)
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPut:
		var req struct {
			Overwrite *bool       `json:"overwrite"`
			Zone      []recordSet `json:"zone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Zone) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid body"})
			return
		}
		overwrite := req.Overwrite == nil || *req.Overwrite // API default is true
		for _, in := range req.Zone {
			idx := indexOf(sets, in.Name, in.Type)
			switch {
			case idx < 0:
				sets = append(sets, in)
			case overwrite:
				sets[idx] = in
			default:
				if in.TTL != 0 {
					sets[idx].TTL = in.TTL
				}
				sets[idx].Records = append(sets[idx].Records, in.Records...)
			}
		}
		s.zones[zone] = sets
		writeJSON(w, http.StatusOK, map[string]string{})

	case http.MethodDelete:
		var req struct {
			Filters []struct{ Name, Type string } `json:"filters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Filters) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid body"})
			return
		}
		for _, f := range req.Filters {
			if idx := indexOf(sets, f.Name, f.Type); idx >= 0 {
				sets = append(sets[:idx], sets[idx+1:]...)
			}
		}
		s.zones[zone] = sets
		writeJSON(w, http.StatusOK, map[string]string{})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
	}
}

func indexOf(sets []recordSet, name, typ string) int {
	for i, rs := range sets {
		if rs.Name == name && rs.Type == typ {
			return i
		}
	}
	return -1
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
