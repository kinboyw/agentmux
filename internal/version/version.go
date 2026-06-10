package version

import (
	"encoding/json"
	"fmt"
	"runtime"
)

var (
	Version   = "dev"
	Commit    = ""
	BuildTime = ""
)

type Info struct {
	Role      string `json:"role,omitempty"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func Get(role string) Info {
	return Info{
		Role: role, Version: Version, Commit: Commit, BuildTime: BuildTime,
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
	}
}

func String(role string) string {
	info := Get(role)
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	buildTime := info.BuildTime
	if buildTime == "" {
		buildTime = "unknown"
	}
	return fmt.Sprintf("agentmux %s role=%s commit=%s built=%s go=%s os=%s arch=%s", info.Version, info.Role, commit, buildTime, info.GoVersion, info.OS, info.Arch)
}

func JSON(role string) ([]byte, error) {
	return json.MarshalIndent(Get(role), "", "  ")
}
