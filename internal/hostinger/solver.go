// Package hostinger implements a cert-manager ACME DNS01 webhook solver
// backed by the Hostinger DNS API.
package hostinger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/cert-manager/cert-manager/pkg/issuer/acme/dns/util"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	recordTypeTXT     = "TXT"
	defaultTTL        = 60
	defaultSecretKey  = "api-token"
	operationTimeout  = 2 * time.Minute
	defaultSolverName = "hostinger"
)

// Config is the solver configuration decoded from the Issuer's
// `webhook.config` stanza.
type Config struct {
	// APITokenSecretRef references the Secret holding the Hostinger API token.
	// The Secret is read from the Issuer's namespace, or from cert-manager's
	// cluster resource namespace for a ClusterIssuer.
	APITokenSecretRef cmmeta.SecretKeySelector `json:"apiTokenSecretRef"`
	// ServerURL overrides the Hostinger API endpoint. Defaults to DefaultBaseURL.
	ServerURL string `json:"serverURL,omitempty"`
	// TTL of the challenge TXT record in seconds. Defaults to 60.
	TTL *int `json:"ttl,omitempty"`
	// Zone overrides the Hostinger domain to edit. By default the zone that
	// cert-manager resolved via SOA lookups is used; set this when that zone
	// is not the domain name Hostinger knows it as (e.g. a delegated subzone).
	// The challenge FQDN must be inside it.
	Zone string `json:"zone,omitempty"`
}

// Solver implements webhook.Solver for Hostinger.
type Solver struct {
	kube      kubernetes.Interface
	userAgent string

	// locks serialises read-modify-write cycles on the same record set so
	// concurrent challenges for one name (e.g. example.com and *.example.com)
	// don't clobber each other.
	locks sync.Map
}

var _ webhook.Solver = (*Solver)(nil)

// New returns a Solver. The Kubernetes client is created in Initialize.
func New(userAgent string) *Solver {
	return &Solver{userAgent: userAgent}
}

// NewWithClient returns a Solver using the given Kubernetes client; used by tests.
func NewWithClient(kube kubernetes.Interface, userAgent string) *Solver {
	return &Solver{kube: kube, userAgent: userAgent}
}

// Name is the solverName referenced from the Issuer.
func (s *Solver) Name() string {
	return defaultSolverName
}

// Initialize builds the Kubernetes client used to read API token Secrets.
func (s *Solver) Initialize(kubeClientConfig *rest.Config, _ <-chan struct{}) error {
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return err
	}
	s.kube = cl
	return nil
}

// Present adds the challenge key to the TXT record set at ResolvedFQDN,
// keeping any other values already present. It is idempotent.
func (s *Solver) Present(ch *v1alpha1.ChallengeRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()

	api, cfg, err := s.clientFor(ctx, ch)
	if err != nil {
		return err
	}
	zone, name, err := zoneAndName(ch, cfg)
	if err != nil {
		return err
	}

	unlock := s.lock(zone, name)
	defer unlock()

	existing, err := findTXT(ctx, api, zone, name)
	if err != nil {
		return err
	}
	if existing != nil && hasValue(existing, ch.Key) {
		return nil
	}

	ttl := defaultTTL
	if cfg.TTL != nil {
		ttl = *cfg.TTL
	}
	// overwrite=false appends to an existing TXT set instead of replacing it,
	// which keeps concurrent challenges on the same name intact.
	err = api.UpdateRecords(ctx, zone, false, []RecordSet{{
		Name:    name,
		Type:    recordTypeTXT,
		TTL:     ttl,
		Records: []Record{{Content: ch.Key}},
	}})
	if err != nil {
		return fmt.Errorf("adding TXT record %q to zone %q: %w", name, zone, err)
	}
	return nil
}

