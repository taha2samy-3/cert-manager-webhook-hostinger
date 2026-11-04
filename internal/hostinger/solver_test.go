package hostinger_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/taha2samy-3/cert-manager-webhook-hostinger/internal/hostinger"
	"github.com/taha2samy-3/cert-manager-webhook-hostinger/internal/hostinger/hostingertest"
)

const (
	testToken = "test-token"
	testNS    = "cert-manager"
)

func newSolver(t *testing.T) (*hostinger.Solver, *hostingertest.Server) {
	t.Helper()
	api := hostingertest.NewServer(testToken, "example.com")
	t.Cleanup(api.Close)
	kube := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hostinger", Namespace: testNS},
		Data:       map[string][]byte{"api-token": []byte(testToken + "\n")},
	})
	return hostinger.NewWithClient(kube, "test"), api
}

func challenge(t *testing.T, api *hostingertest.Server, fqdn, key string, extra map[string]any) *v1alpha1.ChallengeRequest {
	t.Helper()
	cfg := map[string]any{
		"apiTokenSecretRef": map[string]string{"name": "hostinger"},
		"serverURL":         api.URL,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &v1alpha1.ChallengeRequest{
		ResolvedZone:      "example.com.",
		ResolvedFQDN:      fqdn,
		Key:               key,
		ResourceNamespace: testNS,
		Config:            &extapi.JSON{Raw: raw},
	}
}

func TestPresentIsIdempotent(t *testing.T) {
	s, api := newSolver(t)
	ch := challenge(t, api, "_acme-challenge.example.com.", "k1", nil)

	for range 3 {
		if err := s.Present(ch); err != nil {
			t.Fatalf("Present: %v", err)
		}
	}
	if got := api.TXT("example.com", "_acme-challenge"); !slices.Equal(got, []string{"k1"}) {
		t.Fatalf("TXT = %v, want [k1]", got)
	}
	if n := api.Calls("PUT"); n != 1 {
		t.Fatalf("PUT calls = %d, want 1", n)
	}
}

// Wildcard and apex certificates share _acme-challenge.<domain>; both
// values must coexist and cleaning one must keep the other.
func TestSameNameChallengesCoexist(t *testing.T) {
	s, api := newSolver(t)
	ch1 := challenge(t, api, "_acme-challenge.example.com.", "apex", nil)
	ch2 := challenge(t, api, "_acme-challenge.example.com.", "wildcard", nil)

	for _, ch := range []*v1alpha1.ChallengeRequest{ch1, ch2} {
		if err := s.Present(ch); err != nil {
			t.Fatalf("Present: %v", err)
		}
	}
	if got := api.TXT("example.com", "_acme-challenge"); !slices.Equal(got, []string{"apex", "wildcard"}) {
		t.Fatalf("TXT after Present = %v", got)
	}

	if err := s.CleanUp(ch1); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge"); !slices.Equal(got, []string{"wildcard"}) {
		t.Fatalf("TXT after first CleanUp = %v, want [wildcard]", got)
	}

	if err := s.CleanUp(ch2); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge"); len(got) != 0 {
		t.Fatalf("TXT after second CleanUp = %v, want none", got)
	}
	if n := api.Calls("DELETE"); n != 1 {
		t.Fatalf("DELETE calls = %d, want 1", n)
	}
}

func TestConcurrentPresentAndCleanUp(t *testing.T) {
	s, api := newSolver(t)
	var chs []*v1alpha1.ChallengeRequest
	for i := range 8 {
		chs = append(chs, challenge(t, api, "_acme-challenge.example.com.", fmt.Sprintf("k%d", i), nil))
	}

	var wg sync.WaitGroup
	for _, ch := range chs {
		wg.Go(func() {
			if err := s.Present(ch); err != nil {
				t.Errorf("Present: %v", err)
			}
		})
	}
	wg.Wait()
	if got := api.TXT("example.com", "_acme-challenge"); len(got) != len(chs) {
		t.Fatalf("TXT = %v, want %d values", got, len(chs))
	}

	for _, ch := range chs[1:] {
		wg.Go(func() {
			if err := s.CleanUp(ch); err != nil {
				t.Errorf("CleanUp: %v", err)
			}
		})
	}
	wg.Wait()
	if got := api.TXT("example.com", "_acme-challenge"); !slices.Equal(got, []string{"k0"}) {
		t.Fatalf("TXT = %v, want [k0]", got)
	}
}

