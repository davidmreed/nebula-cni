package internal

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// TODO: make configurable
const (
	ConfigDir        = "/etc/nebula-cni"
	ConfigPath       = ConfigDir + "/config.yml"
	AnnotationPrefix = "nebula-cni.slackhq.github.com"
	StateDir         = "/run/nebula-cni"
)

type Config struct {
	// TODO: Make the above configurable with this YAML config struct
}

func LoadConfig() (*Config, error) {
	raw, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read %s: %w", ConfigPath, err)
	}

	var config Config
	err = yaml.Unmarshal(raw, &config)
	if err != nil {
		return nil, fmt.Errorf("unable to parse %s: %w", ConfigPath, err)
	}

	return &config, nil
}

// TODO: Some installs might not be able to calculate the IP before talking to the CA
func CalculateNebulaIP(ip string) string {
	// TODO: calculate the Nebula IP for this pod from the underlay IP
	return ip
}
