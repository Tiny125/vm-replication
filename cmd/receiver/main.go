// Command receiver is the standalone target-side daemon. It listens over
// mutually-authenticated TLS and applies an agent's block stream to a target
// device (a raw disk on a Linode in Rescue Mode, or a file image for testing).
//
// The appliance control plane (controld) embeds the same logic per migration;
// this command is for manual/standalone use.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"

	"github.com/tiny125/vm-replication/internal/receiver"
	"github.com/tiny125/vm-replication/internal/transport"
)

// options are the receiver's command-line settings.
type options struct {
	listen, device, manifest  string
	certFile, keyFile, caFile string
	once                      bool
}

// defineFlags registers every receiver flag on fs.
func defineFlags(fs *flag.FlagSet) *options {
	o := &options{}
	fs.StringVar(&o.listen, "listen", ":4444", "listen address")
	fs.StringVar(&o.device, "device", "", "target block device or image file (required)")
	fs.StringVar(&o.manifest, "manifest", "", "path to write the applied manifest (default <device-basename>.cbt)")
	fs.BoolVar(&o.once, "once", false, "exit after the first completed session (handy for tests)")
	fs.StringVar(&o.certFile, "cert", "receiver.crt", "receiver certificate PEM")
	fs.StringVar(&o.keyFile, "key", "receiver.key", "receiver private key PEM")
	fs.StringVar(&o.caFile, "ca", "ca.crt", "CA certificate PEM")
	return o
}

func main() {
	o := defineFlags(flag.CommandLine)
	flag.Parse()

	if o.device == "" {
		flag.Usage()
		log.Fatal("receiver: --device is required")
	}
	if o.manifest == "" {
		o.manifest = defaultManifestPath(o.device)
	}

	tlsCfg, err := transport.ServerConfig(transport.Files{CertFile: o.certFile, KeyFile: o.keyFile, CAFile: o.caFile})
	if err != nil {
		log.Fatalf("receiver: %v", err)
	}
	ln, err := tls.Listen("tcp", o.listen, tlsCfg)
	if err != nil {
		log.Fatalf("receiver: listen: %v", err)
	}
	defer ln.Close()
	log.Printf("receiver listening on %s, applying to %s", o.listen, o.device)

	if err := receiver.Serve(context.Background(), ln, o.device, o.manifest, o.once, nil, nil, nil, nil, nil, nil); err != nil {
		log.Fatalf("receiver: %v", err)
	}
}
