package main

import (
	"context"
	"fmt"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

type systemdLauncher struct {
	ctx  context.Context
	conn *dbus.Conn
}

func newSystemdLauncher(ctx context.Context) (*systemdLauncher, error) {
	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to systemd: %w", err)
	}

	return &systemdLauncher{ctx: ctx, conn: conn}, nil
}

type launchArgs struct {
	PodName      string
	PodNamespace string
	PodUID       string
	PodIP        string
	PodConfig    string
	CfgDir       string
	NetNS        string
}

func (s *systemdLauncher) Launch(containerID string, args launchArgs) error {
	ch := make(chan string)

	unit := unitName(containerID)

	properties := []dbus.Property{
		dbusProperty("Description", fmt.Sprintf("nebula-cni-instance: %s: %s", args.PodNamespace, args.PodName)),
		dbusProperty("Type", "notify"),
		dbusProperty("NotifyAccess", "main"),
		dbusProperty("Restart", "on-failure"),
		dbusProperty("SyslogIdentifier", "nebula-cni-instance"),
		dbusProperty("NetworkNamespacePath", args.NetNS),
		dbusProperty("WorkingDirectory", args.CfgDir),
		dbusProperty("Environment", []string{
			fmt.Sprintf("POD_NAME=%s", args.PodName),
			fmt.Sprintf("POD_NAMESPACE=%s", args.PodNamespace),
			fmt.Sprintf("POD_UID=%s", args.PodUID),
			fmt.Sprintf("POD_IP=%s", args.PodIP),
			fmt.Sprintf("NEBULA_CONFIG=%s", args.PodConfig),
		}),

		// For the boolean here, `true` means "ignore-failure", even though
		// go-systemd calls the arg "uncleanIsFailure":
		// -  https://github.com/systemd/systemd/commit/1072d9473123ed174e033704fc5222216b655c9e#diff-06b02e516a4e9be79228af7e8885140d31721867811bbfedca5e34253cc59375R1184
		dbus.PropExecStart([]string{
			"/opt/cni/bin/nebula-cni-instance",
			// We set these as command line flags so we can do CPU monitoring based on the process command line
			"-n", args.PodNamespace,
			"-p", args.PodName,

			"-container-id", containerID,
			"-netns", args.NetNS,
			"-pod-uid", args.PodUID,
			"-pod-ip", args.PodIP,
		}, false),
	}

	_, err := s.conn.StartTransientUnitContext(s.ctx, unit, "replace", properties, ch)
	if err != nil {
		return fmt.Errorf("failed to start nebula-cni-instance unit: %s: %w", unit, err)
	}

	startResult := <-ch
	if startResult != "done" {
		return fmt.Errorf("failed to start nebula-cni-instance unit: %s: %s", unit, startResult)
	}

	return nil
}

func (s *systemdLauncher) Stop(containerID string) error {
	unit := unitName(containerID)
	ch := make(chan string)

	_, err := s.conn.StopUnitContext(s.ctx, unit, "replace", ch)
	if err != nil {
		return fmt.Errorf("failed to stop systemd unit: %s: %w", unit, err)
	}
	stopResult := <-ch

	if stopResult != "done" {
		return fmt.Errorf("unexpected result from systemd: %s", stopResult)
	}
	return nil
}

func (s *systemdLauncher) Exists(containerID string) (bool, error) {
	unit := unitName(containerID)
	prop, err := s.conn.GetUnitPropertyContext(s.ctx, unit, "LoadState")
	if err != nil {
		return false, err
	}

	return prop.Value.String() == "loaded", nil
}

func (s *systemdLauncher) Close() {
	s.conn.Close()
}

func dbusProperty(name string, v any) dbus.Property {
	return dbus.Property{
		Name:  name,
		Value: godbus.MakeVariant(v),
	}
}

func unitName(containerID string) string {
	return fmt.Sprintf("nebula-cni-instance@%s.service", containerID)
}
