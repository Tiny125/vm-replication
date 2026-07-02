// Command receiver is the standalone target-side daemon. It listens over
// mutually-authenticated TLS and applies an agent's block stream to a target
// device (a raw disk on a Linode in Rescue Mode, or a file image for testing).
//
// The appliance control plane (controld) embeds the same logic per migration;
// this command is for manual/standalone use and testing.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"

	"github.com/tiny125/vm-replication/internal/receiver"
	"github.com/tiny125/vm-replication/internal/transport"
)

func main() {
	var (
		listen   = flag.String("listen", ":4444", "listen address")
		device   = flag.String("device", "", "target block device or image file (required)")
		manifest = flag.String("manifest", "", "path to write the applied manifest (default <device-basename>.cbt)")
		once     = flag.Bool("once", false, "exit after the first completed session (handy for tests)")
		certFile = flag.String("cert", "receiver.crt", "receiver certificate PEM")
		keyFile  = flag.String("key", "receiver.key", "receiver private key PEM")
		caFile   = flag.String("ca", "ca.crt", "CA certificate PEM")
	)
	flag.Parse()

	if *device == "" {
		flag.Usage()
		log.Fatal("receiver: --device is required")
	}
	if *manifest == "" {
		*manifest = defaultManifestPath(*device)
	}

	tlsCfg, err := transport.ServerConfig(transport.Files{CertFile: *certFile, KeyFile: *keyFile, CAFile: *caFile})
	if err != nil {
		log.Fatalf("receiver: %v", err)
	}
	ln, err := tls.Listen("tcp", *listen, tlsCfg)
	if err != nil {
		log.Fatalf("receiver: listen: %v", err)
	}
	defer ln.Close()
	log.Printf("receiver listening on %s, applying to %s", *listen, *device)

	if err := receiver.Serve(context.Background(), ln, *device, *manifest, *once, nil, nil, nil, nil, nil, nil); err != nil {
		log.Fatalf("receiver: %v", err)
	}
}
