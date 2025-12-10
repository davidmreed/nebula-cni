package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/slackhq/nebula-cni/internal"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func initLogger() {
	// Log to a file
	// TODO: make configurable / send to journald
	file, err := os.OpenFile("/var/log/nebula-cni.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}
	logger := slog.New(slog.NewTextHandler(file, nil))
	slog.SetDefault(logger)
}

type NetConf struct {
	types.NetConf

	// Optional: Should point to a .kubeconfig file with credentials to GET
	// pod details. If not set, we fallback to inClusterConfig. If
	// inClusterConfig fails, we fallback to the default config.
	KubeConfig string `json:"kubeconfig,omitempty"`
}

// Based on: https://github.com/aws/amazon-vpc-cni-k8s/blob/v1.13.4/cmd/routed-eni-cni-plugin/cni.go#L74-L86
// K8sArgs is the valid CNI_ARGS used for Kubernetes
type K8sArgs struct {
	types.CommonArgs

	// K8S_POD_NAME is pod's name
	K8S_POD_NAME types.UnmarshallableString

	// K8S_POD_NAMESPACE is pod's namespace
	K8S_POD_NAMESPACE types.UnmarshallableString

	// K8S_POD_INFRA_CONTAINER_ID is pod's sandbox id
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString

	K8S_POD_UID types.UnmarshallableString
}

func loadConf(args *skel.CmdArgs) (*NetConf, *K8sArgs, error) {
	var n NetConf
	var k8sArgs K8sArgs
	if err := json.Unmarshal(args.StdinData, &n); err != nil {
		return nil, nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	if args.Args != "" {
		err := types.LoadArgs(args.Args, &k8sArgs)
		if err != nil {
			return nil, nil, err
		}
	}

	return &n, &k8sArgs, nil
}

func getK8sClient(kubeConfig string) (kubernetes.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("clientcmd.BuildConfigFromFlags(%s) failed: %w", kubeConfig, err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig failed: %w", err)
	}

	return client, nil
}

// https://github.com/containernetworking/cni/blob/v1.2.0/SPEC.md#add-add-container-to-network-or-apply-modifications
func cmdAdd(args *skel.CmdArgs) (err error) {
	l := slog.With("cmd", "add")
	defer func() {
		if err != nil {
			l.Error("failed", "error", err)
		} else {
			l.Info("done")
		}
	}()

	logCalled(l, args)

	conf, k8sArgs, err := loadConf(args)
	if err != nil {
		return fmt.Errorf("failed to loadConf: %w", err)
	}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %w", err)
	}
	if conf.NetConf.RawPrevResult == nil {
		return fmt.Errorf("nebula-cni-plugin: Required prevResult missing")
	}
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return fmt.Errorf("version.ParsePrevResult failed: %w", err)
	}

	_, err = os.Stat(internal.ConfigDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			l.Info("cmdAdd: skipping due to no " + internal.ConfigDir)
			return types.PrintResult(conf.PrevResult, conf.CNIVersion)
		}

		return fmt.Errorf("failed to stat %s: %w", internal.ConfigDir, err)
	}

	result, err := current.NewResultFromResult(conf.PrevResult)
	if err != nil {
		return fmt.Errorf("current.NewResultFromResult failed: %w", err)
	}

	podNamespace := string(k8sArgs.K8S_POD_NAMESPACE)
	podName := string(k8sArgs.K8S_POD_NAME)
	podUID := string(k8sArgs.K8S_POD_UID)
	l = l.With("container_id", args.ContainerID, "pod_namespace", podNamespace, "pod_name", podName, "pod_uid", podUID)

	if conf.KubeConfig == "" {
		l.Info("skipping due to no kubeconfig", "pod_name", podName)
		return types.PrintResult(conf.PrevResult, conf.CNIVersion)
	}

	k8sClient, err := getK8sClient(conf.KubeConfig)
	if err != nil {
		return fmt.Errorf("getK8sClient failed: %w", err)
	}
	podSpec, err := k8sClient.CoreV1().Pods(podNamespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not get pod spec: %w", err)
	}
	annotations := podSpec.ObjectMeta.Annotations

	// check annotation to allow opt-out
	if annotations == nil ||
		annotations[internal.AnnotationPrefix+"/nebula.cni.enabled"] != "true" {
		l.Info("skipping due to no annotation", "pod_name", podName)
		return types.PrintResult(conf.PrevResult, conf.CNIVersion)
	}

	setPodIP := cmp.Or(annotations[internal.AnnotationPrefix+"/nebula.cni.set_pod_ip"], "true") == "true"

	var podIP string

	// Add the Nebula IP for the pod to the first item in the IPs list
	// This will tell kube to use this IP as the POD_IP
	for _, ip := range result.IPs {
		if v4 := ip.Address.IP.To4(); v4 != nil {
			podIP = v4.String()
			nebulaIP := internal.CalculateNebulaIP(podIP)
			if podIP != nebulaIP {
				if setPodIP {
					// TODO: configure podIP to be the Nebula IP
					ipc := &current.IPConfig{
						Interface: ip.Interface,
						Address:   net.IPNet{
							// IP: net.IP{...},
							// Mask: net.CIDRMask(...),
						},
						// Gateway: net.IP{...},
					}
					result.IPs = append([]*current.IPConfig{ipc}, result.IPs...)

					podIP = ipc.Address.IP.String()
				}

				break
			}
		}
	}

	// Finish early if we aren't doing the full stage
	if cmp.Or(annotations[internal.AnnotationPrefix+"/nebula.cni.stage"], "full") != "full" {
		l.Info("not running nebula-cni-instance")
		return types.PrintResult(result, conf.CNIVersion)
	}
	l.Info("running nebula-cni-instance")

	cfgDir := filepath.Join(internal.StateDir, args.ContainerID)
	err = os.MkdirAll(cfgDir, 0700)
	if err != nil {
		return fmt.Errorf("failed to create cni dir: %s: %w", cfgDir, err)
	}

	podConfig := annotations[internal.AnnotationPrefix+"/nebula.cni.config"]

	launcher, err := newSystemdLauncher(context.Background())
	if err != nil {
		return fmt.Errorf("failed to created launcher: %w", err)
	}
	defer launcher.Close()

	loaded, err := launcher.Exists(args.ContainerID)
	if err != nil {
		return fmt.Errorf("failed to check LoadState: %w", err)
	}

	if loaded {
		l.Info("nebula-cni-instance already exists")
		return types.PrintResult(result, conf.CNIVersion)
	}

	err = launcher.Launch(args.ContainerID, launchArgs{
		PodName:      podName,
		PodNamespace: podNamespace,
		PodUID:       podUID,
		PodIP:        podIP,
		PodConfig:    podConfig,
		CfgDir:       cfgDir,
		NetNS:        args.Netns,
	})

	if err != nil {
		return fmt.Errorf("failed to launch: %w", err)
	}

	l.Info("nebula-cni-instance launched")

	return types.PrintResult(result, conf.CNIVersion)
}

