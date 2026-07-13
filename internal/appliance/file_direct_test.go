package appliance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/transport"
)

// The destination bootstrap must: mint a token, build a cloud-init script that
// downloads the receiver + certs and runs the receiver writing to / on the
// fixed port, and serve those downloads only for a valid token.
func TestDestBootstrap(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	s.cfg.PublicKeyPin = "PINPIN"

	tok := s.registerDestBootstrap(9, time.Hour)
	if _, ok := s.lookupDestBootstrap(tok); !ok {
		t.Fatal("freshly minted token should resolve")
	}
	if _, ok := s.lookupDestBootstrap("nope"); ok {
		t.Error("unknown token must not resolve")
	}

	ci := s.destCloudInit(tok)
	for _, want := range []string{
		"/dest/receiver?token=" + tok,
		"/dest/cert?token=" + tok,
		"-mode file", "-device /", ":5999",
		"vmrepl-receiver.service", "sha256//PINPIN",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("cloud-init missing %q", want)
		}
	}

	// Endpoints: bad token -> 403.
	rr := httptest.NewRecorder()
	s.handleDestReceiver(rr, httptest.NewRequest("GET", "/dest/receiver?token=nope", nil))
	if rr.Code != 403 {
		t.Errorf("receiver bad token: %d, want 403", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleDestCert(rr, httptest.NewRequest("GET", "/dest/cert?token=nope&name=ca.crt", nil))
	if rr.Code != 403 {
		t.Errorf("cert bad token: %d, want 403", rr.Code)
	}

	// Good token: cert served from the configured file.
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, []byte("CADATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.TLS.CAFile = ca
	rr = httptest.NewRecorder()
	s.handleDestCert(rr, httptest.NewRequest("GET", "/dest/cert?token="+tok+"&name=ca.crt", nil))
	if rr.Code != 200 || rr.Body.String() != "CADATA" {
		t.Errorf("cert good token: %d %q", rr.Code, rr.Body.String())
	}
	// Unknown cert name rejected.
	rr = httptest.NewRecorder()
	s.handleDestCert(rr, httptest.NewRequest("GET", "/dest/cert?token="+tok+"&name=secret", nil))
	if rr.Code != 400 {
		t.Errorf("unknown cert name: %d, want 400", rr.Code)
	}
}

// The destination watcher must NEVER give up: the console promises "the
// console keeps watching and unlocks Start replication as soon as the receiver
// answers", so a receiver brought up MANUALLY (Lish paste) long after the
// automatic-install grace must still flip the destination to ready. The old
// code stopped polling for good after 15 minutes — a later manual install was
// never detected and Start stayed locked forever.
func TestDestWatcherKeepsWatchingAfterWarn(t *testing.T) {
	s := automationServer(t)
	ctx := context.Background()
	m, _, err := s.st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "app", SourceHostname: "app", BootTarget: api.BootTargetFile,
		OSImage: "linode/ubuntu24.04", LinodeType: "g6-nanode-1",
		Devices: []api.DeviceSpec{{Device: "/", SizeBytes: 1 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	d := &fileDest{instanceID: 100, ip: "192.0.2.9", label: "app-dest", token: "tk"}
	s.fileDests.Store(m.ID, d)
	calls := 0
	s.destProbe = func(addr string) bool {
		calls++
		return calls > 30 // the receiver only answers LONG after the warn grace
	}
	// Tiny fast window so the warn fires quickly, then keep polling.
	s.watchFileDest(m.ID, d.instanceID, net.JoinHostPort(d.ip, "5999"), 5*time.Millisecond, time.Millisecond, time.Millisecond)

	if !s.destReady(api.Migration{ID: m.ID, BootTarget: api.BootTargetFile}) {
		t.Fatal("a receiver answering after the warn grace must still mark the destination ready")
	}
	evs, err := s.st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawWarn, sawReady bool
	for _, e := range evs {
		if strings.Contains(e.Message, "keeps watching") {
			sawWarn = true
		}
		if strings.Contains(e.Message, "ready to receive") {
			sawReady = true
		}
	}
	if !sawWarn {
		t.Error("the manual-install hint (with the keeps-watching promise) should have been logged once the grace passed")
	}
	if !sawReady {
		t.Error("the destination-ready event should have been logged when the receiver finally answered")
	}
}

// A watcher must stop when its destination record is dropped (migration
// deleted/closed) or replaced by a retried launch — a stale watcher must not
// poll forever or mark a different instance's record ready.
func TestDestWatcherStopsWhenDestinationReplaced(t *testing.T) {
	s := automationServer(t)
	d := &fileDest{instanceID: 100, ip: "192.0.2.9", label: "app-dest", token: "tk"}
	s.fileDests.Store(int64(7), d)
	s.destProbe = func(string) bool { return false } // never answers
	done := make(chan struct{})
	// Long fast window (no warn) polling every ms; it must exit when the record
	// is replaced, not run forever.
	go func() {
		s.watchFileDest(7, d.instanceID, net.JoinHostPort(d.ip, "5999"), time.Hour, time.Millisecond, time.Millisecond)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	// A retried launch replaced the record with a NEW instance.
	s.fileDests.Store(int64(7), &fileDest{instanceID: 200, ip: "192.0.2.10", label: "app-dest"})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stale watcher must exit once its destination record is replaced")
	}
	if v, _ := s.fileDests.Load(int64(7)); v.(*fileDest).ready || v.(*fileDest).instanceID != 200 {
		t.Error("the stale watcher must not touch the replacement record")
	}
}

// The readiness probe must prove the RECEIVER answered — not merely that
// something accepted TCP. A crash-looping service (or anything else squatting
// the port) accepts connections briefly without ever serving; marking that
// "ready" would redirect the agent into a dead port. The probe performs a TLS
// handshake verified against the appliance's own data-plane CA.
func TestTLSProbeDest(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeTestReceiverCert(t, dir)
	s := &Server{}
	s.cfg.PublicHost = "127.0.0.1"
	s.cfg.TLS = transport.Files{CertFile: certFile, KeyFile: keyFile, CAFile: certFile}

	// Nothing listening → not ready.
	if s.tlsProbeDest("127.0.0.1:1") {
		t.Error("a closed port must not probe ready")
	}

	// A plain TCP listener that accepts and closes (a crash-looping or foreign
	// service) → not ready.
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	go func() {
		for {
			c, aerr := plain.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if s.tlsProbeDest(plain.Addr().String()) {
		t.Error("a bare TCP accept must not probe ready — the receiver never answered")
	}

	// The real receiver (mTLS with the appliance's cert) → ready.
	srvCfg, err := transport.ServerConfig(s.cfg.TLS)
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // drives the server side of the handshake
				_ = c.Close()
			}(c)
		}
	}()
	if !s.tlsProbeDest(ln.Addr().String()) {
		t.Error("the appliance-certified receiver must probe ready")
	}
}

// writeTestReceiverCert writes a self-signed cert/key (its own CA) with server
// and client EKUs and a 127.0.0.1 SAN, standing in for the appliance's
// data-plane receiver certificate in tests.
func writeTestReceiverCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "vmrepl-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "receiver.crt")
	keyFile = filepath.Join(dir, "receiver.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// isPrivateIP must pick out public IPv4s (the destination's reachable address).
func TestIsPrivateIP(t *testing.T) {
	for _, priv := range []string{"10.0.0.1", "192.168.1.5", "172.16.0.1", "127.0.0.1", "169.254.1.1", "bogus"} {
		if !isPrivateIP(priv) {
			t.Errorf("%s should be treated as non-public", priv)
		}
	}
	for _, pub := range []string{"172.237.70.65", "203.0.113.10", "8.8.8.8"} {
		if isPrivateIP(pub) {
			t.Errorf("%s should be treated as public", pub)
		}
	}
}