func TestCleanUpMissingRecordIsNoop(t *testing.T) {
	s, api := newSolver(t)
	if err := s.CleanUp(challenge(t, api, "_acme-challenge.example.com.", "gone", nil)); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if n := api.Calls("PUT") + api.Calls("DELETE"); n != 0 {
		t.Fatalf("unexpected write calls: %d", n)
	}
}

// The real API once answered 200 to an append that it did not save.
func TestPresentWritesAgainWhenTheValueWasNotSaved(t *testing.T) {
	t.Cleanup(hostinger.SetWriteRetryDelay(time.Millisecond))
	s, api := newSolver(t)
	api.LoseWrites(1)
	if err := s.Present(challenge(t, api, "_acme-challenge.example.com.", "k1", nil)); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge"); !slices.Equal(got, []string{"k1"}) {
		t.Fatalf("TXT = %v, want [k1]", got)
	}
	if n := api.Calls("PUT"); n != 2 {
		t.Fatalf("PUT calls = %d, want 2", n)
	}
}

func TestPresentGivesUpWhenWritesKeepGettingLost(t *testing.T) {
	t.Cleanup(hostinger.SetWriteRetryDelay(time.Millisecond))
	s, api := newSolver(t)
	api.LoseWrites(10)
	err := s.Present(challenge(t, api, "_acme-challenge.example.com.", "k1", nil))
	if err == nil || !strings.Contains(err.Error(), "accepted the value 3 times, but did not save it") {
		t.Fatalf("Present error = %v", err)
	}
	if n := api.Calls("PUT"); n != 3 {
		t.Fatalf("PUT calls = %d, want 3", n)
	}
}

func TestCleanUpWritesAgainWhenTheChangeWasNotSaved(t *testing.T) {
	t.Cleanup(hostinger.SetWriteRetryDelay(time.Millisecond))
	s, api := newSolver(t)
	ch1 := challenge(t, api, "_acme-challenge.example.com.", "k1", nil)
	ch2 := challenge(t, api, "_acme-challenge.example.com.", "k2", nil)
	for _, ch := range []*v1alpha1.ChallengeRequest{ch1, ch2} {
		if err := s.Present(ch); err != nil {
			t.Fatalf("Present: %v", err)
		}
	}

	api.LoseWrites(1)
	if err := s.CleanUp(ch1); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge"); !slices.Equal(got, []string{"k2"}) {
		t.Fatalf("TXT = %v, want [k2]", got)
	}

	api.LoseWrites(1)
	if err := s.CleanUp(ch2); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge"); len(got) != 0 {
		t.Fatalf("TXT = %v, want none", got)
	}
	if n := api.Calls("DELETE"); n != 2 {
		t.Fatalf("DELETE calls = %d, want 2", n)
	}
}

// Record sets made in hPanel can have a TTL the API refuses on writes.
func TestCleanUpRaisesATTLBelowTheAPIMinimum(t *testing.T) {
	s, api := newSolver(t)
	api.Seed("example.com", hostingertest.RecordSet{Name: "_acme-challenge", Type: "TXT", TTL: 50, Records: []hostingertest.Record{
		{Content: `"other"`}, {Content: `"k1"`},
	}})
	if err := s.CleanUp(challenge(t, api, "_acme-challenge.example.com.", "k1", nil)); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	sets := api.RecordSets("example.com")
	if len(sets) != 1 || sets[0].TTL != 60 || len(sets[0].Records) != 1 || sets[0].Records[0].Content != `"other"` {
		t.Fatalf("record sets = %+v", sets)
	}
}

// The API cannot write is_disabled, so rewriting a set would turn values
// that were turned off in hPanel back on.
func TestCleanUpDoesNotTurnOnDisabledValues(t *testing.T) {
	s, api := newSolver(t)
	seeded := hostingertest.RecordSet{Name: "_acme-challenge", Type: "TXT", TTL: 60, Records: []hostingertest.Record{
		{Content: `"off"`, IsDisabled: true}, {Content: `"k1"`},
	}}
	api.Seed("example.com", seeded)
	err := s.CleanUp(challenge(t, api, "_acme-challenge.example.com.", "k1", nil))
	if err == nil || !strings.Contains(err.Error(), "turned off in hPanel") {
		t.Fatalf("CleanUp error = %v", err)
	}
	if sets := api.RecordSets("example.com"); len(sets) != 1 || !reflect.DeepEqual(sets[0], seeded) {
		t.Fatalf("record sets = %+v, want them unchanged", sets)
	}
}

