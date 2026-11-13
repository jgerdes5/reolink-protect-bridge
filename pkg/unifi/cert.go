package unifi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"time"
)

// GenerateCert creates the TLS client certificate the camera presents to
// Protect: a self-signed P-256 certificate with the subject unifi-cam-proxy uses.
// It returns key and certificate as one PEM text (like the proxy's client.pem).
func GenerateCert() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return "", err
	}

	subject := pkix.Name{
		Country:            []string{"TW"},
		Locality:           []string{"Taipei"},
		Organization:       []string{"Ubiquiti Networks Inc."},
		OrganizationalUnit: []string{"devint"},
		CommonName:         "camera.ubnt.dev",
		ExtraNames: []pkix.AttributeTypeAndValue{
			{Type: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 1}, Value: "support@ubnt.com"},
		},
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		Issuer:       subject,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(100, 0, 0),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}

	b := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	return string(b), nil
}

// LoadCert parses key + certificate from one PEM text.
func LoadCert(pemText string) (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(pemText), []byte(pemText))
}
