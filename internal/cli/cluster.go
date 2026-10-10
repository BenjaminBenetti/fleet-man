package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/BenjaminBenetti/fleet-man/fleetgrpc"
	"github.com/BenjaminBenetti/fleet-man/internal/fleetclient"
)

// cluster.go is `fleet cluster`: the daemon's Kubernetes cluster connections
// (kube.proto). Every cluster call runs in fleetd, so each subcommand is one
// kube RPC, and it works the same against a local daemon, through the gateway
// (FLEET_GATEWAY) or over Armada SSH (FLEET_SSH): kubeconfig and extra_path
// entries are paths on the DAEMON host, and are sent as typed, never resolved
// against this machine.

// dialCluster opens the connection the cluster commands talk over: the
// daemon fleetclient.Dial picks (local socket, gateway, Armada SSH or
// FLEET_SERVER). A package var so the command tests can hand in a fake
// service.
var dialCluster = func(ctx context.Context) (fleetgrpc.FleetServiceClient, func(), error) {
	conn, err := fleetclient.Dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn.Service(), func() { _ = conn.Close() }, nil
}

// errClusterTestFailed ends `fleet cluster test` with exit 1 once the result
// line is printed.
var errClusterTestFailed = errors.New("cluster test failed")

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage Kubernetes cluster connections",
		Long: "Manage fleetd's Kubernetes cluster connections: one kubeconfig context and\n" +
			"namespace each. Kubeconfig and extra_path paths are paths on the daemon host.",
		// Usage is for argument mistakes: a failed RPC prints just its error.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { cmd.SilenceUsage = true },
	}
	cmd.AddCommand(
		newClusterListCmd(),
		newClusterContextsCmd(),
		newClusterAddCmd(),
		newClusterSetCmd(),
		newClusterRemoveCmd(),
		newClusterTestCmd(),
		newClusterGCCmd(),
		newClusterLoginCmd(),
	)
	return cmd
}

// withCluster runs fn on a fresh connection to the daemon.
func withCluster(ctx context.Context, fn func(fleetgrpc.FleetServiceClient) error) error {
	svc, closeConn, err := dialCluster(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	return clusterErr(fn(svc))
}

// clusterErr is an RPC error as its message alone.
func clusterErr(err error) error {
	if err == nil || errors.Is(err, errClusterTestFailed) {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if st.Code() == codes.Unimplemented {
		return errors.New("this fleetd has no Kubernetes support (update it)")
	}
	return errors.New(st.Message())
}

// findCluster resolves a cluster argument: an id, a name, or a context one
// connection uses.
func findCluster(ctx context.Context, svc fleetgrpc.FleetServiceClient, ref string) (*fleetgrpc.KubeClusterStatus, error) {
	reply, err := svc.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
	if err != nil {
		return nil, err
	}
	return fleetgrpc.FindKubeCluster(reply.GetClusters(), ref)
}

// --- ls ---

func newClusterListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls [<cluster>]",
		Aliases: []string{"list"},
		Short:   "List cluster connections and their health",
		Long: "List cluster connections and their health. With a cluster, also print\n" +
			"its warnings and every field it sets, in the form `set` takes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				reply, err := svc.ListKubeClusters(cmd.Context(), &fleetgrpc.ListKubeClustersRequest{})
				if err != nil {
					return err
				}
				clusters := reply.GetClusters()
				if len(args) == 1 {
					st, err := fleetgrpc.FindKubeCluster(clusters, args[0])
					if err != nil {
						return err
					}
					clusters = []*fleetgrpc.KubeClusterStatus{st}
				}
				out := cmd.OutOrStdout()
				if asJSON {
					return printClustersJSON(out, clusters)
				}
				if len(args) == 0 && len(clusters) == 0 {
					fmt.Fprintln(out, "No cluster connections; add one with 'fleet cluster add <context>'.")
					return nil
				}
				printClusterTable(out, clusters)
				if len(args) == 1 {
					printClusterDetail(out, clusters[0])
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the connections and their health as JSON")
	return cmd
}

func printClusterTable(out io.Writer, clusters []*fleetgrpc.KubeClusterStatus) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tCONTEXT\tNAMESPACE\tSTATE\tDETAIL")
	for _, st := range clusters {
		c := st.GetCluster()
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.DisplayNameOrID(), c.GetId(), c.GetContext(), c.GetNamespace(), st.GetState().Display(), oneLine(st.GetMessage()))
	}
	_ = w.Flush()
}

