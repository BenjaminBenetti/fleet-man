package imageserve_test

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenjaminBenetti/fleet-man/internal/gateway"
	"github.com/BenjaminBenetti/fleet-man/internal/k8s/imageserve"
)

// conformance_test.go runs the OCI distribution-spec conformance suite's
// pull workflow against the image server, through an in-process gateway and
// fleetd's tunnel client. The suite is a separate test binary:
//
//	git clone --depth 1 --branch v1.1.1 https://github.com/opencontainers/distribution-spec
//	(cd distribution-spec/conformance && go test -c -o /tmp/oci-conformance.test .)
//	FLEET_OCI_CONFORMANCE=/tmp/oci-conformance.test go test -run Conformance -v ./internal/k8s/imageserve/
//
// It runs read-only: the suite skips its push setup only when OCI_TAG_NAME,
// OCI_MANIFEST_DIGEST and OCI_BLOB_DIGEST are all set, and the image server
// serves no tags, so the "tag" is the manifest's digest (the suite's tag
// specs then pull by digest too).

func TestOCIConformancePull(t *testing.T) {
	bin := os.Getenv("FLEET_OCI_CONFORMANCE")
	if bin == "" {
		t.Skip("set FLEET_OCI_CONFORMANCE to the distribution-spec conformance test binary")
	}
	t.Setenv("HOME", t.TempDir())
	publicAddr, grpcAddr, _ := startGateway(t, gateway.Config{PublicURL: "https://gw.example.com", SessionKey: "conformance"}, nil, nil)
	f := startFleetd(t, fleetdOptions{dial: registerDial(grpcAddr, nil), gatewayURL: "https://gw.example.com"})
	prefix := f.waitPrefix(t)
	a := imageserve.NewTestArchive(t, "amd64", map[string]string{"etc/conformance": "pull"})
	_, user, pass := f.deliver(t, "kc-1", a)
	// The route is live once fleetd answers (its challenge) rather than the
	// gateway's 404.
	rid := strings.TrimPrefix(prefix, "gw.example.com/")
	get(t, "HEAD", "http://"+publicAddr+"/v2/"+rid+"/devcontainer/manifests/"+a.Manifest, user, pass)

	report := t.TempDir()
	cmd := exec.Command(bin, "-test.v", "-ginkgo.no-color")
	cmd.Dir = report
	cmd.Env = append(os.Environ(),
		"OCI_ROOT_URL=http://"+publicAddr,
		"OCI_NAMESPACE="+rid+"/devcontainer",
		"OCI_USERNAME="+user,
		"OCI_PASSWORD="+pass,
		"OCI_TEST_PULL=1",
		"OCI_TEST_PUSH=0",
		"OCI_TEST_CONTENT_DISCOVERY=0",
		"OCI_TEST_CONTENT_MANAGEMENT=0",
		"OCI_MANIFEST_DIGEST="+a.Manifest,
		"OCI_BLOB_DIGEST="+a.Config,
		"OCI_TAG_NAME="+a.Manifest,
		"OCI_HIDE_SKIPPED_WORKFLOWS=1",
		"OCI_REPORT_DIR="+report,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("conformance output:\n%s", tail(string(out), 4000))
	if err != nil {
		t.Fatalf("the conformance suite failed: %v", err)
	}
	junit, rerr := os.ReadFile(filepath.Join(report, "junit.xml"))
	if rerr != nil {
		t.Fatalf("no junit report: %v", rerr)
	}
	var suites struct {
		Suites []struct {
			Tests    int `xml:"tests,attr"`
			Failures int `xml:"failures,attr"`
			Errors   int `xml:"errors,attr"`
			Skipped  int `xml:"skipped,attr"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(junit, &suites); err != nil || len(suites.Suites) == 0 {
		t.Fatalf("unreadable junit report: %v", err)
	}
	s := suites.Suites[0]
	if s.Failures != 0 || s.Errors != 0 || s.Tests-s.Skipped < 12 {
		t.Fatalf("conformance: %d tests, %d failures, %d errors, %d skipped", s.Tests, s.Failures, s.Errors, s.Skipped)
	}
	t.Logf("conformance pull workflow: %d of %d specs passed, %d skipped (push, discovery, management)", s.Tests-s.Skipped, s.Tests, s.Skipped)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
