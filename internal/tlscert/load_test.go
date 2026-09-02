package tlscert

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadCertificateFindsParentWildcard(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestCertificate(t, filepath.Join(root, "example.com"), []string{"*.example.com"})
	loaded, err := loadCertificate(root, []string{"mx.example.com", "pop.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loaded.directory, filepath.Join(root, "example.com"); got != want {
		t.Fatalf("directory = %q, want %q", got, want)
	}
	if loaded.certificate == nil || loaded.leaf == nil {
		t.Fatal("loaded certificate is incomplete")
	}
}

func TestLoadCertificateRejectsCertificateMissingServiceName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestCertificate(t, filepath.Join(root, "example.com"), []string{"mx.example.com"})
	if _, err := loadCertificate(root, []string{"mx.example.com", "pop.example.com"}); err == nil {
		t.Fatal("loadCertificate accepted certificate missing POP hostname")
	}
}

func TestDomainCandidates(t *testing.T) {
	t.Parallel()

	got := domainCandidates("mx.mail.example.co.uk")
	want := []string{"mx.mail.example.co.uk", "mail.example.co.uk", "example.co.uk", "co.uk"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}

func writeTestCertificate(t *testing.T, directory string, dnsNames []string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	if err := os.WriteFile(filepath.Join(directory, fullchainName), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, privateKeyName), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}
