package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula"
	"github.com/slackhq/nebula-cni/internal"
	"github.com/slackhq/nebula/cert"
	"github.com/slackhq/nebula/config"
	"github.com/slackhq/nebula/util"
)

// A version string that can be set with
//
//	-ldflags "-X main.Build=SOMEVERSION"
//
// at compile-time.
var Build string

var l = logrus.New()

func main() {
	if Build == "" {
		Build = internal.ModuleVersion()
	}
	os.Exit(_main())
}

func _main() int {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)

	// We set these as command line flags so we can do CPU monitoring based on the process command line
	flag.String("n", "", "Pod Namespace")
	podNameFlag := flag.String("p", "", "Pod Name")

	containerID := flag.String("container-id", "", "from CNI")
	netNS := flag.String("netns", "", "from CNI")
	podUID := flag.String("pod-uid", "", "POD_UID from Kubernetes")
	podIP := flag.String("pod-ip", "", "POD_IP from Kubernetes")
	printVersion := flag.Bool("version", false, "Print version")
	printUsage := flag.Bool("help", false, "Print command line usage")
	// We want to be able to add flags in the future, so we do not exit
	// when the user passes an unknown flag.
	fu := flag.CommandLine.Usage
	flag.CommandLine.Usage = func() {}
	flag.Parse()
	flag.CommandLine.Usage = fu

	if *printVersion {
		fmt.Printf("Version: %s\n", Build)
		return 0
	}

	if *printUsage {
		flag.CommandLine.Usage()
		return 0
	}

	for k, v := range map[string]*string{
		"-container-id": containerID,
		"-netns":        netNS,
		"-pod-uid":      podUID,
		"-pod-ip":       podIP,
	} {
		if *v == "" {
			fmt.Fprintf(os.Stderr, "%s flag must be set", k)
			flag.Usage()
			return 1
		}
	}

	l.SetFormatter(&logrus.JSONFormatter{})

	podName := cmp.Or(*podNameFlag, os.Getenv("POD_NAME"))

	nebulaIP := internal.CalculateNebulaIP(*podIP)

	logOut, err := NewLogrusOutHack(podName, *podIP)
	if err != nil {
		l.WithError(err).Error("failed to create logrus out hack")
		return 1
	}
	l.Out = logOut

	// TODO: use the config
	_, err = internal.LoadConfig()
	if err != nil {
		l.WithError(err).Error("Failed to load CNI config")
		return 1
	}

	l.WithField("podUID", *podUID).WithField("netns", *netNS).WithField("containerID", *containerID).Info("Started")

	nebulaConfig := os.Getenv("NEBULA_CONFIG")
	configDir, err := renderConfigs(*containerID, nebulaConfig)
	if err != nil {
		l.WithError(err).Error("Failed to render configs")
		return 1
	}

	hostCertPEM, err := checkExistingCert(*containerID)
	if err != nil {
		l.WithError(err).Error("error checking for existing host.crt, continuing")
		hostCertPEM = ""
	}

	// TODO: Some installs might not need this provisional cert step
	// and can just get the final cert here before starting Nebula?
	if hostCertPEM == "" {
		err = provisionalCert(*containerID, podName, nebulaIP)
		if err != nil {
			l.WithError(err).Error("Failed to generate provisional cert")
			return 1
		}

		l.Info("provisional cert created")
	}

	c := config.NewC(l)
	err = c.Load(configDir)
	if err != nil {
		l.WithError(err).Error("Failed to load config")
		return 1
	}

	version := fmt.Sprintf("%s-cni-%s", NebulaVersion, Build)
	ctrl, err := nebula.Main(c, false, version, l, nil)
	if err != nil {
		var ctxErr *util.ContextualError
		if errors.As(err, &ctxErr) {
			ctxErr.Log(l)
			return 1
		}

		l.WithError(err).Error("Failed to start")
		return 1
	}

	l.Info("starting nebula")

	ctrl.Start()

	if err = waitForNebulaReady(); err != nil {
		l.WithError(err).Error("failed to wait for nebula to be ready")
		return 1
	}

	notifyReady()

	// If we already completed last run, then the initContainer will be gone
	isComplete, err := checkComplete(*containerID)
	if err != nil {
		l.WithError(err).Error("failed to check if complete")
		return 1
	}
	if isComplete {
		l.Info("already completed, so will not wait for init container")
	} else {
		l.Info("waiting for init container")

		waitForInitContainer(nebulaIP)
	}

	// ilog will send log messages directly to the nebula-cni-init container
	// so it can log them and make them visible to the user.
	initLogger := startInitLogger(nebulaIP)
	ilog := &multiLogger{sl: slog.New(slog.NewTextHandler(initLogger, nil))}
	defer initLogger.CloseFlush()

	ilog.Info("nebula-cni-instance connected", "instance-version", Build, "nebula-version", NebulaVersion)

	authToken, err := getInitToken(nebulaIP)
	if err != nil {
		l.WithError(err).Error("failed to GET /token")
		return 1
	}

	if hostCertPEM == "" {
		ilog.Info("fetching cert")

		hostCertPEM, err = fetchCert(*containerID, nebulaIP, authToken)
		if err != nil {
			ilog.Error(err, "Failed to fetch cert")
			return 1
		}

		ilog.Info("cert fetched")

		c.ReloadConfig()

		ilog.Info("config reloaded")
	}

	go reloader(c, ctrl, *containerID, nebulaConfig)

	// we are finished sending logs to the init container, close it and make
	// sure nothing else tries to use it
	initLogger.Close()
	ilog = nil

	url := fmt.Sprintf("http://%s/init", net.JoinHostPort(nebulaIP, internal.InitPort))
	l.WithField("url", url).Info("POSTing to init container")

	body := internal.InitPostBody{
		NebulaCertificate: hostCertPEM,
	}
	bodyBytes, err := json.Marshal(&body)
	if err != nil {
		l.WithError(err).Error("Failed to generate POST json")
		return 1
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		l.WithError(err).Error("failed to POST, continuing")
	} else {
		httpDiscardBody(resp)

		var logLevel logrus.Level
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNoContent:
			logLevel = logrus.InfoLevel
		default:
			logLevel = logrus.ErrorLevel
		}

		l.WithField("url", url).
			WithField("init-version", resp.Header.Get(internal.HeaderVersion)).
			WithField("status", resp.StatusCode).
			Log(logLevel, "POSTed to init container")
	}

	if !isComplete {
		err = markComplete(*containerID)
		if err != nil {
			l.WithError(err).Error("failed to mark as complete")
		}
	}

	ctrl.ShutdownBlock()

	l.Info("shutdown")

	return 0
}

