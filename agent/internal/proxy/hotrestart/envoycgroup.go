package hotrestart

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This file is CgroupCpuUtil (source/server/cgroup_cpu_util.cc) of the pinned
// Envoy, step for step: the cgroup term of its default worker count
// (envoycpucount.go, issue #1442). Where Envoy gives up, so does this: "no
// limit" is the answer to every failure, as it is there.

const (
	cgroupV1 = "v1"
	cgroupV2 = "v2"
)

// cgroupMount is a cgroup filesystem mount from /proc/self/mountinfo: where it
// is mounted and which directory of the hierarchy is its root.
type cgroupMount struct {
	root       string
	mountPoint string
}

// cgroupCPULimit is CgroupCpuUtil::getCpuLimit: the CPU limit of the process's
// own cgroup in whole CPUs, rounded down with a floor of 1, and whether there
// is one.
func cgroupCPULimit(src cpuSources) (int, bool) {
	dir, version, ok := ownCgroupDir(src)
	if !ok {
		return 0, false
	}
	var ratio float64
	if version == cgroupV1 {
		quota, qerr := readAsEnvoy(src, dir+"/cpu.cfs_quota_us")
		period, perr := readAsEnvoy(src, dir+"/cpu.cfs_period_us")
		if qerr != nil || perr != nil {
			return 0, false
		}
		ratio, ok = cgroupV1Ratio(quota, period)
	} else {
		cpuMax, err := readAsEnvoy(src, dir+"/cpu.max")
		if err != nil {
			return 0, false
		}
		ratio, ok = cgroupV2Ratio(cpuMax)
	}
	switch {
	case !ok:
		return 0, false
	case ratio < 2:
		// max(1, floor(ratio)).
		return 1, true
	case ratio >= math.MaxInt32:
		return math.MaxInt32, true
	}
	return int(math.Floor(ratio)), true
}

// ownCgroupDir is the directory holding the process's own cgroup files and the
// cgroup version its path was found under.
func ownCgroupDir(src cpuSources) (dir, version string, ok bool) {
	mountInfo, err := readAsEnvoy(src, procMountInfoPath)
	if err != nil {
		return "", "", false
	}
	mount, ok := discoverCgroupMount(mountInfo)
	if !ok {
		return "", "", false
	}
	procCgroup, err := readAsEnvoy(src, procCgroupPath)
	if err != nil {
		return "", "", false
	}
	path, version, ok := currentCgroupPath(procCgroup)
	if !ok {
		return "", "", false
	}
	relative, ok := cgroupPathRelativeToMountRoot(path, mount.root)
	if !ok {
		return "", "", false
	}
	if relative != "" && !strings.HasPrefix(relative, "/") {
		return mount.mountPoint + "/" + relative, version, true
	}
	return mount.mountPoint + relative, version, true
}

// readAsEnvoy reads path unless Envoy's file reader refuses it: every read of
// the cgroup detection goes through Filesystem::fileReadToEnd, which answers
// "Invalid path" for a path envoyIllegalPath rejects, and that is "no limit".
func readAsEnvoy(src cpuSources, path string) (string, error) {
	if envoyIllegalPath(src, path) {
		return "", fmt.Errorf("%s: a path envoy does not read", path)
	}
	raw, err := src.ReadFile(path)
	return string(raw), err
}

// envoyIllegalPath is InstanceImplPosix::illegalPath of the pinned Envoy
// (source/common/filesystem/posix/filesystem_impl.cc), in its order:
//
//	if path starts with "/dev/fd/"                           -> legal
//	if path is "/proc/self/mountinfo" or "/proc/self/cgroup",
//	   or starts with "/sys/fs/cgroup/"                      -> legal
//	canonical = realpath(path); if that fails                -> illegal
//	if canonical is /dev, /sys or /proc, or below one:
//	    if canonical is "/dev/shm" or below it (Linux)       -> legal
//	    otherwise                                            -> illegal
//	otherwise                                                -> legal
//
// The first two checks are made on the path as written, before any symlink is
// resolved; the rest on the resolved path. So a cgroup hierarchy mounted under
// /dev/shm is read, one mounted elsewhere under /dev, /sys or /proc is not,
// and neither is a path that does not resolve.
func envoyIllegalPath(src cpuSources, path string) bool {
	if strings.HasPrefix(path, "/dev/fd/") {
		return false
	}
	if path == procMountInfoPath || path == procCgroupPath || strings.HasPrefix(path, "/sys/fs/cgroup/") {
		return false
	}
	canonical, err := src.EvalSymlinks(path)
	if err != nil || canonical == "" {
		return true
	}
	if isOrBelow(canonical, "/dev") || isOrBelow(canonical, "/sys") || isOrBelow(canonical, "/proc") {
		// /dev/shm is the one place under them Envoy reads (Linux).
		return !isOrBelow(canonical, "/dev/shm")
	}
	return false
}

