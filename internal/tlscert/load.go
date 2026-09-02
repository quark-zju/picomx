// Package tlscert discovers and reloads the one TLS identity used by picomx.
package tlscert

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	fullchainName  = "fullchain.pem"
	privateKeyName = "privkey.pem"
)

type loadedCertificate struct {
	certificate *tls.Certificate
	leaf        *x509.Certificate
	hash        [sha256.Size]byte
	directory   string
}

func loadCertificate(certDir string, hostnames []string) (loadedCertificate, error) {
	names, err := normalizeHostnames(hostnames)
	if err != nil {
		return loadedCertificate{}, err
	}
	for _, candidate := range domainCandidates(names[0]) {
		directory := filepath.Join(certDir, candidate)
		loaded, err := loadDirectory(directory, names)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return loadedCertificate{}, fmt.Errorf("load certificate directory %q: %w", directory, err)
		}
		return loaded, nil
	}
	return loadedCertificate{}, fmt.Errorf("no certificate found for %q under %q", names[0], certDir)
}

func loadDirectory(directory string, hostnames []string) (loadedCertificate, error) {
	certPEM, err := os.ReadFile(filepath.Join(directory, fullchainName))
	if err != nil {
		return loadedCertificate{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(directory, privateKeyName))
	if err != nil {
		return loadedCertificate{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return loadedCertificate{}, fmt.Errorf("parse certificate pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return loadedCertificate{}, fmt.Errorf("parse leaf certificate: %w", err)
	}
	for _, hostname := range hostnames {
		if err := leaf.VerifyHostname(hostname); err != nil {
			return loadedCertificate{}, fmt.Errorf("certificate does not cover %q: %w", hostname, err)
		}
	}
	pair.Leaf = leaf
	content := append(append(make([]byte, 0, len(certPEM)+len(keyPEM)), certPEM...), keyPEM...)
	return loadedCertificate{
		certificate: &pair,
		leaf:        leaf,
		hash:        sha256.Sum256(content),
		directory:   directory,
	}, nil
}

func normalizeHostnames(hostnames []string) ([]string, error) {
	normalized := make([]string, 0, len(hostnames))
	seen := make(map[string]struct{}, len(hostnames))
	for _, raw := range hostnames {
		name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if !validDNSName(name) {
			return nil, fmt.Errorf("invalid TLS hostname %q", raw)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	if len(normalized) == 0 {
		return nil, errors.New("at least one TLS hostname is required")
	}
	return normalized, nil
}

func validDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func domainCandidates(hostname string) []string {
	labels := strings.Split(hostname, ".")
	if len(labels) < 2 {
		return nil
	}
	candidates := make([]string, 0, len(labels)-1)
	for index := 0; index < len(labels)-1; index++ {
		candidates = append(candidates, strings.Join(labels[index:], "."))
	}
	return candidates
}
