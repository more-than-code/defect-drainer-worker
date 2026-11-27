package release

import (
	"errors"
	"regexp"
	"strconv"
)

// Version is a dd-worker release version: MAJOR.MINOR.PATCH with an
// optional -rcN. SemVer orders pre-release identifiers as text, which puts
// rc10 before rc9; here N compares as a number, and a release sorts after
// its RCs (0.2.0-rc9 < 0.2.0-rc10 < 0.2.0). Any other pre-release form is
// not a version a host updates to by itself.
type Version struct {
	Major, Minor, Patch int
	// RC is N of -rcN, or 0 for a final release.
	RC int
}

var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})\.(0|[1-9][0-9]{0,3})\.(0|[1-9][0-9]{0,3})(?:-rc([1-9][0-9]{0,3}))?$`)

// ErrBadVersion: not X.Y.Z or X.Y.Z-rcN.
var ErrBadVersion = errors.New("version is not X.Y.Z or X.Y.Z-rcN")

// ParseVersion reads X.Y.Z or X.Y.Z-rcN (each part at most four digits).
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, ErrBadVersion
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.RC, _ = strconv.Atoi(m[4])
	}
	return v, nil
}

// String prints the version as ParseVersion reads it.
func (v Version) String() string {
	s := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.RC > 0 {
		s += "-rc" + strconv.Itoa(v.RC)
	}
	return s
}

// Compare returns -1, 0 or 1 as v is older than, equal to or newer than o.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if d[0] != d[1] {
			return sign(d[0] - d[1])
		}
	}
	switch {
	case v.RC == o.RC:
		return 0
	case v.RC == 0:
		return 1 // a final release is newer than its RCs
	case o.RC == 0:
		return -1
	}
	return sign(v.RC - o.RC)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Refusal reasons of Allowed (docs/adr/0002-self-update.md §5, §6).
const (
	ReasonNotAllowed = "not_allowed" // a version this rule does not read
	ReasonNotNewer   = "not_newer"
	ReasonCrossMinor = "cross_minor"
)

// Allowed checks the update rule from the installed version cur to target:
// both parse, target is newer, and the major and minor are the same. It
// returns "" when the update is allowed, else a reason.
func Allowed(cur, target string) string {
	c, err := ParseVersion(cur)
	if err != nil {
		return ReasonNotAllowed
	}
	t, err := ParseVersion(target)
	if err != nil {
		return ReasonNotAllowed
	}
	if t.Compare(c) <= 0 {
		return ReasonNotNewer
	}
	if t.Major != c.Major || t.Minor != c.Minor {
		return ReasonCrossMinor
	}
	return ""
}
