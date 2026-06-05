// Package transport builds the mutually-authenticated TLS configs used by the
// agent and receiver. Both ends present a certificate signed by the same
// internal CA and refuse anything else, which gives us cert-pinned mTLS without
// depending on the public PKI.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Files bundles the on-disk PEM material an endpoint needs.
type Files struct {
	CertFile string // this endpoint's certificate
	KeyFile  string // this endpoint's private key
	CAFile   string // CA that signed the peer's certificate
}

func loadCA(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("transport: read CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("transport: no certificates parsed from %s", caFile)
	}
	return pool, nil
}

// ServerConfig builds a TLS config for the receiver: present our cert, demand
// and verify a client cert signed by our CA.
func ServerConfig(f Files) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("transport: load server keypair: %w", err)
	}
	pool, err := loadCA(f.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig builds a TLS config for the agent: present our cert, verify the
// receiver's cert against our CA. serverName must match the receiver cert's SAN.
func ClientConfig(f Files, serverName string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("transport: load client keypair: %w", err)
	}
	pool, err := loadCA(f.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
