package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	pythonFiniteLoopRE = regexp.MustCompile(`(?s)\bfor\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s*(\([^\r\n)]*\)|\[[^\r\n\]]*\])\s*:`)
	pythonStringRE     = regexp.MustCompile(`(?s)['\"]([A-Za-z0-9][A-Za-z0-9_-]*)['\"]`)
	// Only absolute f-string paths whose placeholder is the finite loop variable
	// are eligible. The finite literal Path(parent)/(name + suffix) form is also
	// eligible. A command must name every repository selected this way.
	pythonFStringRE = regexp.MustCompile(`(?s)\b[fF]['\"](/[^'\"\r\n]*?)['\"]`)
	// This is the Python form used by a loop that constructs a repository root
	// from a literal parent and a finite application name.
	pythonPathJoinRE = regexp.MustCompile(`(?s)\bPath\(\s*['\"](/[^'\"\r\n]*)['\"]\s*\)\s*/\s*\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\+\s*['\"]([^/'\"\r\n]+)['\"]\s*\)`)
)

// deliveryRootRegistry is intentionally separate from per-turn tally state:
// Stop is allowed to outlive a turn, while an edited repository remains pending
// until ship-it reports a successful delivery of its captured generation.
type deliveryRootRegistry struct {
	Version   int                            `json:"version"`
	SessionID string                         `json:"session_id"`
	Roots     map[string]uint64              `json:"roots"`
	Attempts  map[string]deliveryRootAttempt `json:"attempts,omitempty"`
}

type deliveryRootAttempt struct {
	TurnID     string `json:"turn_id"`
	Generation uint64 `json:"generation"`
}

func deliveryRootRegistryPath(sessionID string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", errors.New("touched root registry requires a session ID")
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(dir, "touched-roots-"+hex.EncodeToString(sum[:16])+".json"), nil
}

func recordExplicitEditRoots(e event) error {
	if !isEditTool(e.ToolName) {
		return nil
	}
	paths := explicitEditPaths(e.ToolName, e.ToolInput)
	if len(paths) == 0 || strings.TrimSpace(e.SessionID) == "" {
		return nil
	}
	roots := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if root, ok := canonicalEditedRoot(path, e.CWD); ok {
			roots[root] = struct{}{}
		}
	}
	if len(roots) == 0 {
		return nil
	}
	return recordDeliveryRoots(e.SessionID, roots)
}

func recordDeliveryRoots(sessionID string, roots map[string]struct{}) error {
	if len(roots) == 0 || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	registryPath, err := deliveryRootRegistryPath(sessionID)
	if err != nil {
		return err
	}
	unlock, err := acquireDeliveryRootRegistryLock(registryPath)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	registry, err := loadDeliveryRootRegistry(registryPath, sessionID)
	if err != nil {
		return err
	}
	for root := range roots {
		registry.Roots[root]++
	}
	return saveDeliveryRootRegistry(registryPath, registry)
}

// opaqueCommandRootSnapshots records a small, attributable set of repository
// snapshots for a dynamic Python loop. It deliberately does not walk a parent
// directory: every selected root must come from a finite literal loop value
// substituted into an absolute f-string path in the command itself.
func opaqueCommandRootSnapshots(command string) map[string]string {
	roots := pythonFiniteLoopRoots(command)
	if len(roots) == 0 {
		return nil
	}
	snapshots := make(map[string]string, len(roots))
	for root := range roots {
		if snapshot, known := gitWorktreeSnapshot(root); known {
			snapshots[root] = snapshot
		}
	}
	return snapshots
}

func recordChangedOpaqueCommandRoots(sessionID string, before map[string]string) error {
	if len(before) == 0 {
		return nil
	}
	changed := make(map[string]struct{}, len(before))
	for root, snapshot := range before {
		if current, known := gitWorktreeSnapshot(root); known && current != snapshot {
			changed[root] = struct{}{}
		}
	}
	return recordDeliveryRoots(sessionID, changed)
}

func pythonFiniteLoopRoots(command string) map[string]struct{} {
	roots := map[string]struct{}{}
	for _, loop := range pythonFiniteLoopRE.FindAllStringSubmatch(command, -1) {
		if len(loop) != 3 {
			continue
		}
		variable, values := loop[1], pythonFiniteLiteralValues(loop[2])
		if len(values) == 0 {
			continue
		}
		for _, match := range pythonFStringRE.FindAllStringSubmatch(command, -1) {
			if len(match) != 2 || !strings.Contains(match[1], "{"+variable+"}") {
				continue
			}
			for _, value := range values {
				path := strings.ReplaceAll(match[1], "{"+variable+"}", value)
				if root, ok := canonicalCommandRoot(path); ok {
					roots[root] = struct{}{}
				}
			}
		}
		for _, match := range pythonPathJoinRE.FindAllStringSubmatch(command, -1) {
			if len(match) != 4 || match[2] != variable {
				continue
			}
			for _, value := range values {
				if root, ok := canonicalCommandRoot(filepath.Join(match[1], value+match[3])); ok {
					roots[root] = struct{}{}
				}
			}
		}
	}
	return roots
}

