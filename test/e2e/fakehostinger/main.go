// Command fakehostinger runs the in-memory Hostinger API and its DNS server
// inside the e2e cluster, so issuance can be tested end to end with Pebble
// without a real Hostinger account.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/taha2samy-3/cert-manager-webhook-hostinger/internal/hostinger/hostingertest"
)

func main() {
	httpAddr := flag.String("http", ":8080", "API listen address")
	dnsAddr := flag.String("dns", ":5353", "DNS listen address (UDP and TCP)")
	token := flag.String("token", "e2e-token", "accepted API bearer token")
	zones := flag.String("zones", "example.com", "comma-separated zones to serve")
	flag.Parse()

	fake := hostingertest.New(*token, strings.Split(*zones, ",")...)
	for _, network := range []string{"udp", "tcp"} {
		srv := &dns.Server{Addr: *dnsAddr, Net: network, Handler: fake}
		go func() { log.Fatal(srv.ListenAndServe()) }()
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", fake)
	// Unauthenticated read-only view of a zone, reachable through the API
	// server's service proxy (which strips Authorization headers).
	mux.HandleFunc("GET /debug/zones/{zone}", func(w http.ResponseWriter, r *http.Request) {
		req := r.Clone(r.Context())
		req.URL.Path = "/api/dns/v1/zones/" + r.PathValue("zone")
		req.Header.Set("Authorization", "Bearer "+*token)
		fake.ServeHTTP(w, req)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Printf("fake Hostinger API on %s, DNS on %s, zones %s", *httpAddr, *dnsAddr, *zones)
	srv := &http.Server{Addr: *httpAddr, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
