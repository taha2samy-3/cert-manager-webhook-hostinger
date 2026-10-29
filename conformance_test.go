//go:build conformance

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	acmetest "github.com/cert-manager/cert-manager/test/acme"

	"github.com/taha2samy-3/cert-manager-webhook-hostinger/internal/hostinger"
	"github.com/taha2samy-3/cert-manager-webhook-hostinger/internal/hostinger/hostingertest"
)

// TestConformance runs cert-manager's DNS01 webhook conformance suite.
//
// By default it runs against an in-memory fake of the Hostinger API and a
// local DNS server. Set TEST_ZONE_NAME (e.g. "example.com.") and
// HOSTINGER_API_TOKEN to run it against the real Hostinger API instead;
// TEST_DNS_SERVER defaults to Hostinger's ns1.dns-parking.com:53 in that mode.
func TestConformance(t *testing.T) {
	zone := os.Getenv("TEST_ZONE_NAME")
	token := os.Getenv("HOSTINGER_API_TOKEN")
	serverURL := os.Getenv("HOSTINGER_API_URL")
	dnsServer := os.Getenv("TEST_DNS_SERVER")
	propagation := 2 * time.Minute

	if zone == "" || token == "" {
		zone, token = "example.com.", "conformance-token"
		api := hostingertest.NewServer(token, "example.com")
		defer api.Close()
		addr, stop, err := api.StartDNS()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stop() }()
		serverURL, dnsServer, propagation = api.URL, addr, 30*time.Second
	} else if dnsServer == "" {
		dnsServer = "ns1.dns-parking.com:53"
	}

	manifests := t.TempDir()
	secret := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: hostinger-credentials
type: Opaque
stringData:
  api-token: %q
`, token)
	if err := os.WriteFile(filepath.Join(manifests, "secret.yaml"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	fixture := acmetest.NewFixture(hostinger.New("conformance-test"),
		acmetest.SetResolvedZone(zone),
		acmetest.SetAllowAmbientCredentials(false),
		acmetest.SetManifestPath(manifests),
		acmetest.SetDNSServer(dnsServer),
		acmetest.SetUseAuthoritative(false),
		acmetest.SetStrict(true),
		acmetest.SetPropagationLimit(propagation),
		acmetest.SetConfig(map[string]any{
			"apiTokenSecretRef": map[string]string{"name": "hostinger-credentials", "key": "api-token"},
			"serverURL":         serverURL,
		}),
	)
	// Strict mode enables Extended/DeletingOneRecordRetainsOthers, which is
	// what wildcard + apex certificates rely on.
	fixture.RunConformance(t)
}
