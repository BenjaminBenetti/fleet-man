package binstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullBinaryHostAndCache(t *testing.T) {
	ctx := context.Background()
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			good := program(t, "linux", arch, false, "v9.9.9")
			s := testStore(t, Identity{Version: "v9.9.9"}, nil)
			s.SelfExe = good
			a, err := s.FullBinary(ctx, arch)
			if err != nil || a.Source != SourceHost {
				t.Fatalf("compatible host: %+v %v", a, err)
			}
			for name, host := range map[string]string{
				"mac":        program(t, "darwin", "arm64", false, "v9.9.9"),
				"pod":        program(t, "linux", arch, true, "v9.9.9"),
				"old":        program(t, "linux", arch, false, "v9.9.8"),
				"unreadable": filepath.Join(t.TempDir(), "missing"),
			} {
				t.Run(name, func(t *testing.T) {
					s.SelfExe = host
					copyTo(t, good, filepath.Join(s.Dir, FullBinaryAsset(arch)))
					a, err := s.FullBinary(ctx, arch)
					if err != nil || a.Source != SourceCache || a.Info.GOARCH != arch || a.Kind != KindFullBinary {
						t.Fatalf("fallback: %+v %v", a, err)
					}
				})
			}
		})
	}
	s := testStore(t, Identity{Version: "v9.9.9"}, nil)
	s.SelfExe = program(t, "linux", "amd64", false, "v9.9.9")
	copyTo(t, program(t, "linux", "arm64", false, "v9.9.9"), filepath.Join(s.Dir, FullBinaryAsset("arm64")))
	if a, err := s.FullBinary(ctx, "arm64"); err != nil || a.Source != SourceCache {
		t.Fatalf("foreign CPU host: %+v %v", a, err)
	}
	for _, arch := range []string{"", "../amd64", "riscv64"} {
		if _, err := s.FullBinary(ctx, arch); err == nil {
			t.Fatalf("accepted arch %q", arch)
		}
	}
}

func TestFullBinaryRejectsWrongOverride(t *testing.T) {
	env := map[string]string{}
	s := testStore(t, Identity{Version: "v9.9.9"}, env)
	copyTo(t, program(t, "linux", "amd64", false, "v9.9.9"), filepath.Join(s.Dir, FullBinaryAsset("amd64")))
	for name, path := range map[string]string{
		"pod":     program(t, "linux", "amd64", true, "v9.9.9"),
		"arch":    program(t, "linux", "arm64", false, "v9.9.9"),
		"mac":     program(t, "darwin", "arm64", false, "v9.9.9"),
		"version": program(t, "linux", "amd64", false, "v9.9.8"),
		"missing": filepath.Join(t.TempDir(), "missing"),
	} {
		env[EnvFullBinaryPrefix+"AMD64"] = path
		if _, err := s.FullBinary(context.Background(), "amd64"); err == nil || !strings.Contains(err.Error(), EnvFullBinaryPrefix) {
			t.Fatalf("%s override silently skipped: %v", name, err)
		}
	}
}

func TestFullBinaryDevRequiresExplicitUnverifiableBuild(t *testing.T) {
	bin := program(t, "linux", "amd64", false, "")
	env := map[string]string{}
	s := testStore(t, Identity{}, env)
	// Even with release access configured, an unstamped build never downloads.
	srv, hits := releaseServer(t, "", nil, nil)
	s.ReleaseBase = srv.URL + "/"
	copyTo(t, bin, filepath.Join(s.Dir, FullBinaryAsset("amd64")))
	if _, err := s.FullBinary(context.Background(), "amd64"); err == nil || !strings.Contains(err.Error(), "unverifiable dev identity") || !strings.Contains(err.Error(), "a dev build has no release") {
		t.Fatalf("unknown dev cache: %v", err)
	}
	hits.Range(func(k, v any) bool { t.Errorf("dev build requested %v", k); return true })
	env[EnvFullBinaryPrefix+"AMD64"] = bin
	if a, err := s.FullBinary(context.Background(), "amd64"); err != nil || a.Source != SourceEnv {
		t.Fatalf("explicit dev cross-build: %+v %v", a, err)
	}
	s.Self = Identity{Revision: "another-commit"}
	if _, err := s.FullBinary(context.Background(), "amd64"); err == nil {
		t.Fatal("accepted different dev revision")
	}
	s.Self = Identity{}
	s.SelfExe = bin
	if a, err := s.FullBinary(context.Background(), "amd64"); err != nil || a.Source != SourceHost {
		t.Fatalf("dev host fast path: %+v %v", a, err)
	}
}

func TestFullBinaryReleaseUpgradeAndOfflineCache(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			asset := FullBinaryAsset(arch)
			bin := program(t, "linux", arch, false, "v9.9.9")
			data, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			srv, hits := releaseServer(t, "v9.9.9", map[string][]byte{asset: data}, map[string]string{asset: shaOf(t, bin)})
			s := testStore(t, Identity{Version: "v9.9.9"}, nil)
			s.SelfExe = program(t, "darwin", "arm64", false, "v9.9.9")
			s.ReleaseBase = srv.URL + "/"
			copyTo(t, program(t, "linux", arch, false, "v9.9.8"), filepath.Join(s.Dir, asset))
			a, err := s.FullBinary(context.Background(), arch)
			if err != nil || a.Source != SourceRelease || a.Info.Version != "v9.9.9" {
				t.Fatalf("upgrade: %+v %v", a, err)
			}
			r, err := a.Open()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, r); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			srv.Close()
			// A new daemon can reuse the cache without a network request.
			restart := testStore(t, s.Self, nil)
			restart.Dir = s.Dir
			restart.ReleaseBase = s.ReleaseBase
			if a, err := restart.FullBinary(context.Background(), arch); err != nil || a.Source != SourceCache {
				t.Fatalf("offline: %+v %v", a, err)
			}
			if n, _ := hits.Load(asset); *n.(*int) != 1 {
				t.Fatalf("downloaded %v times", n)
			}
		})
	}
}

func TestFullBinaryReleaseFailures(t *testing.T) {
	asset := FullBinaryAsset("amd64")
	for _, failure := range []string{"checksum", "wrong version", "pod", "missing asset", "unpublished"} {
		t.Run(failure, func(t *testing.T) {
			bin := program(t, "linux", "amd64", failure == "pod", "v9.9.9")
			if failure == "wrong version" {
				bin = program(t, "linux", "amd64", false, "v9.9.8")
			}
			data, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			assets := map[string][]byte{asset: data}
			sums := map[string]string{asset: shaOf(t, bin)}
			if failure == "checksum" {
				sums[asset] = strings.Repeat("0", 64)
			}
			if failure == "missing asset" {
				delete(sums, asset)
			}
			srv, hits := releaseServer(t, "v9.9.9", assets, sums)
			s := testStore(t, Identity{Version: "v9.9.9"}, nil)
			s.ReleaseBase = srv.URL + "/"
			if failure == "unpublished" {
				s.Self.Version = "v0.0.0-local"
			}
			for range 2 {
				if _, err := s.FullBinary(context.Background(), "amd64"); err == nil {
					t.Fatal("accepted bad release")
				}
			}
			if _, err := os.Stat(filepath.Join(s.Dir, asset)); !os.IsNotExist(err) {
				t.Fatalf("kept invalid download: %v", err)
			}
			if n, ok := hits.Load(asset); ok && *n.(*int) != 1 {
				t.Fatalf("retried failure immediately: %s", fmt.Sprint(n))
			}
		})
	}
}
