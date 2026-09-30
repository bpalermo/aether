// Package udspath resolves the endpoint.aether.io/uds-socket annotation (or an
// EndpointPolicy's spec.udsSocket) to the host path of a workload's Unix socket
// (proposal 034), on the csi.aether.io carrier (proposal 039).
//
// Since 039 Phase 2 the ONLY carrier for a UDS-delivered socket is an inline
// `csi: {driver: csi.aether.io}` volume. The node plugin mounts a per-pod tmpfs
// at <uds-csi-root>/<pod-UID> on the host (nosymfollow,nodev,nosuid,noexec) and
// binds it onto the volume's target path, so a socket the app binds at
// <mountPath>/<file> is host-visible at
//
//	<uds-csi-root>/<pod-UID>/<file>
//
// which is what the node proxy dials. The volume NAME only selects which of the
// pod's volumes carries the socket; it never appears in the path. The kubelet's
// pod-volumes tree — and with it the old emptyDir shape — is gone from the data
// path entirely.
//
// The annotation value ("<volume>/<file>") is attacker-influenced input (any
// pod author can set it), so resolution is fail-closed: the volume must be the
// pod's own csi.aether.io volume, and the file a single, clean path segment — a
// malicious annotation must never address another pod's socket or an arbitrary
// host path.
package udspath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// CSIDriver is the CSI driver name of the UDS carrier (proposal 039): a pod's
// socket volume is `csi: {driver: csi.aether.io}`. It must equal the node
// plugin's DriverName (agent/internal/udscsi), which a test there pins.
const CSIDriver = "csi.aether.io"

// DefaultCSIRoot is where the node plugin mounts the per-pod tmpfs directories
// on the host (the chart's udsCsi.root, the agent's --uds-csi-root default).
const DefaultCSIRoot = "/run/aether/uds"

// MaxPipePathLen is the longest pathname an AF_UNIX address can carry:
// sockaddr_un.sun_path is 108 bytes on Linux and the path is NUL-terminated.
// The check MUST live here, not at the Envoy boundary: an over-long Pipe
// address makes Envoy reject the cluster and NACK the whole CDS update, which
// would take down every cluster in the node's snapshot over one bad
// annotation. Resolution failing instead degrades that single pod to TCP.
const MaxPipePathLen = 107

// PodUIDLen is the length of a Kubernetes pod UID: an RFC 4122 string, which is
// both the common and the worst case (a static pod's UID is a 32-hex hash).
const PodUIDLen = 36

// MaxFileLen is the budget for the socket file name under the default root:
// MaxPipePathLen minus "<DefaultCSIRoot>/<36-byte UID>/", i.e. 107 - 53 = 54
// bytes. It is independent of the volume name and of the kubelet root, so
// admission can check it exactly (proposal 039, R4). An agent run with a longer
// --uds-csi-root shrinks it; the agent re-checks against its own root.
const MaxFileLen = MaxPipePathLen - len(DefaultCSIRoot) - 1 - PodUIDLen - 1

// Reason classifies a resolution failure. The values are the `reason`
// attribute of aether.agent.uds.resolve_failures and appear verbatim in the
// agent's log, so they are part of the operator contract.
type Reason string

const (
	// ReasonBadRequest: the value is not "<volume>/<file>", or the volume part
	// is not a clean single segment.
	ReasonBadRequest Reason = "bad_request"
	// ReasonVolumeNotDeclared: the named volume is not declared by the pod at
	// all (the "drift" case: a policy naming a volume the pods do not mount).
	ReasonVolumeNotDeclared Reason = "volume_not_declared"
	// ReasonNotCSI: the pod declares the named volume, but not as an inline
	// csi.aether.io volume — typically an emptyDir left over from before
	// proposal 039 Phase 2. The breaking cut-over's loud failure.
	ReasonNotCSI Reason = "not_csi"
	// ReasonBadFile: the socket file is not a single, clean path segment.
	ReasonBadFile Reason = "bad_file"
	// ReasonPathTooLong: the resolved path overflows the AF_UNIX sun_path.
	ReasonPathTooLong Reason = "path_too_long"
	// ReasonNoUID: the pod UID is missing or not a clean segment (a stored
	// record written before the UID was persisted).
	ReasonNoUID Reason = "no_uid"
	// ReasonMultipleCSIVolumes: the pod declares more than one csi.aether.io
	// volume. One mesh socket volume per pod; the agent trusts neither.
	ReasonMultipleCSIVolumes Reason = "multiple_csi_volumes"
	// ReasonDisabled: the agent runs with --uds-csi-root empty (the chart's
	// udsCsi.enabled=false), so no socket can be reached.
	ReasonDisabled Reason = "disabled"
)

