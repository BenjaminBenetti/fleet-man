package protoconv

import (
	"reflect"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

func TestKubeExecCredentialRoundTripAllFields(t *testing.T) {
	in := filled[fleet.KubeExecCredential](t)
	requireEqual(t, KubeExecCredentialFromProto(KubeExecCredentialToProto(in)), in)
	if got := KubeExecCredentialFromProto(nil); !reflect.DeepEqual(got, fleet.KubeExecCredential{}) {
		t.Fatalf("nil credential proto should map to the zero value, got %+v", got)
	}
	empty := KubeExecCredentialToProto(fleet.KubeExecCredential{})
	if empty.Impersonate != nil || empty.ExpiresAt != nil {
		t.Fatalf("an unset impersonation or expiry must be absent from the wire: %+v", empty)
	}
	if back := KubeExecCredentialFromProto(empty); !reflect.DeepEqual(back, fleet.KubeExecCredential{}) {
		t.Fatalf("empty credential read back as %+v", back)
	}
}

func TestKubeExecCredentialWirePlacement(t *testing.T) {
	pinArity[fleet.KubeExecCredential](t, 14)
	pinArity[fleet.KubeImpersonation](t, 4)
	exp := time.Unix(1900000000, 0).UTC()
	c := fleet.KubeExecCredential{
		ClusterID: "id", Server: "srv", CertificateAuthorityData: []byte("ca"), TLSServerName: "sni",
		ProxyURL: "proxy", Auth: "auth", BearerToken: "tok", ClientCertificateData: []byte("cert"),
		ClientKeyData: []byte("key"), Username: "user", Password: "pw", ExpiresAt: exp,
		Impersonate: fleet.KubeImpersonation{User: "as", UID: "as-uid", Groups: []string{"g"}, Extra: map[string][]string{"k": {"v"}}},
	}
	pc := KubeExecCredentialToProto(c)
	if pc.GetClusterId() != "id" || pc.GetServer() != "srv" || string(pc.GetCertificateAuthorityData()) != "ca" ||
		pc.GetTlsServerName() != "sni" || pc.GetProxyUrl() != "proxy" || pc.GetAuth() != "auth" ||
		pc.GetBearerToken() != "tok" || string(pc.GetClientCertificateData()) != "cert" || string(pc.GetClientKeyData()) != "key" ||
		pc.GetUsername() != "user" || pc.GetPassword() != "pw" || !pc.GetExpiresAt().AsTime().Equal(exp) || pc.GetInsecureSkipTlsVerify() {
		t.Fatalf("credential wire placement wrong: %v", pc)
	}
	im := pc.GetImpersonate()
	if im.GetUser() != "as" || im.GetUid() != "as-uid" || !reflect.DeepEqual(im.GetGroups(), []string{"g"}) ||
		!reflect.DeepEqual(im.GetExtra()["k"].GetValues(), []string{"v"}) {
		t.Fatalf("impersonation wire placement wrong: %v", im)
	}
	if !KubeExecCredentialToProto(fleet.KubeExecCredential{InsecureSkipTLSVerify: true}).GetInsecureSkipTlsVerify() {
		t.Fatal("insecure_skip_tls_verify not set from InsecureSkipTLSVerify")
	}
}

func TestKubeExecCredentialConversionSharesNothing(t *testing.T) {
	c := filled[fleet.KubeExecCredential](t)
	pc := KubeExecCredentialToProto(c)
	c.CertificateAuthorityData[0], c.ClientCertificateData[0], c.ClientKeyData[0] = 'x', 'x', 'x'
	c.Impersonate.Groups[0] = "changed"
	for k := range c.Impersonate.Extra {
		c.Impersonate.Extra[k][0] = "changed"
	}
	back := KubeExecCredentialFromProto(pc)
	if back.CertificateAuthorityData[0] == 'x' || back.ClientCertificateData[0] == 'x' || back.ClientKeyData[0] == 'x' ||
		back.Impersonate.Groups[0] == "changed" {
		t.Fatalf("the proto aliases the credential it was converted from: %+v", back)
	}
	for _, v := range pc.GetImpersonate().GetExtra() {
		if v.GetValues()[0] == "changed" {
			t.Fatal("the proto's impersonation extra aliases the credential's")
		}
	}
	pc.GetImpersonate().GetGroups()[0] = "again"
	if back.Impersonate.Groups[0] == "again" {
		t.Fatal("the read-back credential aliases the proto")
	}
}
