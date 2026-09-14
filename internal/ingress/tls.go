package ingress

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// CloudflareTokenKey is the config key that holds the Cloudflare API token.
// v1 imports one DNS client, and this is it.
const CloudflareTokenKey = "CLOUDFLARE_DNS_API_TOKEN"

// CloudflareProvider is the name acme.dns_provider must carry.
const CloudflareProvider = "cloudflare"

// ACMEConfig is the certificate part of config.json.
type ACMEConfig struct {
	Email       string
	DNSProvider string
	Credentials map[string]string
}

// NewCertMagic builds the certificate manager for one zone. It obtains and
// renews the wildcard certificate in the background through ACME DNS-01. A
// wildcard cannot pass the HTTP or TLS-ALPN challenge, so DNS-01 is the only
// route.
func NewCertMagic(ctx context.Context, root, zone string, cfg ACMEConfig) (*certmagic.Config, error) {
	if zone == "" {
		return nil, fmt.Errorf("ingress: a preview zone is required")
	}
	if cfg.Email == "" {
		return nil, fmt.Errorf("ingress: acme.email is required")
	}
	if cfg.DNSProvider != "" && cfg.DNSProvider != CloudflareProvider {
		return nil, fmt.Errorf("ingress: acme.dns_provider %q is not built into this binary; v1 imports %s only",
			cfg.DNSProvider, CloudflareProvider)
	}
	token := cfg.Credentials[CloudflareTokenKey]
	if token == "" {
		return nil, fmt.Errorf("ingress: acme.credentials.%s is required", CloudflareTokenKey)
	}
	storage := &certmagic.FileStorage{Path: filepath.Join(root, "acme")}

	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	magic = certmagic.New(cache, certmagic.Config{Storage: storage})
	issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:                      certmagic.LetsEncryptProductionCA,
		Email:                   cfg.Email,
		DisableHTTPChallenge:    true,
		DisableTLSALPNChallenge: true,
		DNS01Solver: &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: &cloudflare.Provider{APIToken: token},
			},
		},
	})
	magic.Issuers = []certmagic.Issuer{issuer}

	wildcard := "*." + zone
	go func() {
		if err := magic.ManageAsync(ctx, []string{wildcard}); err != nil {
			log.Printf("ingress: certificate management for %s: %v", wildcard, err)
		}
	}()
	return magic, nil
}