// Reasons is every Reason, in a stable order: the resolve-failure counter is
// seeded at zero for each so "no failures" and "no metric" are distinguishable.
var Reasons = []Reason{
	ReasonBadRequest, ReasonVolumeNotDeclared, ReasonNotCSI, ReasonBadFile,
	ReasonPathTooLong, ReasonNoUID, ReasonMultipleCSIVolumes, ReasonDisabled,
}

// Error is a classified resolution failure.
type Error struct {
	Reason Reason
	Err    error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

func fail(reason Reason, format string, args ...any) error {
	return &Error{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// ReasonOf returns the Reason of a resolution error, or "" when err is nil or
// not a classified *Error.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// Split parses a "<volume>/<file>" request into its two segments, validating
// the volume. The file is validated separately (ReasonBadFile) so a caller can
// tell a malformed request from a bad file name.
func Split(request string) (volume, file string, err error) {
	volume, file, ok := strings.Cut(request, "/")
	if !ok {
		return "", "", fail(ReasonBadRequest, "uds socket %q is not <volume>/<file>", request)
	}
	if err := validateSegment("volume name", volume); err != nil {
		return "", "", &Error{Reason: ReasonBadRequest, Err: err}
	}
	return volume, file, nil
}

// ResolveCSI maps a "<volume>/<file>" request onto the socket's host path
// <udsRoot>/<podUID>/<file>. csiVolume is the name of the pod's inline
// csi.aether.io volume (CNIPod.uds_csi_volume; "" when it has none): the request
// must name exactly that volume, since only it is backed by the node plugin's
// tmpfs. A request naming any other volume fails ReasonVolumeNotDeclared; the
// caller, which knows the pod's other volumes, refines that to ReasonNotCSI.
//
// udsRoot must be an absolute path (the agent's --uds-csi-root).
func ResolveCSI(udsRoot, podUID, csiVolume, request string) (string, error) {
	if !filepath.IsAbs(udsRoot) {
		return "", fmt.Errorf("uds csi root %q is not absolute", udsRoot)
	}
	if err := validateSegment("pod UID", podUID); err != nil {
		return "", &Error{Reason: ReasonNoUID, Err: err}
	}
	volume, file, err := Split(request)
	if err != nil {
		return "", err
	}
	if csiVolume == "" || volume != csiVolume {
		return "", fail(ReasonVolumeNotDeclared,
			"uds socket %q names volume %q, which is not the pod's %s volume: declare it as `csi: {driver: %s}`",
			request, volume, CSIDriver, CSIDriver)
	}
	if err := validateSegment("socket file", file); err != nil {
		return "", &Error{Reason: ReasonBadFile, Err: err}
	}
	path := filepath.Join(udsRoot, podUID, file)
	if len(path) > MaxPipePathLen {
		return "", fail(ReasonPathTooLong,
			"socket path %q is %d bytes, over the %d-byte AF_UNIX limit: shorten the socket file name (at most %d bytes under %s)",
			path, len(path), MaxPipePathLen, MaxPipePathLen-len(udsRoot)-1-len(podUID)-1, udsRoot)
	}
	return path, nil
}

// ValidateRequest checks a "<volume>/<file>" request without a pod: its shape,
// the file segment, and the file budget under DefaultCSIRoot with a worst-case
// (36-byte) pod UID. It is what admission can check for an EndpointPolicy,
// which names no pod; whether the volume is the pod's csi.aether.io volume is
// checked per pod (the pod webhook, and the agent at resolution).
func ValidateRequest(request string) error {
	volume, _, err := Split(request)
	if err != nil {
		return err
	}
	_, err = ResolveCSI(DefaultCSIRoot, strings.Repeat("0", PodUIDLen), volume, request)
	return err
}

// validateSegment rejects anything that is not a single, clean, relative path
// segment: empty strings, path separators (which would smuggle extra
// components past the <volume>/<file> split), "." and ".." (traversal), and
// NUL (C-string truncation at the syscall boundary).
func validateSegment(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", what)
	case s == "." || s == "..":
		return fmt.Errorf("%s %q is a relative path element", what, s)
	case strings.ContainsAny(s, "/\x00"):
		return fmt.Errorf("%s %q contains a path separator or NUL", what, s)
	case filepath.Clean(s) != s:
		return fmt.Errorf("%s %q is not a clean path segment", what, s)
	}
	return nil
}
