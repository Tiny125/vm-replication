// Command applianced is the turnkey "replication server" daemon. Installed by
// scripts/install-replication-server.sh, it serves a password-protected web
// console, enrolls source agents, embeds a receiver per migration, and
// finalizes migrations into launchable Linode artifacts.
//
// On first run it generates an admin password (written to
// <data-dir>/initial-admin-password.txt and logged once) and an at-rest
// encryption key for the stored Linode token.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tiny125/vm-replication/internal/appliance"
	"github.com/tiny125/vm-replication/internal/linode"
	"github.com/tiny125/vm-replication/internal/store"
	"github.com/tiny125/vm-replication/internal/transport"
)

func main() {
	var (
		listen        = flag.String("listen", ":8080", "console HTTP listen address")
		dataDir       = flag.String("data-dir", "/var/lib/vm-repl", "state directory (db, keys, file-fallback volumes)")
		publicHost    = flag.String("public-host", "", "IP/DNS source agents reach this server at (auto-detected if empty)")
		region        = flag.String("region", "us-ord", "default Linode region for volumes/instances")
		objRegion     = flag.String("obj-region", "", "Object Storage region for audit logs (empty = follow the appliance's own region)")
		baseRecvPort  = flag.Int("base-receiver-port", 5000, "first TCP port for per-migration receivers")
		certFile      = flag.String("cert", "", "data-plane (receiver) certificate PEM")
		keyFile       = flag.String("key", "", "data-plane (receiver) key PEM")
		caFile        = flag.String("ca", "", "CA certificate PEM")
		agentCert     = flag.String("agent-cert", "", "agent certificate handed to sources")
		agentKey      = flag.String("agent-key", "", "agent key handed to sources")
		agentBinary   = flag.String("agent-binary", "", "path to the agent binary served to sources")
		convertScript = flag.String("convert-script", "", "path to machine-convert.sh (enables disk conversion)")
		rpoTarget     = flag.Int("rpo-target", 120, "max lag (seconds) for the 'ready to migrate' gate")
		tlsCert       = flag.String("tls-cert", "", "console TLS certificate (self-signed auto-generated if empty)")
		tlsKey        = flag.String("tls-key", "", "console TLS key")
		insecureHTTP  = flag.Bool("insecure-http", false, "serve the console over plain HTTP (NOT recommended; testing/behind-proxy only)")
		showPassword  = flag.Bool("show-password", false, "print the saved console admin password and exit (does not touch the running service)")
	)
	flag.Parse()

	// Password recovery: read the saved plaintext password file and exit. This
	// only reads a file (no DB, no server), so it is safe to run while the
	// appliance is serving — it won't restart, terminate, or change anything.
	if *showPassword {
		pwFile := filepath.Join(*dataDir, "initial-admin-password.txt")
		b, err := os.ReadFile(pwFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not read %s: %v\n(the password is stored only as a hash; if this file was removed it cannot be recovered without resetting)\n", pwFile, err)
			os.Exit(1)
		}
		fmt.Print(string(b))
		return
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatalf("applianced: data dir: %v", err)
	}

	st, err := store.Open(filepath.Join(*dataDir, "appliance.db"))
	if err != nil {
		log.Fatalf("applianced: open store: %v", err)
	}
	defer st.Close()

	// At-rest encryption key for the Linode token.
	key, err := loadOrCreateKey(filepath.Join(*dataDir, "secret.key"))
	if err != nil {
		log.Fatalf("applianced: encryption key: %v", err)
	}
	st.SetEncryptionKey(key)

	// Generate the admin password on first run.
	ctx := context.Background()
	if err := ensureAdminPassword(ctx, st, *dataDir); err != nil {
		log.Fatalf("applianced: admin password: %v", err)
	}

	host := *publicHost
	if host == "" {
		host = detectHost()
		log.Printf("applianced: auto-detected public host %s (override with -public-host)", host)
	}

	applianceID, err := linode.ApplianceLinodeID(ctx)
	if err != nil {
		log.Printf("applianced: could not detect Linode instance id (%v); volume automation disabled (file-fallback)", err)
	} else if applianceID != 0 {
		log.Printf("applianced: running on Linode instance %d", applianceID)
	}

	_, consolePort := splitHostPort(*listen)

	// Console transport: HTTPS by default. Generate a self-signed cert if none
	// is supplied, and compute the public-key pin so the enrollment command can
	// authenticate the server without a public CA (trust on first use).
	scheme := "https"
	consoleCert, consoleKey := *tlsCert, *tlsKey
	var pin string
	if *insecureHTTP {
		scheme = "http"
		log.Printf("applianced: WARNING serving console over plain HTTP (-insecure-http); password/token are exposed in transit")
	} else {
		if consoleCert == "" || consoleKey == "" {
			consoleCert, consoleKey, err = loadOrCreateConsoleCert(*dataDir, host)
			if err != nil {
				log.Fatalf("applianced: console TLS cert: %v", err)
			}
		}
		if pin, err = publicKeyPin(consoleCert); err != nil {
			log.Fatalf("applianced: compute key pin: %v", err)
		}
	}

	srv := appliance.New(ctx, appliance.Config{
		Store:             st,
		DataDir:           *dataDir,
		PublicHost:        host,
		ConsolePort:       consolePort,
		Scheme:            scheme,
		PublicKeyPin:      pin,
		BaseReceiverPort:  *baseRecvPort,
		Region:            *region,
		ObjRegion:         *objRegion,
		TLS:               transport.Files{CertFile: *certFile, KeyFile: *keyFile, CAFile: *caFile},
		AgentCert:         *agentCert,
		AgentKey:          *agentKey,
		CACert:            *caFile,
		AgentBinary:       *agentBinary,
		ApplianceLinodeID: applianceID,
		ConvertScript:     *convertScript,
		RPOTargetSec:      *rpoTarget,
	})
	srv.StartActiveReceivers()
	srv.StartAudit() // tee system logs into the audit trail (changes the global logger)

	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("applianced: console on %s://%s:%d  (data dir %s)", scheme, host, consolePort, *dataDir)
		var serveErr error
		if scheme == "https" {
			if fp, ferr := certFingerprint(consoleCert); ferr == nil {
				log.Printf("applianced: console cert SHA-256 fingerprint (verify in browser): %s", fp)
			}
			log.Printf("applianced: enrollment key pin: sha256//%s", pin)
			serveErr = httpSrv.ListenAndServeTLS(consoleCert, consoleKey)
		} else {
			serveErr = httpSrv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Fatalf("applianced: serve: %v", serveErr)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("applianced: shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
}

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		k := make([]byte, hex.DecodedLen(len(b)))
		n, err := hex.Decode(k, b)
		if err == nil && n == 32 {
			return k[:32], nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func ensureAdminPassword(ctx context.Context, st *store.Store, dataDir string) error {
	set, err := st.AdminPasswordSet(ctx)
	if err != nil {
		return err
	}
	if set {
		return nil
	}
	pwBytes := make([]byte, 12)
	if _, err := rand.Read(pwBytes); err != nil {
		return err
	}
	pw := hex.EncodeToString(pwBytes) // 24 hex chars
	if err := st.SetAdminPassword(ctx, pw); err != nil {
		return err
	}
	pwFile := filepath.Join(dataDir, "initial-admin-password.txt")
	_ = os.WriteFile(pwFile, []byte(pw+"\n"), 0o600)
	log.Printf("================ INITIAL ADMIN PASSWORD ================")
	log.Printf("  %s", pw)
	log.Printf("  (also saved to %s)", pwFile)
	log.Printf("=======================================================")
	return nil
}

// detectHost returns the first global-unicast IPv4 address, or "localhost".
func detectHost() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.IsGlobalUnicast() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return "localhost"
}

func splitHostPort(listen string) (string, int) {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", 8080
	}
	port := 8080
	if n, err := net.LookupPort("tcp", p); err == nil {
		port = n
	}
	return h, port
}