// When the init container is ready, it will listen on internal.InitPort
func waitForInitContainer(nebulaIP string) {
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(nebulaIP, internal.InitPort), 1*time.Second)
		if err == nil {
			err = conn.Close()
			if err != nil {
				l.WithError(err).Error("failed to close conn")
			}
			return
		}

		if !errors.Is(err, syscall.ECONNREFUSED) {
			l.WithError(err).Error("unexpected error waiting for init container")
		}

		time.Sleep(1 * time.Second)
	}
}

func waitForNebulaReady() error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := net.InterfaceByName("tun1"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for nebula")
		}
		time.Sleep(1 * time.Second)
	}
}

func configDir(containerID string) string {
	return filepath.Join(internal.StateDir, containerID)
}

func renderConfigs(containerID string, nebulaConfig string) (string, error) {
	cfgDir := configDir(containerID)

	err := os.MkdirAll(cfgDir, 0700)
	if err != nil {
		return cfgDir, fmt.Errorf("renderConfigs: failed to create dir: %w", err)
	}

	if nebulaConfig != "" {
		bFilename := filepath.Join(cfgDir, "a_pod_nebula_conf.yml")
		err = os.WriteFile(bFilename, []byte(nebulaConfig), 0644)
		if err != nil {
			return cfgDir, fmt.Errorf("renderConfigs: failed to create pod config file: %w", err)
		}
	}

	cFilename := filepath.Join(cfgDir, "b_cluster_nebula_conf.yml")
	err = copyFile(filepath.Join(internal.ConfigDir, "nebula.yml"), cFilename)
	if err != nil {
		return cfgDir, fmt.Errorf("renderConfigs: failed to create cluster config: %w", err)
	}

	return cfgDir, nil
}

func copyFile(source, dest string) (err error) {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		errC := src.Close()
		if err == nil {
			err = errC
		}
	}()

	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer func() {
		errC := dst.Close()
		if err == nil {
			err = errC
		}
	}()

	_, err = io.Copy(dst, src)

	return err
}

