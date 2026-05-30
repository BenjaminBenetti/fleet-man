package deps

import (
	"bufio"
	"os"
	"strings"
)

// Distro identifies a supported Linux distribution family.
type Distro string

const (
	DistroUbuntu  Distro = "ubuntu"
	DistroFedora  Distro = "fedora"
	DistroUnknown Distro = ""
)

// DetectDistro reads /etc/os-release and returns the distro family.
// Returns DistroUnknown if detection fails or the distro is not recognised.
func DetectDistro() Distro {
	osReleaseFile, err := os.Open("/etc/os-release")
	if err != nil {
		return DistroUnknown
	}
	defer osReleaseFile.Close()

	var id, idLike string
	scanner := bufio.NewScanner(osReleaseFile)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "ID="):
			id = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		case strings.HasPrefix(line, "ID_LIKE="):
			idLike = strings.Trim(strings.TrimPrefix(line, "ID_LIKE="), `"`)
		}
	}

	combined := id + " " + idLike
	switch {
	case strings.Contains(combined, "ubuntu") || strings.Contains(combined, "debian"):
		return DistroUbuntu
	case strings.Contains(combined, "fedora") || strings.Contains(combined, "rhel"):
		return DistroFedora
	}
	return DistroUnknown
}

// wlClipboardInstallURL returns the install reference URL for the
// wl-clipboard package, preferring the native distro package page when
// the distro is recognised.
func wlClipboardInstallURL() string {
	switch DetectDistro() {
	case DistroUbuntu:
		return "https://launchpad.net/ubuntu/+source/wl-clipboard"
	case DistroFedora:
		return "https://packages.fedoraproject.org/pkgs/wl-clipboard/wl-clipboard/"
	}
	return "https://github.com/bugaevc/wl-clipboard"
}
