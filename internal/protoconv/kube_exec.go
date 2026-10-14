package protoconv

import (
	"bytes"
	"slices"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// KubeExecCredentialToProto maps the kube-exec shim's transport to the wire
// (IssueKubeExecCredential, local socket only). The result shares no slice
// or map with c, nor does KubeExecCredentialFromProto's with its input.
func KubeExecCredentialToProto(c fleet.KubeExecCredential) *fleetgrpc.KubeExecCredential {
	return &fleetgrpc.KubeExecCredential{
		ClusterId:                c.ClusterID,
		Server:                   c.Server,
		CertificateAuthorityData: bytes.Clone(c.CertificateAuthorityData),
		TlsServerName:            c.TLSServerName,
		InsecureSkipTlsVerify:    c.InsecureSkipTLSVerify,
		ProxyUrl:                 c.ProxyURL,
		Impersonate:              kubeImpersonationToProto(c.Impersonate),
		Auth:                     c.Auth,
		BearerToken:              c.BearerToken,
		ClientCertificateData:    bytes.Clone(c.ClientCertificateData),
		ClientKeyData:            bytes.Clone(c.ClientKeyData),
		Username:                 c.Username,
		Password:                 c.Password,
		ExpiresAt:                timeToProto(c.ExpiresAt),
	}
}

// KubeExecCredentialFromProto is the inverse of KubeExecCredentialToProto.
func KubeExecCredentialFromProto(pc *fleetgrpc.KubeExecCredential) fleet.KubeExecCredential {
	if pc == nil {
		return fleet.KubeExecCredential{}
	}
	return fleet.KubeExecCredential{
		ClusterID:                pc.GetClusterId(),
		Server:                   pc.GetServer(),
		CertificateAuthorityData: bytes.Clone(pc.GetCertificateAuthorityData()),
		TLSServerName:            pc.GetTlsServerName(),
		InsecureSkipTLSVerify:    pc.GetInsecureSkipTlsVerify(),
		ProxyURL:                 pc.GetProxyUrl(),
		Impersonate:              kubeImpersonationFromProto(pc.GetImpersonate()),
		Auth:                     pc.GetAuth(),
		BearerToken:              pc.GetBearerToken(),
		ClientCertificateData:    bytes.Clone(pc.GetClientCertificateData()),
		ClientKeyData:            bytes.Clone(pc.GetClientKeyData()),
		Username:                 pc.GetUsername(),
		Password:                 pc.GetPassword(),
		ExpiresAt:                timeFromProto(pc.GetExpiresAt()),
	}
}

func kubeImpersonationToProto(i fleet.KubeImpersonation) *fleetgrpc.KubeImpersonation {
	if i.User == "" && i.UID == "" && len(i.Groups) == 0 && len(i.Extra) == 0 {
		return nil
	}
	pi := &fleetgrpc.KubeImpersonation{User: i.User, Uid: i.UID, Groups: slices.Clone(i.Groups)}
	if len(i.Extra) > 0 {
		pi.Extra = make(map[string]*fleetgrpc.KubeStringList, len(i.Extra))
		for k, v := range i.Extra {
			pi.Extra[k] = &fleetgrpc.KubeStringList{Values: slices.Clone(v)}
		}
	}
	return pi
}

func kubeImpersonationFromProto(pi *fleetgrpc.KubeImpersonation) fleet.KubeImpersonation {
	if pi == nil {
		return fleet.KubeImpersonation{}
	}
	i := fleet.KubeImpersonation{User: pi.GetUser(), UID: pi.GetUid(), Groups: slices.Clone(pi.GetGroups())}
	if len(pi.GetExtra()) > 0 {
		i.Extra = make(map[string][]string, len(pi.GetExtra()))
		for k, v := range pi.GetExtra() {
			i.Extra[k] = slices.Clone(v.GetValues())
		}
	}
	return i
}
