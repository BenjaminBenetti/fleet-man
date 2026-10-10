package state

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
)

// fillValue sets every field reachable from v to a distinct non-zero value,
// so a round trip that drops any field, now or after a field is added, fails.
func fillValue(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("v-" + path)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		if v.Type() == reflect.TypeOf(time.Duration(0)) {
			v.SetInt(int64(time.Minute))
			return
		}
		v.SetInt(int64(len(path)) + 1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillValue(t, p.Elem(), path)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillValue(t, s.Index(0), path+"[0]")
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillValue(t, k, path+".key")
		e := reflect.New(v.Type().Elem()).Elem()
		fillValue(t, e, path+".value")
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 10, 10, 1, 2, 3, int(len(path)), time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fillValue(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	default:
		t.Fatalf("fillValue: unhandled kind %s at %s", v.Kind(), path)
	}
}

func fullKubeCluster(t *testing.T, id string) fleet.KubeCluster {
	t.Helper()
	var c fleet.KubeCluster
	fillValue(t, reflect.ValueOf(&c).Elem(), "c")
	c.ID = id
	return c
}

// Every field of a record, the gateway credential included, survives the
// file, which is written 0600.
func TestKubeClustersRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, b := fullKubeCluster(t, "minikube-abc234"), fullKubeCluster(t, "prod-eu-xyz234")
	if a.Image.GatewayCredential == "" {
		t.Fatal("fillValue left the gateway credential empty")
	}
	err := UpdateKubeClusters(func(k *KubeClusters) error {
		k.Clusters = append(k.Clusters, a, b)
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateKubeClusters: %v", err)
	}
	info, err := os.Stat(KubeClustersPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("kube_clusters.json mode = %o, want 0600", perm)
	}
	got, err := LoadKubeClusters()
	if err != nil {
		t.Fatalf("LoadKubeClusters: %v", err)
	}
	if got.V != KubeClustersVersion {
		t.Errorf("V = %d, want %d", got.V, KubeClustersVersion)
	}
	if !reflect.DeepEqual(got.Clusters, []fleet.KubeCluster{a, b}) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got.Clusters, []fleet.KubeCluster{a, b})
	}
	data, _ := os.ReadFile(KubeClustersPath())
	if !strings.Contains(string(data), `"v": 1`) || !strings.Contains(string(data), `"gateway_credential"`) {
		t.Errorf("file lacks the version or the credential:\n%s", data)
	}
}

func TestLoadKubeClustersMissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	k, err := LoadKubeClusters()
	if err != nil || len(k.Clusters) != 0 || k.V != KubeClustersVersion {
		t.Fatalf("LoadKubeClusters = %+v, %v; want an empty v%d registry", k, err, KubeClustersVersion)
	}
}

func TestUpdateKubeClustersRefusesBadIDs(t *testing.T) {
	for name, clusters := range map[string][]fleet.KubeCluster{
		"invalid":   {{ID: "Not/Valid"}},
		"empty":     {{ID: ""}},
		"reserved":  {{ID: "jobs"}},
		"duplicate": {{ID: "a-abc234"}, {ID: "a-abc234"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			err := UpdateKubeClusters(func(k *KubeClusters) error {
				k.Clusters = clusters
				return nil
			})
			if err == nil {
				t.Fatal("UpdateKubeClusters accepted the records")
			}
			if _, err := os.Stat(KubeClustersPath()); !os.IsNotExist(err) {
				t.Errorf("a refused update wrote the file: %v", err)
			}
		})
	}
}

func TestUpdateKubeClustersKeepsFileOnCallbackError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	add := func(id string) error {
		return UpdateKubeClusters(func(k *KubeClusters) error {
			k.Clusters = append(k.Clusters, fleet.KubeCluster{ID: id})
			return nil
		})
	}
	if err := add("a-abc234"); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("nope")
	err := UpdateKubeClusters(func(k *KubeClusters) error {
		k.Clusters = nil
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err = %v, want the callback's error verbatim", err)
	}
	k, _ := LoadKubeClusters()
	if len(k.Clusters) != 1 {
		t.Errorf("a failed callback changed the file: %+v", k.Clusters)
	}
}

// A file from a newer fleet is readable but never overwritten.
func TestUpdateKubeClustersRefusesNewerFormat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(FleetDir(), 0700); err != nil {
		t.Fatal(err)
	}
	newer := `{"v": 2, "clusters": [{"id": "a-abc234", "context": "ctx", "namespace": "ns", "future_knob": true}]}`
	if err := os.WriteFile(KubeClustersPath(), []byte(newer), 0600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKubeClusters()
	if err != nil || k.V != 2 || len(k.Clusters) != 1 || k.Clusters[0].Context != "ctx" {
		t.Fatalf("LoadKubeClusters = %+v, %v", k, err)
	}
	called := false
	err = UpdateKubeClusters(func(*KubeClusters) error { called = true; return nil })
	if !errors.Is(err, ErrKubeClustersNewer) || called {
		t.Fatalf("UpdateKubeClusters = %v (callback ran: %v), want ErrKubeClustersNewer before the callback", err, called)
	}
	if data, _ := os.ReadFile(KubeClustersPath()); string(data) != newer {
		t.Errorf("the newer file was rewritten:\n%s", data)
	}
}

// Concurrent updates in the daemon never lose one another.
func TestUpdateKubeClustersConcurrent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var wg sync.WaitGroup
	const n = 16
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := UpdateKubeClusters(func(k *KubeClusters) error {
				k.Clusters = append(k.Clusters, fleet.KubeCluster{ID: fmt.Sprintf("c%d-abc234", i)})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	k, _ := LoadKubeClusters()
	if len(k.Clusters) != n {
		t.Fatalf("%d records after %d concurrent adds", len(k.Clusters), n)
	}
}

func TestKubeClustersFindRemove(t *testing.T) {
	k := &KubeClusters{Clusters: []fleet.KubeCluster{{ID: "a-abc234"}, {ID: "b-abc234"}}}
	if c := k.Find("b-abc234"); c == nil || c.ID != "b-abc234" {
		t.Fatalf("Find = %+v", c)
	}
	k.Find("a-abc234").Context = "edited"
	if k.Clusters[0].Context != "edited" {
		t.Error("Find does not alias the registry")
	}
	if k.Find("zzz") != nil {
		t.Error("Find of a missing id is not nil")
	}
	if !k.Remove("a-abc234") || k.Remove("a-abc234") || len(k.Clusters) != 1 || k.Clusters[0].ID != "b-abc234" {
		t.Errorf("Remove left %+v", k.Clusters)
	}
}

func TestKubePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fleetDir := home + "/.fleet"
	for got, want := range map[string]string{
		KubeClustersPath():                fleetDir + "/kube_clusters.json",
		DaemonIDPath():                    fleetDir + "/daemon_id",
		KubeIncarnationPath():             fleetDir + "/kube/incarnation.json",
		KubeFootprintPath():               fleetDir + "/kube/footprint.json",
		KubeJobPath("abcd2345"):           fleetDir + "/kube/jobs/abcd2345.json",
		KubeImagesPath("minikube-abc234"): fleetDir + "/kube/minikube-abc234/images.json",
		KubeImagesPath("../../etc"):       fleetDir + "/kube/.._.._etc/images.json",
		KubeJobPath("../x"):               fleetDir + "/kube/jobs/.._x.json",
	} {
		if got != want {
			t.Errorf("path = %s, want %s", got, want)
		}
	}
}
