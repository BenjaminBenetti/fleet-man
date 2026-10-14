//go:build !nok8s

package podspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
)

// SpecHash is the hash pod verification compares before it trusts a pod:
// of the parts of a pod spec that decide what the pod can reach and what it
// runs. fleet keeps it on the StatefulSet (the spec-hash annotation) from
// its own render, and recomputes it from the live pod; a pod whose spec was
// changed or substituted hashes differently. The annotation itself is never
// trusted, only the recomputation from fleet's render.
//
// It covers the volumes, the pod's and every container's security context,
// the containers' commands, environment, mounts, devices, ports and hooks
// (init and ephemeral containers included, so a debug container added to a
// live pod changes it), the host namespaces and the service account. It
// leaves out what the API server and the cluster change on their own, so a
// render and the pod made from it hash the same: the image (verification
// checks the running image's digest instead, and fleet re-points a
// gateway-delivered reference without restarting the pod), resources (a
// LimitRange fills them in), pull policy, termination message settings,
// probes, scheduling, metadata. Defaulted fields (a Secret volume's mode, a
// port's protocol, the default service account) are compared as defaulted.
func SpecHash(spec *corev1.PodSpec) string {
	h := hashedPod{
		HostNetwork:           spec.HostNetwork,
		HostPID:               spec.HostPID,
		HostIPC:               spec.HostIPC,
		HostUsers:             spec.HostUsers,
		ShareProcessNamespace: spec.ShareProcessNamespace,
		SecurityContext:       spec.SecurityContext,
		ServiceAccountName:    spec.ServiceAccountName,
		AutomountToken:        spec.AutomountServiceAccountToken,
		RuntimeClassName:      spec.RuntimeClassName,
	}
	if h.SecurityContext == nil {
		h.SecurityContext = &corev1.PodSecurityContext{}
	}
	if h.ServiceAccountName == "" {
		h.ServiceAccountName = "default"
	}
	for _, v := range spec.Volumes {
		h.Volumes = append(h.Volumes, normalizeVolume(v))
	}
	for _, c := range spec.InitContainers {
		h.InitContainers = append(h.InitContainers, hashContainer(c, spec.HostNetwork))
	}
	for _, c := range spec.Containers {
		h.Containers = append(h.Containers, hashContainer(c, spec.HostNetwork))
	}
	for _, c := range spec.EphemeralContainers {
		h.EphemeralContainers = append(h.EphemeralContainers, hashContainer(corev1.Container(c.EphemeralContainerCommon), spec.HostNetwork))
	}
	data, err := json.Marshal(h)
	if err != nil {
		// Every field is plain data; Marshal cannot fail on it.
		panic(err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type hashedPod struct {
	Volumes               []corev1.Volume            `json:"volumes"`
	InitContainers        []hashedContainer          `json:"initContainers"`
	Containers            []hashedContainer          `json:"containers"`
	EphemeralContainers   []hashedContainer          `json:"ephemeralContainers"`
	HostNetwork           bool                       `json:"hostNetwork"`
	HostPID               bool                       `json:"hostPID"`
	HostIPC               bool                       `json:"hostIPC"`
	HostUsers             *bool                      `json:"hostUsers"`
	ShareProcessNamespace *bool                      `json:"shareProcessNamespace"`
	SecurityContext       *corev1.PodSecurityContext `json:"securityContext"`
	ServiceAccountName    string                     `json:"serviceAccountName"`
	AutomountToken        *bool                      `json:"automountServiceAccountToken"`
	RuntimeClassName      *string                    `json:"runtimeClassName"`
}

type hashedContainer struct {
	Name            string                         `json:"name"`
	Command         []string                       `json:"command"`
	Args            []string                       `json:"args"`
	WorkingDir      string                         `json:"workingDir"`
	Env             []corev1.EnvVar                `json:"env"`
	EnvFrom         []corev1.EnvFromSource         `json:"envFrom"`
	VolumeMounts    []corev1.VolumeMount           `json:"volumeMounts"`
	VolumeDevices   []corev1.VolumeDevice          `json:"volumeDevices"`
	Ports           []corev1.ContainerPort         `json:"ports"`
	Lifecycle       *corev1.Lifecycle              `json:"lifecycle"`
	SecurityContext *corev1.SecurityContext        `json:"securityContext"`
	RestartPolicy   *corev1.ContainerRestartPolicy `json:"restartPolicy"`
	Stdin           bool                           `json:"stdin"`
	StdinOnce       bool                           `json:"stdinOnce"`
	TTY             bool                           `json:"tty"`
}

// defaultMode is the file mode the API server gives a projected file when
// the volume names none.
const defaultMode = int32(0o644)

func normalizeVolume(v corev1.Volume) corev1.Volume {
	v = *v.DeepCopy()
	s := &v.VolumeSource
	switch {
	case s.Secret != nil && s.Secret.DefaultMode == nil:
		s.Secret.DefaultMode = ptr(defaultMode)
	case s.ConfigMap != nil && s.ConfigMap.DefaultMode == nil:
		s.ConfigMap.DefaultMode = ptr(defaultMode)
	case s.Projected != nil && s.Projected.DefaultMode == nil:
		s.Projected.DefaultMode = ptr(defaultMode)
	case s.DownwardAPI != nil && s.DownwardAPI.DefaultMode == nil:
		s.DownwardAPI.DefaultMode = ptr(defaultMode)
	case s.HostPath != nil && s.HostPath.Type == nil:
		s.HostPath.Type = ptr(corev1.HostPathUnset)
	}
	return v
}

func hashContainer(c corev1.Container, hostNetwork bool) hashedContainer {
	c = *c.DeepCopy()
	for i := range c.Ports {
		p := &c.Ports[i]
		if p.Protocol == "" {
			p.Protocol = corev1.ProtocolTCP
		}
		if hostNetwork && p.HostPort == 0 {
			p.HostPort = p.ContainerPort
		}
	}
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPropagation == nil {
			c.VolumeMounts[i].MountPropagation = ptr(corev1.MountPropagationNone)
		}
	}
	for i := range c.Env {
		if f := c.Env[i].ValueFrom; f != nil && f.FieldRef != nil && f.FieldRef.APIVersion == "" {
			f.FieldRef.APIVersion = "v1"
		}
	}
	return hashedContainer{
		Name: c.Name, Command: c.Command, Args: c.Args, WorkingDir: c.WorkingDir,
		Env: c.Env, EnvFrom: c.EnvFrom, VolumeMounts: c.VolumeMounts, VolumeDevices: c.VolumeDevices,
		Ports: c.Ports, Lifecycle: c.Lifecycle, SecurityContext: c.SecurityContext, RestartPolicy: c.RestartPolicy,
		Stdin: c.Stdin, StdinOnce: c.StdinOnce, TTY: c.TTY,
	}
}
