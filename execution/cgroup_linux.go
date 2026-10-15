//go:build linux

package execution

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	cgroupRoot  = "/sys/fs/cgroup"
	managerLeaf = "execution-manager" // where InitCgroup parks the supervisor itself
	childPrefix = "exec-"             // exec-<supervisor pid>-<seq>, one per Process
	cpuPeriod   = 100000              // µs; cpu.max quota = CPUs * period
)

var (
	cgMu     sync.Mutex
	cgParent string // set by InitCgroup
	cgSeq    atomic.Uint64
)

func unavailable(err error, format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrLimitsUnavailable, msg)
	}
	return fmt.Errorf("%w: %s: %w", ErrLimitsUnavailable, msg, err)
}

// InitCgroup prepares this process's own, delegated cgroup v2 subtree to host
// per-Process limit cgroups. Call it once at startup when deploying with
// Delegate=yes (or any writable cgroup). It
//
//   - moves ONLY the calling process into a leaf "execution-manager" (cgroup v2
//     forbids processes in a cgroup that delegates controllers to children),
//   - enables the memory, cpu and pids controllers for the parent,
//
// It never touches other processes: if the cgroup holds any, enabling
// controllers fails with ErrLimitsUnavailable. Afterwards every Process gets a
// cgroup (full tree containment), even with zero Limits.
//
// Without InitCgroup, Start uses the process's current cgroup only if it
// already has the needed controllers enabled, else it returns
// ErrLimitsUnavailable for non-zero Limits.
func InitCgroup() error {
	cgMu.Lock()
	defer cgMu.Unlock()
	own, err := ownCgroup()
	if err != nil {
		return err
	}
	parent := own
	alreadyLeaf := filepath.Base(own) == managerLeaf
	if alreadyLeaf {
		parent = filepath.Dir(own)
	}
	want := []string{"memory", "cpu", "pids"}
	// Fail predictable delegation/occupancy errors before moving the caller.
	if err := preflightCgroup(parent, os.Getpid()); err != nil {
		return err
	}
	if !alreadyLeaf {
		leaf := filepath.Join(own, managerLeaf)
		if err := os.Mkdir(leaf, 0755); err != nil && !errors.Is(err, fs.ErrExist) {
			return unavailable(err, "create %s", leaf)
		}
		if err := writeCgroup(leaf, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
			return unavailable(err, "move supervisor into %s", leaf)
		}
	}
	if len(missingFrom(parent, "cgroup.subtree_control", want)) > 0 {
		if err := writeCgroup(parent, "cgroup.subtree_control", "+memory +cpu +pids"); err != nil {
			return unavailable(err, "enable controllers in %s (other processes in that cgroup?)", parent)
		}
	}
	cgParent = parent
	return nil
}

// This check is deliberately read-only. Controller enablement can still fail
// later if another process joins concurrently; startup must treat that as fatal.
func preflightCgroup(parent string, caller int) error {
	want := []string{"memory", "cpu", "pids"}
	if missing := missingFrom(parent, "cgroup.controllers", want); len(missing) > 0 {
		return unavailable(nil, "%s does not offer controllers %v", parent, missing)
	}
	b, err := os.ReadFile(filepath.Join(parent, "cgroup.procs"))
	if err != nil {
		return unavailable(err, "read delegation root processes")
	}
	for _, field := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid != caller {
			return unavailable(nil, "delegation root contains another process")
		}
	}
	return nil
}

// cgroup is the per-Process cgroup holding the limits.
type cgroup struct {
	dir string
	fd  *os.File // directory handle for clone3(CLONE_INTO_CGROUP)
}

// newCgroup creates the cgroup with limits already written. It returns nil if
// neither limits nor InitCgroup ask for one.
func newCgroup(l Limits) (*cgroup, error) {
	cgMu.Lock()
	parent := cgParent
	cgMu.Unlock()
	if parent == "" {
		if l == (Limits{}) {
			return nil, nil
		}
		var err error
		if parent, err = ownCgroup(); err != nil {
			return nil, err
		}
	}
	if missing := missingFrom(parent, "cgroup.subtree_control", l.controllers()); len(missing) > 0 {
		return nil, unavailable(nil, "controllers %v are not enabled in %s/cgroup.subtree_control; call InitCgroup or delegate the cgroup (systemd Delegate=yes)", missing, parent)
	}
	dir := filepath.Join(parent, fmt.Sprintf("%s%d-%d", childPrefix, os.Getpid(), cgSeq.Add(1)))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, unavailable(err, "create cgroup %s", dir)
	}
	c := &cgroup{dir: dir}
	if err := c.configure(l); err != nil {
		c.remove()
		return nil, err
	}
	fd, err := os.Open(dir)
	if err != nil {
		c.remove()
		return nil, unavailable(err, "open cgroup %s", dir)
	}
	c.fd = fd
	return c, nil
}

