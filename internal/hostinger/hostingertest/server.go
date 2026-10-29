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

// Server is a fake Hostinger API.
type Server struct {
	*httptest.Server

	Token string

	mu    sync.Mutex
	zones map[string][]recordSet
	calls map[string]int
}

// NewServer starts a fake API serving the given zones (domain names without
// trailing dot). Requests must carry "Authorization: Bearer <token>".
func NewServer(token string, zones ...string) *Server {
	s := &Server{Token: token, zones: map[string][]recordSet{}, calls: map[string]int{}}
	for _, z := range zones {
		s.zones[strings.ToLower(z)] = nil
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// TXT returns the unquoted values of the TXT set called name in zone.
func (s *Server) TXT(zone, name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.txtLocked(zone, name)
}

// Calls returns how many requests were made with the given HTTP method.
func (s *Server) Calls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *Server) txtLocked(zone, name string) []string {
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

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
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

// StartDNS serves TXT queries for the fake zones over UDP on 127.0.0.1 and
// returns the listen address and a shutdown function.
func (s *Server) StartDNS() (string, func() error, error) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(s.serveDNS)}
	go func() { _ = srv.ActivateAndServe() }()
	return pc.LocalAddr().String(), srv.Shutdown, nil
}

func (s *Server) serveDNS(w dns.ResponseWriter, req *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true

	s.mu.Lock()
	for _, q := range req.Question {
		if q.Qtype != dns.TypeTXT {
			continue
		}
		fqdn := strings.ToLower(strings.TrimSuffix(q.Name, "."))
		for zone := range s.zones {
			name, ok := relative(fqdn, zone)
			if !ok {
				continue
			}
			for _, v := range s.txtLocked(zone, name) {
				msg.Answer = append(msg.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
					Txt: []string{v},
				})
			}
		}
	}
	s.mu.Unlock()

	_ = w.WriteMsg(msg)
}

func relative(fqdn, zone string) (string, bool) {
	if fqdn == zone {
		return "@", true
	}
	name, ok := strings.CutSuffix(fqdn, "."+zone)
	return name, ok
}