func printClusterDetail(out io.Writer, st *fleetgrpc.KubeClusterStatus) {
	for _, warning := range st.GetWarnings() {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	fmt.Fprintln(out)
	for _, line := range clusterFieldLines(st.GetCluster()) {
		fmt.Fprintln(out, line)
	}
}

func printClustersJSON(out io.Writer, clusters []*fleetgrpc.KubeClusterStatus) error {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&fleetgrpc.KubeClusters{Clusters: clusters})
	if err != nil {
		return err
	}
	// protojson's spacing differs from build to build: indent it stably.
	var b bytes.Buffer
	if err := json.Indent(&b, data, "", "  "); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, b.String())
	return err
}

// oneLine is s with its line breaks folded, for a table cell.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// --- contexts ---

func newClusterContextsCmd() *cobra.Command {
	var kubeconfigs []string
	cmd := &cobra.Command{
		Use:   "contexts",
		Short: "List the kubeconfig contexts on the daemon host",
		Long: "List the kubeconfig contexts on the daemon host: --kubeconfig files there,\n" +
			"else kubectl's defaults in fleetd's environment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				reply, err := svc.ListKubeContexts(cmd.Context(), &fleetgrpc.ListKubeContextsRequest{KubeconfigPaths: kubeconfigs})
				if err != nil {
					return err
				}
				names := map[string]string{}
				if list, err := svc.ListKubeClusters(cmd.Context(), &fleetgrpc.ListKubeClustersRequest{}); err == nil {
					for _, st := range list.GetClusters() {
						names[st.GetCluster().GetId()] = st.GetCluster().DisplayNameOrID()
					}
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%s on fleetd @ %s\n", strings.Join(reply.GetKubeconfigPaths(), ":"), reply.GetHost())
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "CURRENT\tNAME\tCLUSTER\tNAMESPACE\tAUTH\tCONNECTED")
				for _, c := range reply.GetContexts() {
					current := ""
					if c.GetCurrent() {
						current = "*"
					}
					var connected []string
					for _, id := range c.GetConnected() {
						if name := names[id]; name != "" {
							id = name
						}
						connected = append(connected, id)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", current, c.GetName(), c.GetCluster(), c.GetNamespace(), contextAuth(c), strings.Join(connected, ","))
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().StringArrayVar(&kubeconfigs, "kubeconfig", nil, "kubeconfig `path` on the daemon host (repeatable, in precedence order)")
	return cmd
}

// contextAuth is how a context authenticates, with its exec plugin.
func contextAuth(c *fleetgrpc.KubeContext) string {
	if c.GetPlugin() == "" {
		return c.GetAuth()
	}
	auth := c.GetAuth() + ":" + c.GetPlugin()
	if c.GetPluginMissing() != "" {
		auth += " (missing)"
	}
	return auth
}

// --- add ---

func newClusterAddCmd() *cobra.Command {
	var (
		name, namespace string
		kubeconfigs     []string
		createNamespace bool
		namespaceLabels []string
	)
	cmd := &cobra.Command{
		Use:   "add <context> [<field>=<value>...]",
		Short: "Connect a kubeconfig context on the daemon host",
		Long: "Connect a kubeconfig context on the daemon host. Fields as for `set`.\n" +
			"fleetd checks the connection in the background; `test` shows the checks.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := parseClusterFields(args[1:])
			if err != nil {
				return err
			}
			if err := applyConnectionFlags(cmd, c, args[0], kubeconfigs, namespace); err != nil {
				return err
			}
			if cmd.Flags().Changed("name") {
				c.DisplayName = name
			}
			if len(namespaceLabels) > 0 && !createNamespace {
				return errors.New("--namespace-label needs --create-namespace")
			}
			labels, err := parseLabels(namespaceLabels)
			if err != nil {
				return err
			}
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				out := cmd.OutOrStdout()
				if c.GetDisplayName() == "" && !cmd.Flags().Changed("name") {
					c.DisplayName = defaultClusterName(cmd.Context(), svc, c)
				}
				if createNamespace {
					if err := createClusterNamespace(cmd.Context(), svc, out, c, labels); err != nil {
						return err
					}
				}
				reply, err := svc.AddKubeCluster(cmd.Context(), &fleetgrpc.AddKubeClusterRequest{Cluster: c})
				if err != nil {
					return err
				}
				added := reply.GetCluster().GetCluster()
				fmt.Fprintf(out, "Added cluster %q (%s): context %s, namespace %s; %s\n", added.DisplayNameOrID(), added.GetId(),
					added.GetContext(), added.GetNamespace(), reply.GetCluster().GetState().Display())
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "display name (default: the context's)")
	f.StringArrayVar(&kubeconfigs, "kubeconfig", nil, "kubeconfig `path` on the daemon host (repeatable; default: fleetd's)")
	f.StringVarP(&namespace, "namespace", "n", "", "namespace (default: the context's, else default)")
	f.BoolVar(&createNamespace, "create-namespace", false, "create the namespace first")
	f.StringArrayVar(&namespaceLabels, "namespace-label", nil, "`key=value` label for --create-namespace (repeatable)")
	return cmd
}

// applyConnectionFlags puts the context, --kubeconfig and --namespace on c,
// refusing the same setting given as a flag and as a field too.
func applyConnectionFlags(cmd *cobra.Command, c *fleetgrpc.KubeCluster, kubeContext string, kubeconfigs []string, namespace string) error {
	if c.GetContext() != "" {
		return errors.New("give the context once")
	}
	c.Context = kubeContext
	if len(kubeconfigs) > 0 {
		if len(c.GetKubeconfigPaths()) > 0 {
			return errors.New("give kubeconfig paths as --kubeconfig or kubeconfig_paths=, not both")
		}
		c.KubeconfigPaths = kubeconfigs
	}
	if cmd.Flags().Changed("namespace") {
		if c.GetNamespace() != "" {
			return errors.New("give the namespace as -n or namespace=, not both")
		}
		c.Namespace = namespace
	}
	return nil
}

// defaultClusterName names a new connection after its context, unless a
// connection already has that name or the context's name is no display name
// (then the daemon shows the id).
func defaultClusterName(ctx context.Context, svc fleetgrpc.FleetServiceClient, c *fleetgrpc.KubeCluster) string {
	name := c.GetContext()
	if len(name) > 63 || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return ""
	}
	reply, err := svc.ListKubeClusters(ctx, &fleetgrpc.ListKubeClustersRequest{})
	if err != nil {
		return name
	}
	taken := func(n string) bool {
		for _, st := range reply.GetClusters() {
			if strings.EqualFold(st.GetCluster().DisplayNameOrID(), n) {
				return true
			}
		}
		return false
	}
	if !taken(name) {
		return name
	}
	if ns := c.GetNamespace(); ns != "" && len(name)+1+len(ns) <= 63 && !taken(name+"-"+ns) {
		return name + "-" + ns
	}
	return ""
}

func parseLabels(labels []string) (map[string]string, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--namespace-label %q is not key=value", l)
		}
		m[k] = v
	}
	return m, nil
}

