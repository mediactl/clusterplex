// Package plexcert reads the TLS certificate Plex Media Server serves: the
// plex.direct wildcard plex.tv issues it, kept in its data directory as a
// PKCS#12 file whose password is derived from the server's identifier. The
// proxy ends a client's TLS connection with it, so a request Plex's own
// certificate protects can be read like a plain one (pkg/proxy).
package plexcert

import (
	"crypto/sha512"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// recheck is how often the files are looked at again.
const recheck = 30 * time.Second

var processedID = regexp.MustCompile(`\bProcessedMachineIdentifier="([0-9a-f]+)"`)

// Store serves the certificate in a Plex data directory, read again when
// the file changes (Plex renews it in place).
type Store struct {
	// Dir is Plex's data directory, the one holding Preferences.xml.
	Dir string

	mu      sync.Mutex
	checked time.Time
	mod     time.Time
	cert    *tls.Certificate
}

// For returns the certificate for a TLS client asking for serverName, or
// nil when there is none that is valid for that name right now.
func (s *Store) For(serverName string) *tls.Certificate {
	if serverName == "" {
		return nil
	}
	cert := s.current()
	if cert == nil || cert.Leaf == nil {
		return nil
	}
	now := time.Now()
	if now.Before(cert.Leaf.NotBefore) || now.After(cert.Leaf.NotAfter) || cert.Leaf.VerifyHostname(serverName) != nil {
		return nil
	}
	return cert
}

func (s *Store) current() *tls.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.checked) < recheck {
		return s.cert
	}
	s.checked = time.Now()
	path := filepath.Join(s.Dir, "Cache", "certificate.p12")
	info, err := os.Stat(path)
	if err != nil {
		s.cert, s.mod = nil, time.Time{}
		return nil
	}
	if s.cert != nil && info.ModTime().Equal(s.mod) {
		return s.cert
	}
	cert, err := Load(path, filepath.Join(s.Dir, "Preferences.xml"))
	if err != nil {
		s.cert, s.mod = nil, time.Time{}
		return nil
	}
	s.cert, s.mod = cert, info.ModTime()
	return cert
}

// Load reads the certificate file p12 with the password Plex derives from
// the ProcessedMachineIdentifier in the preferences file.
func Load(p12, preferences string) (*tls.Certificate, error) {
	prefs, err := os.ReadFile(preferences)
	if err != nil {
		return nil, err
	}
	m := processedID.FindSubmatch(prefs)
	if m == nil {
		return nil, errors.New("plexcert: the preferences name no ProcessedMachineIdentifier")
	}
	data, err := os.ReadFile(p12)
	if err != nil {
		return nil, err
	}
	return Decode(data, string(m[1]))
}

// Decode opens a certificate file's bytes for the server whose processed
// machine identifier is id.
func Decode(data []byte, id string) (*tls.Certificate, error) {
	sum := sha512.Sum512([]byte("plex" + id))
	key, leaf, chain, err := pkcs12.DecodeChain(data, hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return cert, nil
}