// https://github.com/containernetworking/cni/blob/v1.2.0/SPEC.md#check-check-containers-networking-is-as-expected
func cmdCheck(args *skel.CmdArgs) (err error) {
	l := slog.With("cmd", "check")
	defer func() {
		if err != nil {
			l.Error("failed", "error", err)
		} else {
			l.Info("done")
		}
	}()

	logCalled(l, args)

	cfgDir := filepath.Join(internal.StateDir, args.ContainerID)
	_, err = os.Stat(cfgDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Probably not full stage, so we have nothing to check
			return nil
		}
		return fmt.Errorf("error reading %s: %w", cfgDir, err)
	}

	launcher, err := newSystemdLauncher(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create launcher: %w", err)
	}
	defer launcher.Close()

	loaded, err := launcher.Exists(args.ContainerID)
	if err != nil {
		return fmt.Errorf("failed to check Exists: %w", err)
	}
	if !loaded {
		return fmt.Errorf("nebula-cni-instance is not running")
	}

	return nil
}

// https://github.com/containernetworking/cni/blob/v1.2.0/SPEC.md#del-remove-container-from-network-or-un-apply-modifications
func cmdDel(args *skel.CmdArgs) (err error) {
	l := slog.With("cmd", "del")
	defer func() {
		if err != nil {
			l.Error("failed", "error", err)
		} else {
			l.Info("done")
		}
	}()

	logCalled(l, args)

	cfgDir := filepath.Join(internal.StateDir, args.ContainerID)

	_, err = os.Stat(cfgDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Probably not full stage, so we have nothing to delete
			return nil
		}
		return fmt.Errorf("error reading %s: %w", cfgDir, err)
	}

	launcher, err := newSystemdLauncher(context.Background())
	if err != nil {
		return fmt.Errorf("failed to create launcher: %w", err)
	}
	defer launcher.Close()

	err = launcher.Stop(args.ContainerID)
	if err != nil {
		loaded, err2 := launcher.Exists(args.ContainerID)
		if err2 != nil {
			return fmt.Errorf("failed to check Exists: %w", err2)
		}
		if loaded {
			return fmt.Errorf("nebula-cni-instance failed to stop: %w", err)
		}

		l.Info("failed to stop because is no longer exists. continuing")
	} else {
		l.Info("nebula-cni-instance stopped")
	}

	// cleanup
	// TODO: Does the install specific code need to do any cleanup?

	err = os.RemoveAll(cfgDir)
	if err != nil {
		return fmt.Errorf("failed to rm dir: %s: %w", cfgDir, err)
	}

	return nil
}

// https://github.com/containernetworking/cni/blob/v1.2.0/SPEC.md#gc-clean-up-any-stale-resources
func cmdGC(args *skel.CmdArgs) (err error) {
	// TODO clean up any zombie nebula-cni-instance processes?
	return nil
}

// https://github.com/containernetworking/cni/blob/v1.2.0/SPEC.md#status-check-plugin-status
func cmdStatus(args *skel.CmdArgs) (err error) {
	return nil
}

// A version string that can be set with
//
//	-ldflags "-X main.Build=SOMEVERSION"
//
// at compile-time.
var Build string

func main() {
	if Build == "" {
		Build = internal.ModuleVersion()
	}
	printVersion := flag.Bool("version", false, "Print version")
	flag.Parse()

	if *printVersion {
		fmt.Printf("Version: %s\n", Build)
		return
	}

	initLogger()

	funcs := skel.CNIFuncs{
		Add:    cmdAdd,
		Check:  cmdCheck,
		Del:    cmdDel,
		GC:     cmdGC,
		Status: cmdStatus,
	}
	skel.PluginMainFuncs(funcs, version.All, bv.BuildString("nebula-cni"))
}

func logCalled(l *slog.Logger, args *skel.CmdArgs) {
	l.Info("called",
		"container_id", args.ContainerID,
		"netns", args.Netns,
		"if_name", args.IfName,
		"args", args.Args,
		"path", args.Path,
		"stdin_data", string(args.StdinData),
	)
}