func checkComplete(containerID string) (bool, error) {
	cfgDir := configDir(containerID)

	_, err := os.Stat(filepath.Join(cfgDir, "complete"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

func markComplete(containerID string) error {
	cfgDir := configDir(containerID)

	err := os.WriteFile(filepath.Join(cfgDir, "complete"), []byte("complete"), 0644)
	if err != nil {
		return err
	}

	return nil
}

func checkExistingCert(containerID string) (string, error) {
	cfgDir := configDir(containerID)

	certRaw, err := os.ReadFile(filepath.Join(cfgDir, "host.crt"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}

	cert, _, err := cert.UnmarshalCertificateFromPEM(certRaw)
	if err != nil {
		return "", err
	}

	if cert.Expired(time.Now()) {
		l.WithField("cert", cert).Info("Existing cert is expired, ignoring")
		return "", nil
	}

	l.WithField("cert", cert).Info("Existing cert found, keeping")
	return string(certRaw), nil
}

func provisionalCert(containerID, podName, nebulaIP string) error {
	// TODO: create a provisional host.crt and host.key files to do bootstrap with

	return nil
}

func fetchCert(containerID, nebulaIP, authToken string) (string, error) {
	cfgDir := configDir(containerID)

	// TODO: Get a signed cert for this host, perhaps using a volume mounted service account token

	certBytes, err := os.ReadFile(filepath.Join(cfgDir, "host.crt"))
	if err != nil {
		return "", err
	}

	return string(certBytes), nil
}

// See https://github.com/slackhq/nebula/pull/929
func notifyReady() {
	// SdNotifyReady tells systemd the service is ready and dependent services can now be started
	// https://www.freedesktop.org/software/systemd/man/sd_notify.html
	// https://www.freedesktop.org/software/systemd/man/systemd.service.html
	const SdNotifyReady = "READY=1"

	sockName := os.Getenv("NOTIFY_SOCKET")
	if sockName == "" {
		l.Info("NOTIFY_SOCKET systemd env var not set, not sending ready signal")
		return
	}

	conn, err := net.DialTimeout("unixgram", sockName, time.Second)
	if err != nil {
		l.WithError(err).Error("failed to connect to systemd notification socket")
		return
	}
	defer conn.Close()

	err = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err != nil {
		l.WithError(err).Error("failed to set the write deadline for the systemd notification socket")
		return
	}

	if _, err = conn.Write([]byte(SdNotifyReady)); err != nil {
		l.WithError(err).Error("failed to signal the systemd notification socket")
		return
	}

	l.Info("notified systemd the service is ready")
}

// TODO: make this configurable (the file names are hardcoded) or remove
func reloader(c *config.C, ctrl *nebula.Control, containerID, nebulaConfig string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		l.WithError(err).Error("Failed to create fsnotify watcher")
		return
	}
	defer watcher.Close()

	err = watcher.Add(internal.ConfigDir)
	if err != nil {
		l.WithError(err).Error("Failed to add cluster config directory to watcher")
		return
	}
	err = watcher.Add(internal.ConfigDir + "/root.crt")
	if err != nil {
		l.WithError(err).Error("Failed to add root cert to watcher")
		return
	}
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	lastReload := time.Now()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Reload config iff it is one of the files we are interested in.
			if !strings.HasSuffix(event.Name, "nebula.yml") &&
				!strings.HasSuffix(event.Name, "root.crt") {
				continue
			}
			l.WithField("event", event).Info("Config file changed")
			// chef moves files into place triggering Create instead of Write.
			if event.Has(fsnotify.Write | fsnotify.Create | fsnotify.Rename) {
				if time.Since(lastReload) > time.Minute {
					// if chef changed one file, it may change the other so
					// wait 5 seconds before actually reloading, but
					// set lastReload to now, because we don't want these
					// queuing up.
					lastReload = time.Now()
					time.Sleep(5 * time.Second)
					l.Info("Re-copying config and readloading due to global config/cert change")
					// containerID can't change while running, so configDir
					// will not change. Intentionally ignoring.
					_, err := renderConfigs(containerID, nebulaConfig)
					if err != nil {
						l.WithError(err).Error("Failed to render configs")
						break
					}
					c.ReloadConfig()
					lastReload = time.Now()
				} else {
					l.Info("Skipping config reload - too soon since last reload")
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			l.WithError(err).Error("Watcher error")
		case <-ticker.C:
			if time.Since(lastReload) > time.Minute {
				l.Info("Reloading config due to hourly ticker")
				_, err := renderConfigs(containerID, nebulaConfig)
				if err != nil {
					l.WithError(err).Error("Failed to render configs")
					break
				}
				c.ReloadConfig()

				lastReload = time.Now()
			} else {
				l.Info("Skipping config reload - too soon since last reload")
			}
		case <-ctrl.Context().Done():
			return
		}
	}
}
