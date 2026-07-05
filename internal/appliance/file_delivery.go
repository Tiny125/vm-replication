package appliance

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// File-transfer cutover delivery.
//
// A file migration stages the source's file tree on the appliance (the file
// receiver writes it there). At cutover the appliance launches the destination
// Linode from the chosen OS image, then the operator pastes ONE command in the
// destination's Lish console. That command downloads the staged tree as a tar
// stream from the appliance, extracts it over the live root (preserving
// owners/permissions), signals completion back to the appliance, and reboots so
// every copied service starts from the migrated files.
//
// This reuses the token-gated, key-pinned pattern proven by the disk-boot
// rescue copy — no per-destination certificates, no new mTLS hop.

// fileDelivery authorizes one migration's file tar download + completion ping.
type fileDelivery struct {
	migID   int64
	root    string        // the staged file tree to tar up
	done    chan struct{} // closed when the destination reports the extract finished
	expires time.Time
}

// registerFileDelivery mints a token authorizing the tar download + done ping
// for a migration's file cutover, and returns it with the done channel.
func (s *Server) registerFileDelivery(migID int64, root string, ttl time.Duration) (string, chan struct{}) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	done := make(chan struct{})
	s.fileDeliveries.Store(tok, &fileDelivery{migID: migID, root: root, done: done, expires: time.Now().Add(ttl)})
	return tok, done
}

func (s *Server) lookupFileDelivery(token string) (*fileDelivery, bool) {
	v, ok := s.fileDeliveries.Load(token)
	if !ok {
		return nil, false
	}
	d := v.(*fileDelivery)
	if time.Now().After(d.expires) {
		s.fileDeliveries.Delete(token)
		return nil, false
	}
	return d, true
}

// dropFileDelivery invalidates a migration's delivery token(s).
func (s *Server) dropFileDelivery(migID int64) {
	s.fileDeliveries.Range(func(k, v any) bool {
		if v.(*fileDelivery).migID == migID {
			s.fileDeliveries.Delete(k)
		}
		return true
	})
}

// fileCopyCmd is the ONE line the operator pastes into the destination's Lish
// console. It streams the staged tree onto the live root, pings done, reboots.
func (s *Server) fileCopyCmd(token string) string {
	base := fmt.Sprintf("%s://%s:%d", s.scheme(), s.cfg.PublicHost, s.cfg.ConsolePort)
	pin := s.curlPinFlag()
	// tar extracts relative paths (the staged tree has no leading slash), -p keeps
	// perms, --same-owner keeps owners. Virtual dirs are already excluded at the
	// source; --keep-directory-symlink avoids clobbering e.g. /lib -> /usr/lib.
	return fmt.Sprintf("curl -fsSL %s'%s/cutover/files.tar?token=%s' | tar -xp --same-owner --keep-directory-symlink -C / && curl -fsS %s'%s/cutover/done?token=%s' ; reboot",
		pin, base, token, pin, base, token)
}

// handleCutoverTar streams the migration's staged file tree as a tar archive
// (GET /cutover/files.tar, token-gated — the destination has no console
// session). Paths are relative (no leading slash) so tar extracts under /.
func (s *Server) handleCutoverTar(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupFileDelivery(r.URL.Query().Get("token"))
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired cutover token")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	tw := tar.NewWriter(w)
	defer tw.Close()
	root := d.root
	_ = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		info, ierr := de.Info()
		if ierr != nil {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return nil
		}
		hdr.Name = rel // relative -> extracts under /
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, oerr := os.Open(p)
			if oerr != nil {
				return nil
			}
			_, _ = io.Copy(tw, f)
			_ = f.Close()
		}
		return nil
	})
}

// handleCutoverDone is the destination's completion ping (GET /cutover/done):
// the pasted command hits it after extracting the files, so finalizeFile stops
// waiting and marks the migration launched.
func (s *Server) handleCutoverDone(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupFileDelivery(r.URL.Query().Get("token"))
	if !ok {
		writeErr(w, http.StatusForbidden, "invalid or expired cutover token")
		return
	}
	select {
	case <-d.done: // already closed
	default:
		close(d.done)
	}
	log.Printf("appliance: migration %d: destination reported file extract complete", d.migID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
