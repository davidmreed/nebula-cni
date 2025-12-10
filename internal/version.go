package internal

import (
	"runtime/debug"
	"strings"
)

func ModuleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, dep := range info.Deps {
		if dep.Path == "github.com/slackhq/nebula-cni" {
			return strings.TrimPrefix(dep.Version, "v")
		}
	}

	return ""
}