// createClusterNamespace creates the namespace c names (or its default)
// through fleetd before the connection is added; one that exists is fine.
func createClusterNamespace(ctx context.Context, svc fleetgrpc.FleetServiceClient, out io.Writer, c *fleetgrpc.KubeCluster, labels map[string]string) error {
	reply, err := svc.CreateKubeNamespace(ctx, &fleetgrpc.CreateKubeNamespaceRequest{Cluster: c, Labels: labels})
	switch {
	case status.Code(err) == codes.AlreadyExists:
		fmt.Fprintf(out, "%s\n", status.Convert(err).Message())
		return nil
	case err != nil:
		return err
	}
	if reply.GetCreated() {
		fmt.Fprintf(out, "Created namespace:\n%s", reply.GetManifest())
	}
	return nil
}

// --- set ---

const clusterSetLong = "Change a cluster connection's settings; other fields keep their values.\n" +
	"A sub-field is named by its path (image.mode=registry), a list or map\n" +
	"takes one item per field=value (extra_path=/a extra_path=/b,\n" +
	"node_selector=pool=fleet; tolerations=key[=value][:effect[:seconds]]),\n" +
	"and field= resets a field to its default. Fields (kube.proto):\n"

func newClusterSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <cluster> <field>=<value>...",
		Short: "Change a cluster connection's settings",
		Long:  clusterSetLong,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, paths, err := parseClusterFields(args[1:])
			if err != nil {
				return err
			}
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				st, err := findCluster(cmd.Context(), svc, args[0])
				if err != nil {
					return err
				}
				reply, err := svc.UpdateKubeCluster(cmd.Context(), &fleetgrpc.UpdateKubeClusterRequest{
					Id: st.GetCluster().GetId(), Cluster: c, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Updated cluster %q: %s\n", reply.GetCluster().GetCluster().DisplayNameOrID(), strings.Join(paths, ", "))
				return nil
			})
		},
	}
	// The field list is built only when help is shown, not on every start.
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		c.Long = clusterSetLong + clusterFieldHelp()
		c.Parent().HelpFunc()(c, args)
	})
	return cmd
}

