package cli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/BenjaminBenetti/fleet-man/internal/create"
	"github.com/BenjaminBenetti/fleet-man/internal/fleet"
	"github.com/BenjaminBenetti/fleet-man/internal/state"
	"github.com/spf13/cobra"
)

// newCloneCmd returns the `fleet clone` command which duplicates an
// existing instance, preserving manually-installed software inside the
// source container. Only backends that report SupportsClone == true
// can be cloned; the rest fail fast inside create.RunClone.
func newCloneCmd() *cobra.Command {
	var repoFlag string

	cmd := &cobra.Command{
		Use:   "clone <source> <destination>",
		Short: "Clone an existing instance into a new one",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcName := args[0]
			destName := args[1]

			srcTarget, err := fleet.Resolve(srcName, repoFlag)
			if err != nil {
				return err
			}

			st, err := state.Load()
			if err != nil {
				return err
			}

			f, ok := st.Fleets[srcTarget.Fleet]
			if !ok {
				return fmt.Errorf("fleet %q not found", srcTarget.Fleet)
			}
			src, err := f.GetInstance(srcTarget.Instance)
			if err != nil {
				return err
			}
			if _, err := f.GetInstance(destName); err == nil {
				return fmt.Errorf("instance %s/%s already exists", srcTarget.Fleet, destName)
			}

			destWorkspaceDir := filepath.Join(state.WorkspacesDir(), srcTarget.Fleet, destName, srcTarget.Fleet)
			instance := &fleet.Instance{
				Name:         destName,
				DisplayName:  destName,
				Config:       src.Config,
				WorkspaceDir: destWorkspaceDir,
				CreatedAt:    time.Now(),
				Status:       fleet.StatusCloning,
				Backend:      src.Backend,
				Tag:          src.Tag,
				Color:        src.Color,
				Branch:       src.Branch,
			}
			if err := f.AddInstance(instance); err != nil {
				return err
			}
			if err := state.Save(st); err != nil {
				return err
			}

			fmt.Printf("Cloning %s/%s -> %s/%s (backend: %s)...\n",
				srcTarget.Fleet, srcTarget.Instance, srcTarget.Fleet, destName, src.Backend)
			if err := create.RunClone(srcTarget.Fleet, srcTarget.Instance, destName, true); err != nil {
				return err
			}

			st, err = state.Load()
			if err != nil {
				return err
			}
			if f, ok := st.Fleets[srcTarget.Fleet]; ok {
				if cloned, err := f.GetInstance(destName); err == nil && cloned.ContainerID != "" {
					fmt.Printf("Instance %s/%s is running (container: %s)\n",
						srcTarget.Fleet, destName, cloned.ContainerID[:min(12, len(cloned.ContainerID))])
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoFlag, "repo", "", "Git remote URL identifying the fleet (defaults to the source's fleet)")
	return cmd
}
