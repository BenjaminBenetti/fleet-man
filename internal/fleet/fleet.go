package fleet

import "fmt"

// Fleet groups a set of related Instances that share the same remote
// repository and per-fleet settings.
type Fleet struct {
	Name      string        `json:"name"`
	Remote    string        `json:"remote"`
	Settings  FleetSettings `json:"settings,omitzero"`
	Instances []*Instance   `json:"instances"`
}

// GetInstance returns the named instance from the fleet, or an error
// when no instance with that name exists.
func (f *Fleet) GetInstance(name string) (*Instance, error) {
	for _, instance := range f.Instances {
		if instance.Name == name {
			return instance, nil
		}
	}
	return nil, fmt.Errorf("instance %q not found in fleet %q", name, f.Name)
}

// AddInstance appends instance to the fleet, returning an error if an
// instance with the same name already exists.
func (f *Fleet) AddInstance(instance *Instance) error {
	for _, existing := range f.Instances {
		if existing.Name == instance.Name {
			return fmt.Errorf("instance %q already exists in fleet %q", instance.Name, f.Name)
		}
	}
	f.Instances = append(f.Instances, instance)
	return nil
}

// RemoveInstance deletes the named instance from the fleet, returning
// an error when no such instance exists.
func (f *Fleet) RemoveInstance(name string) error {
	for i, instance := range f.Instances {
		if instance.Name == name {
			f.Instances = append(f.Instances[:i], f.Instances[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("instance %q not found in fleet %q", name, f.Name)
}
