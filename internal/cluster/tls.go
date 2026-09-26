package cluster

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Nodes talk to each other over mutual TLS on one port, across the plain
// internet: no VPN needed, because mutual TLS already encrypts the traffic
// and proves both ends are cluster members.
//
// Membership is proven by a private certificate authority that exists only
// for this cluster (never Let's Encrypt): a certificate signed by it is a
// cluster member, anything else is refused. Nobody makes them by hand: the
// first node of a new site makes the CA the first time it starts, and every
// node makes its own certificate from the CA when it has none
// (EnsureCerts). `make install` copies the CA to a new node from the
// machine it's set up from, over the operator's own ssh login; it's never
// in the repository, so having the source gets nobody into the cluster.
//
// Every node certificate carries the same DNS name, clusterName, and the
// connecting side checks for that name. We deliberately don't check the
// node's real hostname: nodes are found by hostname, but the Studio's home
// IP (and so what its name points at) can change, and identity comes from
// the CA's signature, not from DNS. The certificate's CommonName is the
// node's id, for logs.
const clusterName = "grus-node"

// EnsureCerts makes whatever cluster certificates this node is missing,
// in the files the config names: the CA (caCrt, with its key beside it as
// ca.key) only when makeCA is set (the first node of a new site) and
// there's none, then this node's own certificate and key, signed by the
// CA, when there are none. A node joining a site needs the site's CA
// copied in first (make install does it); without it this says so.
func EnsureCerts(caCrt, crt, key, id string, makeCA bool) error {
	dir := filepath.Dir(caCrt)
	caKey := filepath.Join(dir, "ca.key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(caCrt); errors.Is(err, os.ErrNotExist) {
		if !makeCA {
			return fmt.Errorf("no cluster CA at %s: copy ca.crt and ca.key from a node of the site (make install does this)", caCrt)
		}
		if err := InitCA(dir); err != nil {
			return err
		}
		if caCrt != filepath.Join(dir, "ca.crt") {
			if err := os.Rename(filepath.Join(dir, "ca.crt"), caCrt); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(crt); err == nil {
		if _, err := os.Stat(key); err == nil {
			return nil
		}
	}
	if _, err := os.Stat(caKey); err != nil {
		return fmt.Errorf("no certificate for this node at %s, and no CA key (%s) to make one with", crt, caKey)
	}
	return issueNodeCert(caCrt, caKey, crt, key, id)
}

// InitCA creates ca.crt and ca.key in dir. It refuses to overwrite an
// existing CA, since that would cut every existing node out of the cluster.
func InitCA(dir string) error {
	crtPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%s already exists", keyPath)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "grus cluster CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	return writePair(crtPath, keyPath, der, key)
}

// IssueNodeCert creates <id>.crt and <id>.key in dir, signed by the CA in
// the same directory.
func IssueNodeCert(dir, id string) error {
	return issueNodeCert(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"),
		filepath.Join(dir, id+".crt"), filepath.Join(dir, id+".key"), id)
}

func issueNodeCert(caCrt, caKey, crtPath, keyPath, id string) error {
	ca, err := tls.LoadX509KeyPair(caCrt, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: id},
		DNSNames:     []string{clusterName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// Each node is both a server (others dial it) and a client.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		return err
	}
	return writePair(crtPath, keyPath, der, key)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	return n
}

func writePair(crtPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(crtPath, crt, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

// LoadTLS builds the TLS settings for the cluster port from the CA
// certificate and this node's certificate and key.
func LoadTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no certificates in " + caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// As a server: only accept clients with a certificate from our CA.
		ClientCAs:  pool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		// As a client: only talk to servers with a certificate from our CA.
		RootCAs:    pool,
		ServerName: clusterName,
		MinVersion: tls.VersionTLS13,
	}, nil
}