// isOrBelow reports whether path is dir or a path inside it.
func isOrBelow(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// discoverCgroupMount is CgroupCpuUtil::discoverCgroupMount: the first cgroup
// v1 mount with the cpu controller, else the LAST cgroup v2 mount.
func discoverCgroupMount(mountInfo string) (cgroupMount, bool) {
	var (
		v2    cgroupMount
		hasV2 bool
	)
	for line := range strings.SplitSeq(mountInfo, "\n") {
		mount, fsType, superOptions, ok := parseCgroupMountLine(line)
		switch {
		case !ok:
		case fsType == "cgroup2":
			v2, hasV2 = mount, true
		case hasToken(superOptions, "cpu"):
			return mount, true
		}
	}
	return v2, hasV2
}

// parseCgroupMountLine reads one /proc/self/mountinfo line and reports a
// cgroup or cgroup2 mount; any other line, and a malformed one, is not ok.
//
//	mountID parentID major:minor root mountPoint options... - fsType source superOptions
//
// superOptions (where a v1 mount lists its controllers) is empty when the line
// ends at the source.
func parseCgroupMountLine(line string) (mount cgroupMount, fsType, superOptions string, ok bool) {
	rest := line
	// Fields 1-3 are skipped, 4 is the root, 5 the mount point.
	for range 3 {
		if _, rest, ok = strings.Cut(rest, " "); !ok {
			return mount, "", "", false
		}
	}
	root, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return mount, "", "", false
	}
	mountPoint, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return mount, "", "", false
	}
	// The optional fields end at " - ".
	if rest, ok = afterMountInfoSeparator(rest); !ok {
		return mount, "", "", false
	}
	fsType, rest, ok = strings.Cut(rest, " ")
	if !ok || (fsType != "cgroup" && fsType != "cgroup2") {
		return mount, "", "", false
	}
	_, superOptions, _ = strings.Cut(rest, " ")
	mount = cgroupMount{root: unescapeMountInfoPath(root), mountPoint: unescapeMountInfoPath(mountPoint)}
	return mount, fsType, superOptions, true
}

// afterMountInfoSeparator walks the space-separated fields of line up to the
// " - " that ends the optional ones and returns what follows it. It keeps
// Envoy's bounds: a separator with nothing after it is not one.
func afterMountInfoSeparator(line string) (string, bool) {
	for {
		space := strings.IndexByte(line, ' ')
		if space < 0 || space+3 >= len(line) {
			return "", false
		}
		if line[space:space+3] == " - " {
			return line[space+3:], true
		}
		line = line[space+1:]
	}
}

// currentCgroupPath is CgroupCpuUtil::getCurrentCgroupPath over
// /proc/self/cgroup ("hierarchy:controllers:path" per line): the first v1 line
// that lists the cpu controller, else the last v2 line (hierarchy 0).
func currentCgroupPath(procCgroup string) (path, version string, ok bool) {
	var (
		v2Path string
		hasV2  bool
	)
	for line := range strings.SplitSeq(procCgroup, "\n") {
		hierarchy, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		controllers, p, found := strings.Cut(rest, ":")
		if !found {
			continue
		}
		if hierarchy == "0" {
			v2Path, hasV2 = p, true
			continue
		}
		if hasToken(controllers, "cpu") {
			return p, cgroupV1, true
		}
	}
	return v2Path, cgroupV2, hasV2
}

// cgroupPathRelativeToMountRoot turns the process's cgroup path into the path
// below the mount point. A path with a ".." component, or one outside the
// mounted subtree, has none.
func cgroupPathRelativeToMountRoot(path, root string) (string, bool) {
	for component := range strings.SplitSeq(path, "/") {
		if component == ".." {
			return "", false
		}
	}
	switch {
	case root == "/":
		return path, true
	case path == root:
		return "", true
	case strings.HasPrefix(path, root+"/"):
		return path[len(root):], true
	}
	return "", false
}

// cgroupV2Ratio parses cpu.max, "<quota> <period>", with "max" for no limit.
func cgroupV2Ratio(cpuMax string) (float64, bool) {
	parts := strings.Split(trimASCIISpace(cpuMax), " ")
	if len(parts) != 2 || parts[0] == "max" {
		return 0, false
	}
	quota, qerr := strconv.ParseUint(parts[0], 10, 64)
	period, perr := strconv.ParseUint(parts[1], 10, 64)
	if qerr != nil || perr != nil || period == 0 {
		return 0, false
	}
	return float64(quota) / float64(period), true
}

// cgroupV1Ratio parses cpu.cfs_quota_us and cpu.cfs_period_us; a quota of -1
// is no limit, and so is any other value that is not positive.
func cgroupV1Ratio(quotaFile, periodFile string) (float64, bool) {
	quota, qerr := strconv.ParseInt(trimASCIISpace(quotaFile), 10, 64)
	period, perr := strconv.ParseInt(trimASCIISpace(periodFile), 10, 64)
	if qerr != nil || perr != nil || quota <= 0 || period <= 0 {
		return 0, false
	}
	return float64(quota) / float64(period), true
}

func trimASCIISpace(s string) string {
	return strings.Trim(s, " \t\n\v\f\r")
}

// hasToken reports whether the comma-separated list holds token exactly:
// "cpuset" and "cpuacct" are not "cpu".
func hasToken(list, token string) bool {
	for item := range strings.SplitSeq(list, ",") {
		if item == token {
			return true
		}
	}
	return false
}

// unescapeMountInfoPath undoes the kernel's escaping in /proc/self/mountinfo:
// a backslash and three octal digits (\040 is a space). A backslash followed
// by anything else stays as it is.
func unescapeMountInfoPath(path string) string {
	if !strings.Contains(path, `\`) {
		return path
	}
	var out strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '\\' && i+3 < len(path) {
			// Envoy hands strtol the three digits inside a longer string, and
			// strtol reads on: a FOURTH octal digit right after them makes it
			// reject the escape and keep the backslash (`\0407` stays as
			// written; `\040g` is a space then g).
			fourth := i+4 < len(path) && path[i+4] >= '0' && path[i+4] <= '7'
			if v, err := strconv.ParseUint(path[i+1:i+4], 8, 8); err == nil && !fourth {
				out.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		out.WriteByte(path[i])
	}
	return out.String()
}