func (l Limits) controllers() (c []string) {
	if l.MemoryBytes > 0 {
		c = append(c, "memory")
	}
	if l.CPUs > 0 {
		c = append(c, "cpu")
	}
	if l.Processes > 0 {
		c = append(c, "pids")
	}
	return c
}

func (c *cgroup) configure(l Limits) error {
	set := func(file, val string) error {
		if err := writeCgroup(c.dir, file, val); err != nil {
			return unavailable(err, "write %s", file)
		}
		return nil
	}
	if l.MemoryBytes > 0 {
		if err := set("memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return err
		}
		// No swap escape hatch, and an OOM kills the whole tree rather than one victim.
		// Both files are absent on kernels without swap accounting / oom.group.
		for file, val := range map[string]string{"memory.swap.max": "0", "memory.oom.group": "1"} {
			if err := set(file, val); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	if l.CPUs > 0 {
		quota := int64(math.Floor(l.CPUs * cpuPeriod))
		if quota < 1000 || l.CPUs*cpuPeriod >= float64(math.MaxInt64) {
			return unavailable(nil, "CPU quota outside supported range")
		}
		if err := set("cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)); err != nil {
			return err
		}
	}
	if l.Processes > 0 {
		if err := set("pids.max", strconv.Itoa(l.Processes)); err != nil {
			return err
		}
	}
	return nil
}

// kill SIGKILLs every task in the cgroup (cgroup.kill needs Linux 5.14; older
// kernels fall back to signalling the listed pids).
func (c *cgroup) kill() {
	if writeCgroup(c.dir, "cgroup.kill", "1") == nil {
		return
	}
	pids, _ := c.pids()
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// empty reports that no live (non-zombie) task remains.
func (c *cgroup) empty() bool {
	pids, err := c.pids()
	if err != nil {
		return false
	}
	for _, pid := range pids {
		if !isZombie(pid) {
			return false
		}
	}
	return true
}

// Walk descendants as well: cgroup.kill is recursive, but cgroup.procs is not.
func (c *cgroup) pids() (pids []int, err error) {
	err = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				pids = append(pids, pid)
			}
		}
		return nil
	})
	return pids, err
}

// release closes the handle and removes the directory (retrying while the
// kernel finishes tearing tasks down).
func (c *cgroup) release() error {
	if c.fd != nil {
		c.fd.Close()
	}
	return c.remove()
}

func (c *cgroup) remove() error {
	var err error
	waitFor(func() bool {
		var dirs []string
		err = filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				dirs = append(dirs, path)
			}
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if err != nil {
			return false
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			err = syscall.Rmdir(dirs[i])
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false
			}
		}
		return true
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("execution: remove cgroup %s: %w", c.dir, err)
	}
	return nil
}

func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true // already gone
	}
	i := bytes.LastIndexByte(b, ')') // state follows "(comm) "
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

// ownCgroup is this process's cgroup v2 directory.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", unavailable(err, "read /proc/self/cgroup")
	}
	for _, line := range strings.Split(string(b), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			dir := filepath.Join(cgroupRoot, path)
			if _, err := os.Stat(filepath.Join(dir, "cgroup.controllers")); err != nil {
				return "", unavailable(err, "%s is not a cgroup v2 directory", dir)
			}
			return dir, nil
		}
	}
	return "", unavailable(nil, "cgroup v2 is not in use")
}

// missingFrom returns the names of want that are not listed in dir/file.
func missingFrom(dir, file string, want []string) (missing []string) {
	b, _ := os.ReadFile(filepath.Join(dir, file))
	have := strings.Fields(string(b))
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			missing = append(missing, w)
		}
	}
	return missing
}

func writeCgroup(dir, file, val string) error {
	return os.WriteFile(filepath.Join(dir, file), []byte(val), 0o644)
}
