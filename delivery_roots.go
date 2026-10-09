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
	"strconv"
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
	// Native code-mode hooks wrap exec_command in JavaScript. Accept only a
	// literal argument object; never evaluate an expression to discover roots.
	execCommandObjectRE    = regexp.MustCompile(`\btools\.exec_command\s*\(\s*\{((?:\s*(?:"(?:[^"\\]|\\.)*"|[A-Za-z_][A-Za-z_0-9]*)\s*:\s*(?:"(?:[^"\\]|\\.)*"|-?[0-9]+|true|false|null)\s*,?)+)\}\s*\)`)
	execCommandFieldRE     = regexp.MustCompile(`("(?:[^"\\]|\\.)*"|[A-Za-z_][A-Za-z_0-9]*)\s*:\s*("(?:[^"\\]|\\.)*"|-?[0-9]+|true|false|null)`)
	pythonPathExpressionRE = regexp.MustCompile(`(?:\bPath\(\s*['"]([^'"\r\n]+)['"]\s*\)|\bPath\.cwd\(\)|\b[A-Za-z_][A-Za-z_0-9]*)(?:\.parent)*(?:\s*/\s*['"][^'"\r\n]+['"])*`)
	pythonPathAssignmentRE = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z_0-9]*)\s*=\s*(.*)$`)
	pythonPathLiteralRE    = regexp.MustCompile(`['"]([^'"\r\n]+)['"]`)
	javaScriptLiteralRE    = regexp.MustCompile("(?s)\"(?:[^\"\\\\]|\\\\.)*\"|'(?:[^'\\\\]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`")
	nestedPatchTokenRE     = regexp.MustCompile("(?s)//[^\r\n]*|/\\*.*?\\*/|" + javaScriptLiteralRE.String() + `|\btools\.apply_patch\s*\(` + "|[\"'`/]")
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
	Snapshot   string `json:"snapshot,omitempty"`
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
// snapshots for explicit execution directories and finite Python loop targets.
// It deliberately does not walk a parent directory. A root is recorded only
// when its worktree actually changes between the native pre/post tool events.
func opaqueCommandRootSnapshots(command string, input json.RawMessage, cwd string) map[string]string {
	roots := map[string]struct{}{}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(input, &fields)
	addCommand := func(command, directory string) {
		if root, ok := canonicalCommandRoot(directory); ok {
			roots[root] = struct{}{}
		}
		for root := range pythonFiniteLoopRoots(command) {
			roots[root] = struct{}{}
		}
		for root := range pythonStaticPathRoots(command, directory) {
			roots[root] = struct{}{}
		}
	}
	var workdir string
	explicitWorkdir := json.Unmarshal(fields["workdir"], &workdir) == nil && workdir != ""
	if !explicitWorkdir {
		workdir = cwd
	}
	// An explicit workdir remains attributable even when the command quotes tool
	// names. JavaScript wrappers without one must use their literal child targets.
	if explicitWorkdir || len(fields) != 0 && !strings.Contains(command, "tools.exec_command") && !strings.Contains(command, "tools.apply_patch") {
		addCommand(command, workdir)
	}
	for _, object := range execCommandObjectRE.FindAllStringSubmatch(command, -1) {
		childCommand, childDirectory := "", cwd
		for _, field := range execCommandFieldRE.FindAllStringSubmatch(object[1], -1) {
			if field[1] == "workdir" || field[1] == `"workdir"` {
				_ = json.Unmarshal([]byte(field[2]), &childDirectory)
			} else if field[1] == "cmd" || field[1] == `"cmd"` {
				_ = json.Unmarshal([]byte(field[2]), &childCommand)
			}
		}
		addCommand(childCommand, childDirectory)
	}
	for _, path := range nestedPatchPaths(command) {
		if root, ok := canonicalEditedRoot(path, cwd); ok {
			roots[root] = struct{}{}
		}
	}
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

// Skip quoted examples and comments, accepting only a complete literal argument.
// shortcut: regex/division syntax and template interpolation stay opaque; extend only for observed literal forms.
func nestedPatchPaths(source string) []string {
	var paths []string
	for _, token := range nestedPatchTokenRE.FindAllStringIndex(source, -1) {
		value := source[token[0]:token[1]]
		if len(value) == 1 {
			return nil // Unterminated literals or unsupported slash syntax.
		}
		if value[0] == '`' {
			if _, ok := javaScriptPatchLiteral(value); !ok {
				return nil
			}
		}
		if !strings.HasPrefix(value, "tools.apply_patch") {
			continue
		}
		prefix := strings.TrimSpace(source[:token[0]])
		if strings.HasSuffix(prefix, ".") || strings.HasSuffix(prefix, "$") {
			continue
		}
		argument := strings.TrimSpace(source[token[1]:])
		literal := javaScriptLiteralRE.FindStringIndex(argument)
		if literal == nil || literal[0] != 0 {
			continue
		}
		rest := strings.TrimSpace(argument[literal[1]:])
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ","))
		if !strings.HasPrefix(rest, ")") {
			continue
		}
		patch, ok := javaScriptPatchLiteral(argument[:literal[1]])
		if !ok {
			continue
		}
		encoded, _ := json.Marshal(patch)
		paths = append(paths, explicitEditPaths("apply_patch", encoded)...)
	}
	return paths
}