// --- rm ---

func newClusterRemoveCmd() *cobra.Command {
	var destroy, forget bool
	cmd := &cobra.Command{
		Use:     "rm <cluster>",
		Aliases: []string{"remove"},
		Short:   "Disconnect a cluster connection",
		Long: "Disconnect a cluster connection. One that instances or agents use is kept\n" +
			"unless --destroy-instances destroys them first, or --forget leaves them in\n" +
			"the cluster (scaled to 0; their records fail as cluster_missing).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_REFUSE_IF_USED
			switch {
			case destroy && forget:
				return errors.New("--destroy-instances and --forget exclude each other")
			case destroy:
				mode = fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_DESTROY_INSTANCES
			case forget:
				mode = fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET
			}
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				st, err := findCluster(cmd.Context(), svc, args[0])
				if err != nil {
					return err
				}
				c := st.GetCluster()
				reply, err := svc.RemoveKubeCluster(cmd.Context(), &fleetgrpc.RemoveKubeClusterRequest{Id: c.GetId(), Mode: mode})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if !reply.GetRemoved() {
					return fmt.Errorf("cluster %q is in use:\n%s\nuse --destroy-instances to destroy its instances first, or --forget to leave them in the cluster",
						c.DisplayNameOrID(), describeClusterUsers(reply.GetUsers()))
				}
				for _, w := range reply.GetWarnings() {
					fmt.Fprintf(out, "warning: %s\n", w)
				}
				fmt.Fprintf(out, "Removed cluster %q\n", c.DisplayNameOrID())
				if users := reply.GetUsers(); len(users) > 0 {
					verb := "destroyed"
					if mode == fleetgrpc.KubeRemoveMode_KUBE_REMOVE_MODE_FORGET {
						verb = "left in namespace " + c.GetNamespace()
					}
					fmt.Fprintf(out, "%s:\n%s\n", verb, describeClusterUsers(users))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&destroy, "destroy-instances", false, "destroy the instances on it first")
	cmd.Flags().BoolVar(&forget, "forget", false, "leave its instances in the cluster")
	return cmd
}

func describeClusterUsers(users []*fleetgrpc.KubeClusterUser) string {
	lines := make([]string, len(users))
	for i, u := range users {
		if u.GetAgent() != "" {
			lines[i] = fmt.Sprintf("  %s: agent %s", u.GetFleet(), u.GetAgent())
			continue
		}
		lines[i] = fmt.Sprintf("  %s/%s (%s)", u.GetFleet(), u.GetInstance(), u.GetStatus().Display())
	}
	return strings.Join(lines, "\n")
}

// --- test ---

func newClusterTestCmd() *cobra.Command {
	var (
		kubeContext, namespace string
		kubeconfigs            []string
	)
	cmd := &cobra.Command{
		Use:   "test <cluster> | --context <context> [<field>=<value>...]",
		Short: "Run a cluster connection's checks on the daemon",
		Long: "Run every check of a cluster connection on the daemon and print them.\n" +
			"With --context, test a context that is not connected (fields as for\n" +
			"`set`). Exits 0 when the cluster is ready or limited, 1 otherwise.",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("context") {
				for _, a := range args {
					if !isClusterFieldArg(a) {
						return fmt.Errorf("%q is not <field>=<value>", a)
					}
				}
				return nil
			}
			if cmd.Flags().Changed("kubeconfig") || cmd.Flags().Changed("namespace") {
				return errors.New("--kubeconfig and -n go with --context")
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &fleetgrpc.TestKubeClusterRequest{}
			if cmd.Flags().Changed("context") {
				c, _, err := parseClusterFields(args)
				if err != nil {
					return err
				}
				if err := applyConnectionFlags(cmd, c, kubeContext, kubeconfigs, namespace); err != nil {
					return err
				}
				req.Cluster = c
			}
			err := withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				if req.Cluster == nil {
					st, err := findCluster(cmd.Context(), svc, args[0])
					if err != nil {
						return err
					}
					req.Id = st.GetCluster().GetId()
				}
				return runClusterTest(cmd.Context(), svc, req, cmd.OutOrStdout())
			})
			if errors.Is(err, errClusterTestFailed) {
				cmd.SilenceErrors = true
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&kubeContext, "context", "", "test this kubeconfig context instead of a connection")
	f.StringArrayVar(&kubeconfigs, "kubeconfig", nil, "kubeconfig `path` on the daemon host, with --context (repeatable)")
	f.StringVarP(&namespace, "namespace", "n", "", "namespace, with --context")
	return cmd
}

// runClusterTest streams a preflight, printing each check as it finishes,
// then the warnings and the resulting state.
func runClusterTest(ctx context.Context, svc fleetgrpc.FleetServiceClient, req *fleetgrpc.TestKubeClusterRequest, out io.Writer) error {
	stream, err := svc.TestKubeCluster(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%-23s %-7s %s\n", "CHECK", "RESULT", "DETAIL")
	var result *fleetgrpc.KubeClusterStatus
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if c := ev.GetCheck(); c != nil && c.GetResult() != fleetgrpc.KubeCheckResult_KUBE_CHECK_RESULT_RUNNING {
			printCheck(out, c)
		}
		if r := ev.GetResult(); r != nil {
			result = r
		}
	}
	if result == nil {
		return errors.New("fleetd ended the test without a result")
	}
	for _, w := range result.GetWarnings() {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	line := result.GetState().Display()
	if msg := oneLine(result.GetMessage()); msg != "" {
		line += ": " + msg
	}
	fmt.Fprintln(out, line)
	if !result.GetState().Usable() {
		return errClusterTestFailed
	}
	return nil
}

func printCheck(out io.Writer, c *fleetgrpc.KubeCheck) {
	detail := strings.Split(strings.TrimRight(c.GetDetail(), "\n"), "\n")
	row := fmt.Sprintf("%-23s %-7s %s", strings.ReplaceAll(c.GetName(), "_", " "), c.GetResult().Display(), detail[0])
	fmt.Fprintln(out, strings.TrimRight(row, " "))
	for _, more := range detail[1:] {
		fmt.Fprintf(out, "%32s%s\n", "", more)
	}
	if fix := oneLine(c.GetFix()); fix != "" {
		fmt.Fprintf(out, "%32sfix: %s\n", "", fix)
	}
}

// --- gc ---

func newClusterGCCmd() *cobra.Command {
	var (
		apply     bool
		fleetName string
	)
	cmd := &cobra.Command{
		Use:   "gc <cluster>",
		Short: "List, and with --apply delete, objects no instance accounts for",
		Long: "List the objects labelled for this daemon and connection that no instance\n" +
			"record accounts for; --apply deletes them while this daemon holds the\n" +
			"connection's owner Lease. Mounted volumes, objects younger than 1h and\n" +
			"those of an existing fleet are kept (--fleet names one to delete too).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				st, err := findCluster(cmd.Context(), svc, args[0])
				if err != nil {
					return err
				}
				reply, err := svc.PruneKubeCluster(cmd.Context(), &fleetgrpc.PruneKubeClusterRequest{Id: st.GetCluster().GetId(), Apply: apply, Fleet: fleetName})
				if err != nil {
					return err
				}
				printPrune(cmd.OutOrStdout(), reply, apply, time.Now())
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "delete what the listing marks")
	cmd.Flags().StringVar(&fleetName, "fleet", "", "also delete the objects of this existing fleet")
	return cmd
}

func printPrune(out io.Writer, reply *fleetgrpc.PruneKubeClusterReply, apply bool, now time.Time) {
	holder := "this daemon"
	if !reply.GetLeaseHeld() {
		holder = reply.GetLeaseHolder()
		if host := reply.GetLeaseHolderHost(); host != "" {
			holder += " on " + host
		}
		if holder == "" {
			holder = "another daemon"
		}
	}
	fmt.Fprintf(out, "owner %s · lease held by %s\n", reply.GetOwner(), holder)
	objects := reply.GetObjects()
	if len(objects) == 0 {
		fmt.Fprintln(out, "no objects to collect")
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tNAME\tAGE\tMOUNTED\tACTION")
	marked := 0
	for _, o := range objects {
		mounted := "-"
		if o.GetKind() == "pvc" {
			mounted = "no"
			if o.GetMounted() {
				mounted = "yes"
			}
		}
		age := "-"
		if o.GetCreatedAt() != nil {
			age = shortAge(now.Sub(o.GetCreatedAt().AsTime()))
		}
		if o.GetAction() == "would delete" {
			marked++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", o.GetKind(), o.GetName(), age, mounted, o.GetAction())
	}
	_ = w.Flush()
	switch {
	case apply:
		fmt.Fprintf(out, "%d deleted\n", reply.GetDeleted())
	case marked > 0 && !reply.GetLeaseHeld():
		fmt.Fprintf(out, "%d %s; --apply is refused while another daemon holds the owner Lease\n", len(objects), plural(len(objects), "object"))
	case marked > 0:
		fmt.Fprintf(out, "%d %s; run with --apply to delete\n", len(objects), plural(len(objects), "object"))
	default:
		fmt.Fprintf(out, "%d %s; none to delete\n", len(objects), plural(len(objects), "object"))
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// shortAge is d as kubectl prints an age: 45s, 20m, 3h, 9d.
func shortAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// --- login ---

func newClusterLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login <cluster>",
		Short: "Run a cluster connection's login plugin on the daemon host",
		Long: "Run a cluster connection's credential plugin interactively on the daemon\n" +
			"host, relaying only its device-code URL and code, then test the connection.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withCluster(cmd.Context(), func(svc fleetgrpc.FleetServiceClient) error {
				st, err := findCluster(cmd.Context(), svc, args[0])
				if err != nil {
					return err
				}
				stream, err := svc.LoginKubeCluster(cmd.Context(), &fleetgrpc.LoginKubeClusterRequest{Id: st.GetCluster().GetId()})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				for {
					ev, err := stream.Recv()
					if err == io.EOF {
						return errors.New("fleetd ended the login without a result")
					}
					if err != nil {
						return err
					}
					if dc := ev.GetDeviceCode(); dc != nil {
						fmt.Fprintf(out, "To log in, open %s and enter the code %s\n", dc.GetUrl(), dc.GetCode())
					}
					if r := ev.GetResult(); r != nil {
						line := r.GetState().Display()
						if msg := oneLine(r.GetMessage()); msg != "" {
							line += ": " + msg
						}
						fmt.Fprintf(out, "Logged in; cluster %q is %s\n", r.GetCluster().DisplayNameOrID(), line)
						return nil
					}
				}
			})
		},
	}
}