// CleanUp removes only the value matching the challenge key from the TXT
// record set at ResolvedFQDN. Other values on the same name are retained.
func (s *Solver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()

	api, cfg, err := s.clientFor(ctx, ch)
	if err != nil {
		return err
	}
	zone, name, err := zoneAndName(ch, cfg)
	if err != nil {
		return err
	}

	unlock := s.lock(zone, name)
	defer unlock()

	existing, err := findTXT(ctx, api, zone, name)
	if err != nil {
		return err
	}
	if existing == nil || !hasValue(existing, ch.Key) {
		return nil
	}

	var remaining []Record
	for _, r := range existing.Records {
		if unquote(r.Content) != ch.Key {
			remaining = append(remaining, Record{Content: unquote(r.Content)})
		}
	}

	if len(remaining) == 0 {
		err = api.DeleteRecords(ctx, zone, []DeleteFilter{{Name: name, Type: recordTypeTXT}})
	} else {
		// The delete endpoint only filters by name and type, so to drop a
		// single value we replace the set with the values that must stay.
		err = api.UpdateRecords(ctx, zone, true, []RecordSet{{
			Name:    name,
			Type:    recordTypeTXT,
			TTL:     existing.TTL,
			Records: remaining,
		}})
	}
	if err != nil {
		return fmt.Errorf("removing TXT record %q from zone %q: %w", name, zone, err)
	}
	return nil
}

func (s *Solver) lock(zone, name string) func() {
	v, _ := s.locks.LoadOrStore(zone+"/"+name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Solver) clientFor(ctx context.Context, ch *v1alpha1.ChallengeRequest) (*Client, Config, error) {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return nil, cfg, err
	}
	token, err := s.secretValue(ctx, cfg.APITokenSecretRef, ch.ResourceNamespace)
	if err != nil {
		return nil, cfg, err
	}
	return NewClient(cfg.ServerURL, token, s.userAgent), cfg, nil
}

func (s *Solver) secretValue(ctx context.Context, ref cmmeta.SecretKeySelector, namespace string) (string, error) {
	if s.kube == nil {
		return "", errors.New("solver not initialized")
	}
	if ref.Name == "" {
		return "", errors.New("apiTokenSecretRef.name must be set in the solver config")
	}
	key := ref.Key
	if key == "" {
		key = defaultSecretKey
	}
	secret, err := s.kube.CoreV1().Secrets(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting secret %s/%s: %w", namespace, ref.Name, err)
	}
	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s/%s", key, namespace, ref.Name)
	}
	token := strings.TrimSpace(string(value))
	if token == "" {
		return "", fmt.Errorf("key %q in secret %s/%s is empty", key, namespace, ref.Name)
	}
	return token, nil
}

func loadConfig(cfgJSON *extapi.JSON) (Config, error) {
	cfg := Config{}
	if cfgJSON == nil {
		return cfg, errors.New("solver config is missing; apiTokenSecretRef is required")
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("decoding solver config: %w", err)
	}
	if cfg.TTL != nil && *cfg.TTL <= 0 {
		return cfg, fmt.Errorf("ttl must be positive, got %d", *cfg.TTL)
	}
	return cfg, nil
}

// findTXT returns the TXT record set called name in zone, or nil.
func findTXT(ctx context.Context, api *Client, zone, name string) (*RecordSet, error) {
	sets, err := api.GetRecords(ctx, zone)
	if err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("zone %q not found in the Hostinger account of this API token: %w", zone, err)
		}
		return nil, fmt.Errorf("listing records of zone %q: %w", zone, err)
	}
	for i := range sets {
		if sets[i].Type == recordTypeTXT && strings.EqualFold(sets[i].Name, name) {
			return &sets[i], nil
		}
	}
	return nil, nil
}

// zoneAndName splits the challenge FQDN into the Hostinger zone (domain) and
// the record name relative to it, using "@" for the zone apex.
func zoneAndName(ch *v1alpha1.ChallengeRequest, cfg Config) (zone, name string, err error) {
	zone = ch.ResolvedZone
	if cfg.Zone != "" {
		zone = cfg.Zone
	}
	zone = strings.ToLower(util.UnFqdn(zone))
	fqdn := strings.ToLower(util.UnFqdn(ch.ResolvedFQDN))
	if fqdn == zone {
		return zone, "@", nil
	}
	name, ok := strings.CutSuffix(fqdn, "."+zone)
	if !ok {
		return "", "", fmt.Errorf("challenge FQDN %q is not inside zone %q", ch.ResolvedFQDN, zone)
	}
	return zone, name, nil
}

func hasValue(set *RecordSet, value string) bool {
	for _, r := range set.Records {
		if unquote(r.Content) == value {
			return true
		}
	}
	return false
}

// unquote strips the surrounding quotes Hostinger may add to TXT contents.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
