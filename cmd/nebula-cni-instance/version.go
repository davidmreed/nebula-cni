package main

import (
	"runtime/debug"
)

var NebulaVersion string

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	for _, dep := range info.Deps {
		if dep.Path == "github.com/slackhq/nebula" {
			NebulaVersion = dep.Version
			break
		}
	}
}
