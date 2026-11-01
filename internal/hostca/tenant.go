package hostca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

const (
	// tenantCAYears is how long a template that trusts a tenant CA keeps working.
	tenantCAYears = 20
	// leafDays bounds one leaf. The daemon issues a new one at each wake and each
	// start, so only a sandbox that runs this long without either meets it.
	leafDays = 825
)

// TenantCA returns the CA of one tenant, and makes it on first use. Its key stays
// sealed in the database, and two tenants never share a CA.
func TenantCA(ctx context.Context, st store.Store, sealKey []byte, tenant string) (store.TenantCA, error) {
	ca, err := st.TenantCA(ctx, tenant)
	if !errors.Is(err, store.ErrNotFound) {
		return ca, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return store.TenantCA{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return store.TenantCA{}, err
	}
	now := time.Now().Add(-time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Kiln credential CA " + tenant},
		NotBefore:             now,
		NotAfter:              now.AddDate(tenantCAYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Kiln credential CA " + tenant},
	}, &key.PublicKey, key)
	if err != nil {
		return store.TenantCA{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return store.TenantCA{}, err
	}
	sealed, err := store.Seal(sealKey, pemBlock("EC PRIVATE KEY", keyDER))
	if err != nil {
		return store.TenantCA{}, err
	}
	return st.PutTenantCA(ctx, tenant, store.TenantCA{CertPEM: pemBlock("CERTIFICATE", der), KeySealed: sealed})
}

// IssueLeaf makes the certificate of one bound host. Its validity starts with the
// CA, because a guest clock can lag behind the host right after a restore.
func IssueLeaf(sealKey []byte, ca store.TenantCA, host string) (certPEM, keyPEM string, err error) {
	block, _ := pem.Decode([]byte(ca.CertPEM))
	if block == nil {
		return "", "", errors.New("hostca: tenant CA certificate is not PEM")
	}
	parent, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("hostca: tenant CA: %w", err)
	}
	opened, err := store.Unseal(sealKey, ca.KeySealed)
	if err != nil {
		return "", "", err
	}
	block, _ = pem.Decode([]byte(opened))
	if block == nil {
		return "", "", errors.New("hostca: tenant CA key is not PEM")
	}
	parentKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("hostca: tenant CA key: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := newSerial()
	if err != nil {
		return "", "", err
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    parent.NotBefore,
		NotAfter:     time.Now().AddDate(0, 0, leafDays),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, parent, &key.PublicKey, parentKey)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return pemBlock("CERTIFICATE", der), pemBlock("EC PRIVATE KEY", keyDER), nil
}
