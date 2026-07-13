package main

import (
	"flag"
	"io"
	"testing"

	"github.com/tiny125/vm-replication/internal/protocol"
)

// The appliance's destination bootstrap (destCloudInit in internal/appliance)
// launches this binary on the destination Linode as:
//
//	vmrepl-receiver -listen :5999 -device / -mode file -cert … -key … -ca …
//
// Go's flag package EXITS on an unknown flag, so every flag used there MUST be
// defined here. It once wasn't: "-mode" was missing, the receiver died with
// "flag provided but not defined" on every start, systemd's Restart=always
// crash-looped it into a start-limit failure, and the migration sat at
// "installing the file receiver" forever — the automatic cloud-init install
// AND the manual Lish paste both "succeeded" without a listening receiver.
func TestReceiverFlagsCoverDestinationBootstrap(t *testing.T) {
	fs := flag.NewFlagSet("receiver", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defineFlags(fs)
	for _, name := range []string{"listen", "device", "mode", "cert", "key", "ca"} {
		if fs.Lookup(name) == nil {
			t.Errorf("flag -%s is used by the destination bootstrap but not defined", name)
		}
	}
	// The exact argv the bootstrap's systemd unit uses must parse.
	fs2 := flag.NewFlagSet("receiver", flag.ContinueOnError)
	fs2.SetOutput(io.Discard)
	o := defineFlags(fs2)
	if err := fs2.Parse([]string{
		"-listen", ":5999", "-device", "/", "-mode", "file",
		"-cert", "/etc/vmrepl/receiver.crt", "-key", "/etc/vmrepl/receiver.key", "-ca", "/etc/vmrepl/ca.crt",
	}); err != nil {
		t.Fatalf("the destination bootstrap's ExecStart argv must parse: %v", err)
	}
	if o.mode != "file" || o.device != "/" || o.listen != ":5999" {
		t.Errorf("parsed options wrong: %+v", o)
	}
}

// -mode restricts what sessions the receiver accepts: a receiver applying files
// to a live root ("-mode file") must refuse a raw block stream, and vice versa.
// An empty mode (standalone/testing use) accepts both.
func TestModeHelloCheck(t *testing.T) {
	fileHello := protocol.Hello{Mode: protocol.ModeFile}
	blockHello := protocol.Hello{} // empty Mode = block session

	check, err := modeHelloCheck("file")
	if err != nil {
		t.Fatalf("mode file: %v", err)
	}
	if err := check(fileHello); err != nil {
		t.Errorf("mode file must accept a file session: %v", err)
	}
	if err := check(blockHello); err == nil {
		t.Error("mode file must refuse a block session")
	}

	check, err = modeHelloCheck("block")
	if err != nil {
		t.Fatalf("mode block: %v", err)
	}
	if err := check(blockHello); err != nil {
		t.Errorf("mode block must accept a block session: %v", err)
	}
	if err := check(fileHello); err == nil {
		t.Error("mode block must refuse a file session")
	}

	if check, err := modeHelloCheck(""); err != nil || check != nil {
		t.Errorf("empty mode must accept both (nil check), got check=%v err=%v", check, err)
	}
	if _, err := modeHelloCheck("bogus"); err == nil {
		t.Error("an unknown mode must be rejected at startup")
	}
}
