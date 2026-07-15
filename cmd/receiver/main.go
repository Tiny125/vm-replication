// Command receiver is the standalone target-side daemon. It listens over
// mutually-authenticated TLS and applies an agent's block stream to a target
// device (a raw disk on a Linode in Rescue Mode, or a file image for testing),
// or — with -mode file — an agent's FILE stream onto a live root (the
// file-transfer destination Linode).
//
// The appliance control plane (controld) embeds the same logic per migration;
// this command is for the file-transfer destination and manual/standalone use.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"

	"github.com/tiny125/vm-replication/internal/protocol"
	"github.com/tiny125/vm-replication/internal/receiver"
	"github.com/tiny125/vm-replication/internal/transport"
)

// options are the receiver's command-line settings.
type options struct {
	listen, device, manifest, mode string
	certFile, keyFile, caFile      string
	once                           bool
}

// defineFlags registers every receiver flag on fs. The appliance's destination
// bootstrap (destCloudInit in internal/appliance) launches this binary as
//
//	vmrepl-receiver -listen :5999 -device / -mode file -cert … -key … -ca …
//
// so every flag used there MUST stay defined here: Go's flag package EXITS on
// an unknown flag. A missing "-mode" once crash-looped the destination's
// receiver on every start (systemd Restart=always → start-limit failure) and
// left migrations stuck at "installing the file receiver".
func defineFlags(fs *flag.FlagSet) *options {
	o := &options{}
	fs.StringVar(&o.listen, "listen", ":4444", "listen address")
	fs.StringVar(&o.device, "device", "", "target block device or image file; with -mode file, the output root directory (required)")
	fs.StringVar(&o.manifest, "manifest", "", "path to write the applied manifest (default <device-basename>.cbt)")
	fs.StringVar(&o.mode, "mode", "", "restrict sessions: \"file\" (apply file streams under -device) or \"block\"; empty accepts both")
	fs.BoolVar(&o.once, "once", false, "exit after the first completed session (handy for tests)")
	fs.StringVar(&o.certFile, "cert", "receiver.crt", "receiver certificate PEM")
	fs.StringVar(&o.keyFile, "key", "receiver.key", "receiver private key PEM")
	fs.StringVar(&o.caFile, "ca", "ca.crt", "CA certificate PEM")
	return o
}

// modeHelloCheck returns a HelloCheck enforcing the operator's -mode: a
// receiver applying files onto a live root ("-mode file") must never accept a
// raw BLOCK stream, and vice versa. An empty mode accepts both (standalone /
// testing use). An unknown mode is a startup error.
func modeHelloCheck(mode string) (receiver.HelloCheck, error) {
	switch mode {
	case "":
		return nil, nil
	case "file":
		return func(h protocol.Hello) error {
			if h.Mode != protocol.ModeFile {
				return fmt.Errorf("this receiver only accepts file-transfer sessions (started with -mode file)")
			}
			return nil
		}, nil
	case "block":
		return func(h protocol.Hello) error {
			if h.Mode == protocol.ModeFile {
				return fmt.Errorf("this receiver only accepts block sessions (started with -mode block)")
			}
			return nil
		}, nil
	default:
		return nil, fmt.Errorf("unknown -mode %q (want \"file\" or \"block\")", mode)
	}
}

func main() {
	o := defineFlags(flag.CommandLine)
	flag.Parse()

	if o.device == "" {
		flag.Usage()
		log.Fatal("receiver: --device is required")
	}
	check, err := modeHelloCheck(o.mode)
	if err != nil {
		log.Fatalf("receiver: %v", err)
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

	if err := receiver.Serve(context.Background(), ln, o.device, o.manifest, o.once, nil, nil, nil, nil, nil, check, nil); err != nil {
		log.Fatalf("receiver: %v", err)
	}
}