func pythonFiniteLiteralValues(collection string) []string {
	if len(collection) < 2 {
		return nil
	}
	inner := collection[1 : len(collection)-1]
	values := pythonStringRE.FindAllStringSubmatch(inner, -1)
	if len(values) == 0 {
		return nil
	}
	// Refuse expressions, concatenation, and variable references. The extractor
	// is a finite literal expander, not a Python interpreter.
	remainder := pythonStringRE.ReplaceAllString(inner, "")
	if strings.Trim(strings.ReplaceAll(remainder, ",", ""), " \t\r\n") != "" {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value[1])
	}
	return result
}

func canonicalCommandRoot(path string) (string, bool) {
	if !filepath.IsAbs(path) || hasTrashComponent(path) {
		return "", false
	}
	probe, ok := permittedExistingAncestor(path, 0)
	if !ok || hasTrashComponent(probe) {
		return "", false
	}
	if info, err := os.Stat(probe); err == nil && !info.IsDir() {
		probe = filepath.Dir(probe)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rootOutput, err := exec.CommandContext(ctx, "git", "-C", probe, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" || hasTrashComponent(root) {
		return "", false
	}
	return permittedExistingAncestor(root, 0)
}

func explicitEditPaths(tool string, input json.RawMessage) []string {
	name := normalizedToolName(tool)
	var paths []string
	if name == "applypatch" {
		patch := applyPatchText(input)
		if patch == "" {
			return nil
		}
		for _, line := range strings.Split(patch, "\n") {
			for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
				if path, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(path) != "" {
					paths = append(paths, strings.TrimSpace(path))
				}
			}
		}
		return paths
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return nil
	}
	if name == "write" || name == "edit" {
		var path string
		if json.Unmarshal(fields["file_path"], &path) == nil && strings.TrimSpace(path) != "" {
			return []string{strings.TrimSpace(path)}
		}
	}
	return nil
}

func applyPatchText(input json.RawMessage) string {
	var raw string
	if json.Unmarshal(input, &raw) == nil {
		return raw
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}
	for _, key := range []string{"patch", "command"} {
		if json.Unmarshal(fields[key], &raw) == nil {
			return raw
		}
	}
	return ""
}

func normalizedToolName(tool string) string {
	name := strings.ToLower(strings.TrimSpace(tool))
	for _, separator := range []string{"__", ".", "/", ":"} {
		if index := strings.LastIndex(name, separator); index >= 0 {
			name = name[index+len(separator):]
		}
	}
	return strings.NewReplacer("_", "", "-", "").Replace(name)
}

func canonicalEditedRoot(path, cwd string) (string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		if strings.TrimSpace(cwd) == "" {
			return "", false
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if hasTrashComponent(path) {
		return "", false
	}
	probe, ok := permittedExistingAncestor(filepath.Dir(path), 0)
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rootOutput, err := exec.CommandContext(ctx, "git", "-C", probe, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" || hasTrashComponent(root) {
		return "", false
	}
	canonical, ok := permittedExistingAncestor(root, 0)
	if !ok || hasTrashComponent(canonical) {
		return "", false
	}
	return canonical, true
}

// permittedExistingAncestor resolves symlinks one component at a time. It
// checks each link target before following it, so a harmless-looking alias can
// never make the hook inspect a .Trash or .Trashes target.
func permittedExistingAncestor(path string, links int) (string, bool) {
	if links > 40 || !filepath.IsAbs(path) || hasTrashComponent(path) {
		return "", false
	}
	path = filepath.Clean(path)
	volume, rest := filepath.VolumeName(path), strings.TrimPrefix(path, filepath.VolumeName(path))
	current := volume + string(filepath.Separator)
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == filepath.Separator })
	for index, part := range parts {
		candidate := filepath.Join(current, part)
		if hasTrashComponent(candidate) {
			return "", false
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			return current, true
		}
		if err != nil {
			return "", false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", false
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(candidate), target)
			}
			if hasTrashComponent(target) {
				return "", false
			}
			return permittedExistingAncestor(filepath.Join(target, filepath.Join(parts[index+1:]...)), links+1)
		}
		current = candidate
	}
	return current, true
}

func hasTrashComponent(path string) bool {
	for _, component := range strings.FieldsFunc(filepath.Clean(path), func(r rune) bool { return r == filepath.Separator }) {
		if strings.EqualFold(component, ".trash") || strings.EqualFold(component, ".trashes") {
			return true
		}
	}
	return false
}

func loadDeliveryRootRegistry(path, sessionID string) (deliveryRootRegistry, error) {
	registry := deliveryRootRegistry{Version: 1, SessionID: sessionID, Roots: map[string]uint64{}, Attempts: map[string]deliveryRootAttempt{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return deliveryRootRegistry{}, err
	}
	if err := json.Unmarshal(data, &registry); err != nil || registry.Version != 1 || registry.SessionID != sessionID {
		return deliveryRootRegistry{}, errors.New("invalid touched root registry")
	}
	if registry.Roots == nil {
		registry.Roots = map[string]uint64{}
	}
	if registry.Attempts == nil {
		registry.Attempts = map[string]deliveryRootAttempt{}
	}
	return registry, nil
}

func saveDeliveryRootRegistry(path string, registry deliveryRootRegistry) error {
	data, err := json.Marshal(registry)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func acquireDeliveryRootRegistryLock(path string) (func() error, error) {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				closeErr := lock.Close()
				if unlockErr != nil {
					return unlockErr
				}
				return closeErr
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = lock.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = lock.Close()
			return nil, errors.New("timed out waiting for touched root registry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
