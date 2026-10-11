//go:build !nok8s

package kubert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/BenjaminBenetti/fleet-man/internal/atomicfile"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/names"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
)

// tls.go is the mutual TLS of a fleet's shared Buildkit on a connection. The
// fleet has a CA of its own there, kept on the daemon host and never in the
// cluster (state.KubeBuildkitCAPath, 0600). It signs the server's certificate,
// which the server's pod mounts from a Secret, and a client certificate for
// each instance, valid for clientCertTTL, whose subject (CN) is the
// instance's key: the daemon issues it on create, clone, rebuild and start
// and renews it daily (svcruntime.ConfigureBuildkit), and logs each one it
// issues with the instance it went to. An instance holds only its own, so a
// build out of one instance cannot pose as another, and a key copied out of
// an instance stops working when it expires. The CA goes with the fleet's
// objects on the connection (its last instance there destroyed, or the
// fleet), so the next server there takes no certificate of the last one's.

// The lifetimes of the certificates; variables so tests can shorten them.
var (
	// clientCertTTL is an instance's certificate's validity: builds keep
	// working through a daemon outage that long.
	clientCertTTL = 7 * 24 * time.Hour
	// clientCertSkew backdates a certificate's start, for a node clock a
	// little behind the daemon host's.
	clientCertSkew = 5 * time.Minute
	// serverCertTTL is the server's certificate's validity, and
	// serverCertRenew how near its end it is issued again (which restarts
	// the server).
	serverCertTTL   = 365 * 24 * time.Hour
	serverCertRenew = 30 * 24 * time.Hour
	// caTTL is the CA's validity; a CA within serverCertTTL of its end is
	// made again.
	caTTL = 10 * 365 * 24 * time.Hour
)

// The files of the server's Secret, which its pod mounts at buildkitTLSDir.
const (
	tlsCAFile   = "ca.pem"
	tlsCertFile = "cert.pem"
	tlsKeyFile  = "key.pem"
)

// buildkitCA is the fleet's Buildkit CA on a connection, as the daemon keeps
// it: PEM throughout, keys PKCS #8.
type buildkitCA struct {
	V     int    `json:"v"`
	Fleet string `json:"fleet"`
	// CACert and CAKey are the CA.
	CACert string `json:"ca_cert"`
	CAKey  string `json:"ca_key"`
	// ServerName is the server's Service, which its certificate is for.
	ServerName string `json:"server_name"`
	ServerCert string `json:"server_cert"`
	ServerKey  string `json:"server_key"`
	// Installed are the client certificates the fleet's instances on the
	// connection hold, by instance key.
	Installed map[string]installedCert `json:"installed,omitempty"`
}

