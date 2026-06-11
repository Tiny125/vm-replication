package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// loadOrCreateConsoleCert returns paths to a TLS cert/key for the console,
// generating a self-signed pair (valid for host + localhost) on first run.
func loadOrCreateConsoleCert(dir, host string) (certFile, keyFile string, err error) {
	certFile = dir + "/console.crt"
	keyFile = dir + "/console.key"
	if fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, nil
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	// SANs: the public host (IP or DNS) plus localhost for on-box access.
	tmpl.DNSNames = []string{"localhost"}
	tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", "", err
	}
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	if err := writePEM(keyFile, "PRIVATE KEY", keyDER, 0o600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// publicKeyPin returns the base64 SHA-256 of the certificate's SubjectPublicKeyInfo,
// i.e. the value for `curl --pinnedpubkey sha256//<pin>`.
func publicKeyPin(certFile string) (string, error) {
	cert, err := parseCert(certFile)
	if err != nil {
		return "", err
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// certFingerprint returns the colon-separated hex SHA-256 of the whole
// certificate (the value browsers show in their certificate dialog).
func certFingerprint(certFile string) (string, error) {
	cert, err := parseCert(certFile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = hex.EncodeToString([]byte{b})
	}
	return strings.ToUpper(strings.Join(parts, ":")), nil
}

func parseCert(certFile string) (*x509.Certificate, error) {
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", certFile)
	}
	return x509.ParseCertificate(block.Bytes)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: typ, Bytes: der})
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