func javaScriptPatchLiteral(literal string) (string, bool) {
	if literal[0] == '"' {
		var decoded string
		err := json.Unmarshal([]byte(literal), &decoded)
		return decoded, err == nil
	}
	quote, body := literal[0], literal[1:len(literal)-1]
	var decoded strings.Builder
	for body != "" {
		if quote == '`' && strings.HasPrefix(body, "${") {
			return "", false
		}
		if quote == '`' && (strings.HasPrefix(body, "\\`") || strings.HasPrefix(body, "\\$")) {
			decoded.WriteByte(body[1])
			body = body[2:]
			continue
		}
		value, _, rest, err := strconv.UnquoteChar(body, quote)
		if err != nil {
			return "", false
		}
		decoded.WriteRune(value)
		body = rest
	}
	return decoded.String(), true
}

// Resolve only literal Path expressions and earlier literal assignments. Never
// execute Python or discover sibling repositories by walking their parent.
func pythonStaticPathRoots(command, cwd string) map[string]struct{} {
	roots, bindings := map[string]struct{}{}, map[string]string{}
	resolve := func(expression string) (string, bool) {
		match := pythonPathExpressionRE.FindString(expression)
		if match == "" || match != strings.TrimSpace(expression) {
			return "", false
		}
		path, rest := "", ""
		switch {
		case strings.HasPrefix(match, "Path.cwd()"):
			path, rest = cwd, strings.TrimPrefix(match, "Path.cwd()")
		case strings.HasPrefix(match, "Path("):
			literal := pythonPathLiteralRE.FindStringSubmatch(match)
			if len(literal) != 2 || strings.ContainsAny(literal[1], "\\{}") {
				return "", false
			}
			path = literal[1]
			if !filepath.IsAbs(path) {
				path = filepath.Join(cwd, path)
			}
			rest = match[strings.Index(match, ")")+1:]
		default:
			name := strings.FieldsFunc(match, func(r rune) bool { return r == '.' || r == '/' || r == ' ' || r == '\t' })[0]
			path, rest = bindings[name], strings.TrimPrefix(match, name)
		}
		if path == "" || !filepath.IsAbs(path) {
			return "", false
		}
		for strings.HasPrefix(rest, ".parent") {
			path, rest = filepath.Dir(path), strings.TrimPrefix(rest, ".parent")
		}
		for _, literal := range pythonPathLiteralRE.FindAllStringSubmatch(rest, -1) {
			if strings.ContainsAny(literal[1], "\\{}") || filepath.IsAbs(literal[1]) {
				return "", false
			}
			path = filepath.Join(path, literal[1])
		}
		return path, true
	}
	for _, line := range strings.Split(command, "\n") {
		if assignment := pythonPathAssignmentRE.FindStringSubmatch(line); len(assignment) == 3 {
			delete(bindings, assignment[1])
			if path, ok := resolve(assignment[2]); ok {
				bindings[assignment[1]] = path
			}
		}
		for _, expression := range pythonPathExpressionRE.FindAllString(line, -1) {
			if path, ok := resolve(expression); ok {
				if root, ok := canonicalCommandRoot(path); ok {
					roots[root] = struct{}{}
				}
			}
		}
	}
	return roots
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
