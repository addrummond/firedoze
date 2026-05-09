package cgroup

import (
	"os/exec"
	"path/filepath"
)

func Command(binary string, args []string, cgroupPath string) *exec.Cmd {
	if cgroupPath == "" {
		return exec.Command(binary, args...)
	}
	cmd := exec.Command("/bin/sh", append([]string{
		"-c",
		`printf '%s\n' "$$" > "$FIREDOZE_CGROUP_PROCS"; exec "$@"`,
		"firedoze-cgroup-exec",
		binary,
	}, args...)...)
	cmd.Env = append(cmd.Environ(), "FIREDOZE_CGROUP_PROCS="+filepath.Join(cgroupPath, "cgroup.procs"))
	return cmd
}
