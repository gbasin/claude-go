// Package netproxy implements the sandbox egress proxy and its local CA —
// the Go counterpart of Claude Code's sandbox-runtime (SRT) HTTP/S proxy
// (extracted: mod_0215.js env wiring + mod_0222.js policy engine).
package netproxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gbasin/claude-go/internal/config"
)

const (
	caCertFile = "cert.pem"
	caKeyFile  = "key.pem"
)

// CADir is where the local CA persists: <claude dir>/sandbox-ca
// (CLAUDE_CONFIG_DIR overrides ~/.claude via config.ClaudeDir).
func CADir() string { return filepath.Join(config.ClaudeDir(), "sandbox-ca") }

// CA is the local MITM root: an ECDSA P-256 root persisted as PEM plus an
// in-memory cache of per-host leaf certificates it mints on demand.
type CA struct {
	Cert     *x509.Certificate
	Key      *ecdsa.PrivateKey
	CertPEM  []byte
	CertPath string
	KeyPath  string
	// ExtraPaths are extra CA bundle files appended to the bundle the
	// sandboxed command sees (network.extraCaCertPaths).
	ExtraPaths []string

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// LoadOrCreateCA reads cert.pem/key.pem from dir, generating a fresh ECDSA
// P-256 root (10y validity) when either is missing or unreadable.
// extraCertPaths are appended to the CA bundle emitted to sandboxed
// commands (real's network.extraCaCertPaths — corporate/proxy roots the
// generated CA can't supply).
func LoadOrCreateCA(dir string, extraCertPaths ...string) (*CA, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)
	if ca, err := loadCA(certPath, keyPath); err == nil {
		ca.ExtraPaths = extraCertPaths
		return ca, nil
	}
	ca, err := createCA(dir, certPath, keyPath)
	if ca != nil {
		ca.ExtraPaths = extraCertPaths
	}
	return ca, err
}

func loadCA(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not ECDSA")
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM,
		CertPath: certPath, KeyPath: keyPath,
		cache: map[string]*tls.Certificate{}}, nil
}

// BundlePath returns the PEM path a sandboxed command should trust: the
// generated CA cert alone, or a combined bundle when ExtraPaths are set
// (written next to cert.pem as bundle.pem).
func (ca *CA) BundlePath() string {
	if len(ca.ExtraPaths) == 0 {
		return ca.CertPath
	}
	var out bytes.Buffer
	out.Write(ca.CertPEM)
	for _, p := range ca.ExtraPaths {
		if b, err := os.ReadFile(p); err == nil {
			out.WriteByte(0x0a)
			out.Write(b)
		}
	}
	bp := filepath.Join(filepath.Dir(ca.CertPath), "bundle.pem")
	if err := os.WriteFile(bp, out.Bytes(), 0o600); err != nil {
		return ca.CertPath
	}
	return bp
}

func createCA(dir, certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "claude-go Sandbox CA", Organization: []string{"claude-go"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM,
		CertPath: certPath, KeyPath: keyPath,
		cache: map[string]*tls.Certificate{}}, nil
}

// normalizeMintKey canonicalizes a hostname for cert minting: lowercase, no
// brackets (IPv6), no trailing root dot.
func normalizeMintKey(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(host, ".")
}

// Mint returns (from cache, or freshly signed) a leaf certificate for host —
// DNS SAN for names, IP SAN for literals. Leaf validity: 24h, capped at the
// CA's own NotAfter.
func (c *CA) Mint(host string) (*tls.Certificate, error) {
	host = normalizeMintKey(host)
	if host == "" {
		host = "localhost"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.cache[host]; ok {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	cn := host
	if len(cn) > 64 { // CN is capped at 64 bytes; SANs carry the real identity
		cn = cn[:64]
	}
	notAfter := time.Now().Add(24 * time.Hour)
	if notAfter.After(c.Cert.NotAfter) {
		notAfter = c.Cert.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"claude-go sandbox"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else {
		tpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf.Leaf, _ = x509.ParseCertificate(der)
	c.cache[host] = &leaf
	return &leaf, nil
}

// Pool returns a CertPool trusting this CA alone.
func (c *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.CertPEM)
	return pool
}

// InstallCA installs the persisted CA into the OS trust store. It is only run
// on explicit user request (`claude-go sandbox trust`): macOS adds the cert
// to the login keychain via `security add-trusted-cert`; Linux tries p11-kit
// `trust anchor` then falls back to instructions. Returns a human-readable
// summary of what happened.
func InstallCA() (string, error) {
	certPath := filepath.Join(CADir(), caCertFile)
	if _, err := os.Stat(certPath); err != nil {
		return "", fmt.Errorf("no CA at %s yet", certPath)
	}
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		keychain := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
		fmt.Fprintln(os.Stderr, "macOS may show an authorization prompt to modify the login keychain…")
		out, err := exec.Command("security", "add-trusted-cert",
			"-r", "trustRoot", "-k", keychain, certPath).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("security add-trusted-cert failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return fmt.Sprintf("Trusted %s in the login keychain.", certPath), nil
	case "linux":
		if p, err := exec.LookPath("trust"); err == nil {
			if out, err := exec.Command(p, "anchor", "--store", certPath).CombinedOutput(); err == nil {
				return fmt.Sprintf("Anchored %s via p11-kit trust.", certPath), nil
			} else {
				return "", fmt.Errorf("trust anchor failed: %v: %s\nmanual: cp %s /usr/local/share/ca-certificates/claude-go-sandbox.crt && sudo update-ca-certificates",
					err, strings.TrimSpace(string(out)), certPath)
			}
		}
		return "", fmt.Errorf("no `trust` tool; install %s manually, e.g.\n  sudo cp %s /usr/local/share/ca-certificates/claude-go-sandbox.crt && sudo update-ca-certificates",
			certPath, certPath)
	default:
		return "", fmt.Errorf("automatic CA trust is not supported on %s; trust %s manually", runtime.GOOS, certPath)
	}
}
