// Package hostca is the private certificate authority that the host uses to
// authenticate a gateway. The host is the only signer. A gateway holds one
// client certificate, and the host refuses a connection that carries none.
package hostca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dir is the CA directory inside the Kiln root.
const Dir = "ca"

const (
	caCertFile  = "ca.crt"
	caKeyFile   = "ca.key"
	revokedFile = "revoked"
)

// CA reads and writes the files of one certificate authority.
type CA struct{ dir string }

// New names the CA directory inside a Kiln root.
func New(root string) *CA { return &CA{dir: filepath.Join(root, Dir)} }

// Ensure creates the CA when the root has none. It is idempotent, so a second
// kiln init keeps the certificates a gateway already holds.
func (c *CA) Ensure() error {
	if _, err := os.Stat(filepath.Join(c.dir, caCertFile)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kiln host CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(c.dir, caCertFile), pemBlock("CERTIFICATE", der), 0o644); err != nil {
		return err
	}
	return writeFile(filepath.Join(c.dir, caKeyFile), pemBlock("EC PRIVATE KEY", keyDER), 0o600)
}

// Issue signs one client certificate and returns the CA certificate, the
// client certificate, the client key and the serial. The key exists once, in
// this result.
func (c *CA) Issue(name string, days int) (caPEM, certPEM, keyPEM, serial string, err error) {
	return c.issue(name, days, x509.ExtKeyUsageClientAuth, nil)
}

// issue signs one certificate for the given use. hosts names the addresses a
// server certificate answers for, and it is nil for a client certificate.
func (c *CA) issue(name string, days int, use x509.ExtKeyUsage, hosts []string) (caPEM, certPEM, keyPEM, serial string, err error) {
	caCert, caKey, err := c.load()
	if err != nil {
		return "", "", "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", "", err
	}
	number, err := newSerial()
	if err != nil {
		return "", "", "", "", err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: number,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, days),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{use},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return "", "", "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", "", "", err
	}
	caBytes, err := os.ReadFile(filepath.Join(c.dir, caCertFile))
	if err != nil {
		return "", "", "", "", err
	}
	return string(caBytes), pemBlock("CERTIFICATE", der), pemBlock("EC PRIVATE KEY", keyDER), number.String(), nil
}

// Revoke adds one serial to the revoked list. A revoked certificate cannot
// connect, and the CA keeps signing for every other gateway.
func (c *CA) Revoke(serial string) error {
	if _, ok := new(big.Int).SetString(serial, 10); !ok {
		return fmt.Errorf("hostca: %q is not a serial", serial)
	}
	list, err := c.Revoked()
	if err != nil {
		return err
	}
	if list[serial] {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(c.dir, revokedFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, serial)
	return err
}

// Revoked returns the revoked serials.
func (c *CA) Revoked() (map[string]bool, error) {
	out := map[string]bool{}
	body, err := os.ReadFile(filepath.Join(c.dir, revokedFile))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out, nil
}

// ServerTLS returns the TLS config of the host's gateway listener. The
// certificate names every address a gateway dials, so the gateway verifies
// the host. The listener demands a client certificate from the same CA and
// refuses a revoked serial.
func (c *CA) ServerTLS(hostnames ...string) (*tls.Config, error) {
	if len(hostnames) == 0 {
		return nil, errors.New("hostca: the listener certificate needs at least one hostname or address")
	}
	caPEM, certPEM, keyPEM, _, err := c.issue("kiln host", 825, x509.ExtKeyUsageServerAuth, hostnames)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("hostca: the CA certificate does not parse")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
			revoked, err := c.Revoked()
			if err != nil {
				return err
			}
			for _, chain := range chains {
				if len(chain) > 0 && revoked[chain[0].SerialNumber.String()] {
					return fmt.Errorf("hostca: certificate %s is revoked", chain[0].SerialNumber)
				}
			}
			return nil
		},
	}, nil
}

func (c *CA) load() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(c.dir, caCertFile))
	if err != nil {
		return nil, nil, fmt.Errorf("hostca: %w; run kiln init", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(c.dir, caKeyFile))
	if err != nil {
		return nil, nil, fmt.Errorf("hostca: %w; run kiln init", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, errors.New("hostca: the CA certificate does not parse")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	block, _ = pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, errors.New("hostca: the CA key does not parse")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func pemBlock(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

func writeFile(path, body string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(body), mode)
}