func TestDisabledChallengeValue(t *testing.T) {
	s, api := newSolver(t)
	api.Seed("example.com", hostingertest.RecordSet{Name: "_acme-challenge", Type: "TXT", TTL: 60, Records: []hostingertest.Record{
		{Content: `"k1"`, IsDisabled: true},
	}})
	ch := challenge(t, api, "_acme-challenge.example.com.", "k1", nil)
	if err := s.Present(ch); err == nil || !strings.Contains(err.Error(), "turned off in hPanel") {
		t.Fatalf("Present error = %v", err)
	}
	// Our own value may go, even when it is turned off.
	if err := s.CleanUp(ch); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if sets := api.RecordSets("example.com"); len(sets) != 0 {
		t.Fatalf("record sets = %+v, want none", sets)
	}
}

func TestRecordNames(t *testing.T) {
	tests := []struct{ fqdn, want string }{
		{"_acme-challenge.example.com.", "_acme-challenge"},
		{"_acme-challenge.sub.example.com.", "_acme-challenge.sub"},
		{"_ACME-Challenge.Example.COM.", "_acme-challenge"},
		{"example.com.", "@"},
	}
	for _, tt := range tests {
		t.Run(tt.fqdn, func(t *testing.T) {
			s, api := newSolver(t)
			if err := s.Present(challenge(t, api, tt.fqdn, "v", nil)); err != nil {
				t.Fatalf("Present: %v", err)
			}
			if got := api.TXT("example.com", tt.want); !slices.Equal(got, []string{"v"}) {
				t.Fatalf("TXT(%q) = %v", tt.want, got)
			}
		})
	}
}

func TestZoneOverride(t *testing.T) {
	s, api := newSolver(t)
	// cert-manager resolved a delegated subzone, but Hostinger manages the
	// parent domain.
	ch := challenge(t, api, "_acme-challenge.app.example.com.", "v", map[string]any{"zone": "example.com"})
	ch.ResolvedZone = "app.example.com."
	if err := s.Present(ch); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge.app"); !slices.Equal(got, []string{"v"}) {
		t.Fatalf("TXT = %v", got)
	}
	if err := s.CleanUp(ch); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if got := api.TXT("example.com", "_acme-challenge.app"); len(got) != 0 {
		t.Fatalf("TXT after CleanUp = %v", got)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*v1alpha1.ChallengeRequest)
		extra   map[string]any
		wantErr string
	}{
		{
			name: "unknown zone",
			mutate: func(ch *v1alpha1.ChallengeRequest) {
				ch.ResolvedZone = "other.org."
				ch.ResolvedFQDN = "_acme-challenge.other.org."
			},
			wantErr: `zone "other.org" not found`,
		},
		{
			name:    "missing secret",
			extra:   map[string]any{"apiTokenSecretRef": map[string]string{"name": "nope"}},
			wantErr: "getting secret cert-manager/nope",
		},
		{
			name:    "missing key",
			extra:   map[string]any{"apiTokenSecretRef": map[string]string{"name": "hostinger", "key": "nope"}},
			wantErr: `key "nope" not found`,
		},
		{
			name:    "wrong namespace",
			mutate:  func(ch *v1alpha1.ChallengeRequest) { ch.ResourceNamespace = "default" },
			wantErr: "getting secret default/hostinger",
		},
		{
			name:    "ttl below the API minimum",
			extra:   map[string]any{"ttl": 59},
			wantErr: "ttl must be between 60 and 86400 seconds",
		},
		{
			name:    "ttl above the API maximum",
			extra:   map[string]any{"ttl": 86401},
			wantErr: "ttl must be between 60 and 86400 seconds",
		},
		{
			name:    "zone override outside fqdn",
			extra:   map[string]any{"zone": "other.org"},
			wantErr: `is not inside zone "other.org"`,
		},
		{
			name:    "missing config",
			mutate:  func(ch *v1alpha1.ChallengeRequest) { ch.Config = nil },
			wantErr: "solver config is missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, api := newSolver(t)
			ch := challenge(t, api, "_acme-challenge.example.com.", "k", tt.extra)
			if tt.mutate != nil {
				tt.mutate(ch)
			}
			err := s.Present(ch)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Present error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
