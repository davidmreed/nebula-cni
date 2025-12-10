package main

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	defaultHostCniBinPath = "/host/opt/cni/bin"

	envHostCniBinPath = "HOST_CNI_BIN_PATH"
)

func main() {
	os.Exit(_main())
}

// Based on the logic here:
// - https://github.com/aws/amazon-vpc-cni-k8s/blob/73c45d8d2e3b572832a9b4082c718f77d9d2ee71/cmd/aws-vpc-cni/main.go#L422-L428
func _main() int {
	pluginBins := []string{"nebula-cni", "nebula-cni-instance"}
	hostCNIBinPath := cmp.Or(os.Getenv(envHostCniBinPath), defaultHostCniBinPath)
	err := InstallBinaries(pluginBins, hostCNIBinPath)
	if err != nil {
		slog.Error("Failed to install CNI binaries", "error", err)
		return 1
	}
	return 0
}

func InstallBinaries(pluginBins []string, hostCNIBinPath string) error {
	for _, plugin := range pluginBins {
		target := filepath.Join(hostCNIBinPath, plugin)

		if err := CopyFile(plugin, target); err != nil {
			return fmt.Errorf("failed to install %s: %w", target, err)
		}
		slog.Info("Installed", "target", target)
	}
	return nil
}

func CopyFile(src, dst string) (err error) {
	dstTmp := fmt.Sprintf("%s.tmp", dst)
	if err := cp(src, dstTmp); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	err = os.Rename(dstTmp, dst)
	if err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	si, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	err = os.Chmod(dst, si.Mode())
	if err != nil {
		return fmt.Errorf("failed to chmod file: %w", err)
	}

	return nil
}

func cp(src, dst string) error {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	return err
}