// installedCert is a client certificate an instance holds.
type installedCert struct {
	Instance  string    `json:"instance"`
	Serial    string    `json:"serial"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

// serverNames are the names the server's certificate is for: its Service as
// the fleet's instances reach it in the namespace (the first, the one they
// check), and qualified.
func (m fleetMeta) serverNames() []string {
	name := m.name(names.SuffixBuildkit)
	return []string{name, name + "." + m.Namespace, name + "." + m.Namespace + ".svc"}
}

// loadCA reads the fleet's CA on the connection (nil when there is none).
func loadCA(cluster, hash string) (*buildkitCA, error) {
	data, err := os.ReadFile(state.KubeBuildkitCAPath(cluster, hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ca buildkitCA
	if err := json.Unmarshal(data, &ca); err != nil {
		return nil, fmt.Errorf("reading the fleet's Buildkit CA: %w", err)
	}
	return &ca, nil
}

func saveCA(cluster, hash string, ca *buildkitCA) error {
	path := state.KubeBuildkitCAPath(cluster, hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ca, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(data, '\n'), 0o600)
}

// rotateCA forgets the fleet's CA on the connection: the next server there
// gets a new one, and takes no certificate this one signed. removed says
// there was one.
func rotateCA(cluster, hash string) (removed bool, err error) {
	err = os.Remove(state.KubeBuildkitCAPath(cluster, hash))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// ensureCA is the fleet's CA on the connection with a server certificate for
// the server's names that is not near its end, each made as needed (and
// saved); changed says the server's certificate is not the one it had, so
// the server needs its Secret again.
func ensureCA(cluster string, m fleetMeta, at time.Time) (ca *buildkitCA, changed bool, err error) {
	ca, err = loadCA(cluster, m.Hash)
	if err != nil {
		// A file that does not read is made again, as a lost one is: the
		// instances get certificates of the new CA as they renew.
		ca = nil
	}
	caCert, caKey, err := ca.parseCA()
	if err != nil || at.Add(serverCertTTL).After(caCert.NotAfter) {
		if ca, err = newCA(m, at); err != nil {
			return nil, false, err
		}
		changed = true
		if caCert, caKey, err = ca.parseCA(); err != nil {
			return nil, false, err
		}
	}
	if changed || !ca.serverCertValid(m, caCert, at) {
		cert, key, err := issue(caCert, caKey, &x509.Certificate{
			Subject:     pkix.Name{CommonName: m.serverNames()[0]},
			DNSNames:    m.serverNames(),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			NotBefore:   at.Add(-clientCertSkew),
			NotAfter:    at.Add(serverCertTTL),
		})
		if err != nil {
			return nil, false, err
		}
		ca.ServerName, ca.ServerCert, ca.ServerKey = m.serverNames()[0], string(cert), string(key)
		changed = true
	}
	if changed {
		if err := saveCA(cluster, m.Hash, ca); err != nil {
			return nil, false, fmt.Errorf("saving the fleet's Buildkit CA: %w", err)
		}
	}
	return ca, changed, nil
}

// newCA makes a CA for the fleet on the connection.
func newCA(m fleetMeta, at time.Time) (*buildkitCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fleet shared Buildkit CA " + m.Hash, Organization: []string{"fleet"}},
		NotBefore:             at.Add(-clientCertSkew),
		NotAfter:              at.Add(caTTL),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return &buildkitCA{V: 1, Fleet: m.Fleet, CACert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), CAKey: string(keyPEM)}, nil
}

// parseCA reads the CA's certificate and key.
func (ca *buildkitCA) parseCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if ca == nil {
		return nil, nil, errors.New("no CA")
	}
	cert, err := parseCert([]byte(ca.CACert))
	if err != nil {
		return nil, nil, err
	}
	b, _ := pem.Decode([]byte(ca.CAKey))
	if b == nil {
		return nil, nil, errors.New("the CA key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errors.New("the CA key does not match its certificate")
	}
	return cert, key, nil
}

// serverCertValid reports whether the CA's server certificate is the CA's,
// a server's, for the server's names, and not near its end. Its start is not
// judged: a clock set back does not replace a good certificate (and with it
// the server's pod).
func (ca *buildkitCA) serverCertValid(m fleetMeta, caCert *x509.Certificate, at time.Time) bool {
	cert, err := parseCert([]byte(ca.ServerCert))
	if err != nil || ca.ServerName != m.serverNames()[0] || at.Add(serverCertRenew).After(cert.NotAfter) {
		return false
	}
	if cert.CheckSignatureFrom(caCert) != nil || strings.Join(cert.DNSNames, " ") != strings.Join(m.serverNames(), " ") {
		return false
	}
	return len(cert.ExtKeyUsage) == 1 && cert.ExtKeyUsage[0] == x509.ExtKeyUsageServerAuth
}

// issueClient issues the instance with key its client certificate, valid
// from now (backdated by clientCertSkew) for clientCertTTL.
func (ca *buildkitCA) issueClient(key string, at time.Time) (fleet.ServiceCredential, error) {
	caCert, caKey, err := ca.parseCA()
	if err != nil {
		return fleet.ServiceCredential{}, err
	}
	nb := at.Add(-clientCertSkew)
	certPEM, keyPEM, err := issue(caCert, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: key},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		NotBefore:   nb,
		NotAfter:    nb.Add(clientCertTTL),
	})
	if err != nil {
		return fleet.ServiceCredential{}, err
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return fleet.ServiceCredential{}, err
	}
	return fleet.ServiceCredential{
		CA: []byte(ca.CACert), Cert: certPEM, Key: keyPEM, ServerName: ca.ServerName,
		Serial: hex.EncodeToString(cert.SerialNumber.Bytes()), NotBefore: cert.NotBefore, NotAfter: cert.NotAfter,
	}, nil
}

// issue signs tmpl (given a fresh key and serial) with the CA, returning the
// certificate and its key as PEM.
func issue(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, tmpl *x509.Certificate) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if tmpl.SerialNumber, err = newSerial(); err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	if keyPEM, err = encodeKey(key); err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseCert(data []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// tlsHash names the server's certificate and CA in its pod template, so a
// new one replaces the pod (buildkitd reads them once, as it starts).
func (ca *buildkitCA) tlsHash() string {
	sum := sha256.Sum256([]byte(ca.CACert + "\x00" + ca.ServerCert))
	return hex.EncodeToString(sum[:8])
}

// buildkitSecret is the server's certificate, its key and the CA it takes
// client certificates of, as its pod mounts them.
func buildkitSecret(m fleetMeta, owner metav1.OwnerReference, ca *buildkitCA) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: m.objectMeta(names.SuffixBuildkit, m.serviceLabels(fleet.ServiceBuildkit), &owner),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			tlsCAFile:   []byte(ca.CACert),
			tlsCertFile: []byte(ca.ServerCert),
			tlsKeyFile:  []byte(ca.ServerKey),
		},
	}
}
