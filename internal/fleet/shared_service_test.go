package fleet

import (
	"reflect"
	"testing"
)

func TestSharedServiceEnabled(t *testing.T) {
	all := FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true}
	tests := []struct {
		name     string
		settings FleetSettings
		want     []SharedService
	}{
		{"none", FleetSettings{}, nil},
		{"buildkit", FleetSettings{BuildkitServer: true}, []SharedService{ServiceBuildkit}},
		{"deb cache", FleetSettings{DebCacheServer: true}, []SharedService{ServiceDebCache}},
		{"image cache", FleetSettings{ImageCacheServer: true}, []SharedService{ServiceImageCache}},
		{"all in bring-up order", all, []SharedService{ServiceBuildkit, ServiceDebCache, ServiceImageCache}},
		{"the caches", FleetSettings{ImageCacheServer: true, DebCacheServer: true}, []SharedService{ServiceDebCache, ServiceImageCache}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.settings.EnabledSharedServices(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("EnabledSharedServices() = %v, want %v", got, tt.want)
			}
		})
	}
	if all.SharedServiceEnabled("some-future-service") {
		t.Error("an unknown service reads as enabled")
	}
}

// TestEverySharedServiceHasASetting: each listed service is switched by its own
// toggle, so adding a service without a FleetSettings field (or a case in
// SharedServiceEnabled) fails here.
func TestEverySharedServiceHasASetting(t *testing.T) {
	all := FleetSettings{BuildkitServer: true, DebCacheServer: true, ImageCacheServer: true}
	seen := map[SharedService]bool{}
	for _, svc := range SharedServices() {
		if seen[svc] {
			t.Errorf("%q listed twice", svc)
		}
		seen[svc] = true
		if !all.SharedServiceEnabled(svc) {
			t.Errorf("%q has no FleetSettings toggle", svc)
		}
		if (FleetSettings{}).SharedServiceEnabled(svc) {
			t.Errorf("%q is on in zero settings", svc)
		}
	}
}
