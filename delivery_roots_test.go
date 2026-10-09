package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestChildCommandWorkdirRegistersOnlyChangedRootForParentStop(t *testing.T) {
	for _, shape := range []string{"direct", "wrapped", "freeform"} {
		t.Run(shape, func(t *testing.T) {
			stateDir := retainedTestDir(t)
			t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
			parent, _ := committedTestRepo(t, "package parent\n")
			sibling, siblingFile := committedTestRepo(t, "package sibling\n")
			sibling, err := filepath.EvalSymlinks(sibling)
			if err != nil {
				t.Fatal(err)
			}
			unrelated, unrelatedFile := committedTestRepo(t, "package unrelated\n")
			if err := os.WriteFile(unrelatedFile, []byte("package unrelated\nconst dirty = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			command := "python3 - <<'PY'\nfrom pathlib import Path\nPath('app.go').write_text('package changed\\n')\nPY"
			var input any = map[string]any{"cmd": command, "workdir": sibling}
			tool := "Bash"
			if shape != "direct" {
				source := "text((await tools.exec_command({cmd:" + strconv.Quote(command) + ",workdir:" + strconv.Quote(sibling) + ",yield_time_ms:1000,max_output_tokens:1600})).output);"
				input = map[string]any{"cmd": source}
				if shape == "freeform" {
					input, tool = source, "functions.exec"
				}
			}
			for _, eventName := range []string{"PreToolUse", "PostToolUse"} {
				if eventName == "PostToolUse" {
					if err := os.WriteFile(siblingFile, []byte("package changed\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				hook(t, stateDir, map[string]any{
					"session_id": "parent-session", "turn_id": "child-turn", "hook_event_name": eventName,
					"tool_name": tool, "tool_use_id": "child-edit", "cwd": parent,
					"tool_input": input, "tool_response": map[string]any{"exit_code": 0},
				})
			}
			registryPath, err := deliveryRootRegistryPath("parent-session")
			if err != nil {
				t.Fatal(err)
			}
			registry, err := loadDeliveryRootRegistry(registryPath, "parent-session")
			if err != nil || len(registry.Roots) != 1 || registry.Roots[sibling] != 1 || registry.Roots[unrelated] != 0 || registry.Roots[parent] != 0 {
				t.Fatalf("parent Stop registry = %#v, err=%v", registry, err)
			}
			// A second command that leaves the same dirty tree unchanged must not
			// create a new delivery generation merely because it named a workdir.
			for _, eventName := range []string{"PreToolUse", "PostToolUse"} {
				hook(t, stateDir, map[string]any{
					"session_id": "parent-session", "turn_id": "another-child-turn", "hook_event_name": eventName,
					"tool_name": tool, "tool_use_id": "no-change", "cwd": parent,
					"tool_input": input, "tool_response": map[string]any{"exit_code": 0},
				})
			}
			registry, err = loadDeliveryRootRegistry(registryPath, "parent-session")
			if err != nil || registry.Roots[sibling] != 1 {
				t.Fatalf("unchanged worktree advanced generation: %#v, err=%v", registry, err)
			}
		})
	}
}

func TestCommandWorkdirRejectsDynamicAndForbiddenRoots(t *testing.T) {
	repo, _ := committedTestRepo(t, "package example\n")
	alias := filepath.Join(retainedTestDir(t), "safe-alias")
	if err := os.Symlink(filepath.Join(retainedTestDir(t), ".Trash", "missing"), alias); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		`tools.exec_command({cmd:"python3 mutate.py",workdir:` + strconv.Quote(repo) + ` + suffix})`,
		`tools.exec_command({cmd:"python3 mutate.py",workdir:` + strconv.Quote(alias) + `})`,
		`tools.exec_command({cmd:"python3 mutate.py",workdir:"/tmp/.Trashes/blocked"})`,
		`console.log(` + strconv.Quote(`tools.exec_command({cmd:"mutate",workdir:`+strconv.Quote(repo)+`})`) + `)`,
	} {
		if roots := opaqueCommandRootSnapshots(command, nil, ""); len(roots) != 0 {
			t.Fatalf("unsupported command selected roots: %q: %#v", command, roots)
		}
	}
}

func TestPythonLiteralPathsRegisterOnlyChangedExternalRepository(t *testing.T) {
	for _, shape := range []string{"relative", "literal-parent", "cwd-parent", "wrapped"} {
		t.Run(shape, func(t *testing.T) {
			stateDir := retainedTestDir(t)
			t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
			parent, err := filepath.EvalSymlinks(retainedTestDir(t))
			if err != nil {
				t.Fatal(err)
			}
			driver := filepath.Join(parent, "boost-boompay-ca")
			target := filepath.Join(parent, "boompay-vps-infra-l2")
			unchanged := filepath.Join(parent, "jenkins-local")
			unrelated := filepath.Join(parent, "unrelated")
			committedNamedTestRepo(t, driver, "package driver\n")
			targetFile := committedNamedTestRepo(t, target, "package target\n")
			committedNamedTestRepo(t, unchanged, "package unchanged\n")
			unrelatedFile := committedNamedTestRepo(t, unrelated, "package unrelated\n")
			if err := os.WriteFile(unrelatedFile, []byte("package unrelated\nconst dirty = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			python := "Path('../boompay-vps-infra-l2/app.go').write_text('changed')\nPath('../jenkins-local/app.go').read_text()"
			if shape != "relative" {
				base := "Path(" + strconv.Quote(parent) + ")"
				if shape != "literal-parent" {
					base = "Path.cwd().parent"
				}
				python = "base=" + base + "\ncontroller=base/'boompay-vps-infra-l2/app.go'\ncontroller.write_text('changed')\n(base/'jenkins-local/app.go').read_text()"
			}
			command := "python3 - <<'PY'\nfrom pathlib import Path\n" + python + "\nPY"
			tool, input := "exec_command", any(map[string]any{"cmd": command})
			if shape == "wrapped" {
				tool, input = "functions.exec", "text(await tools.exec_command({cmd:"+strconv.Quote(command)+"}));"
			}
			for _, eventName := range []string{"PreToolUse", "PostToolUse"} {
				if eventName == "PostToolUse" {
					if err := os.WriteFile(targetFile, []byte("package changed\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				hook(t, stateDir, map[string]any{
					"session_id": "literal-paths", "turn_id": "turn", "hook_event_name": eventName,
					"tool_name": tool, "tool_use_id": "external-edit", "cwd": driver,
					"tool_input": input, "tool_response": map[string]any{"exit_code": 0},
				})
			}
			registryPath, err := deliveryRootRegistryPath("literal-paths")
			if err != nil {
				t.Fatal(err)
			}
			registry, err := loadDeliveryRootRegistry(registryPath, "literal-paths")
			if err != nil || len(registry.Roots) != 1 || registry.Roots[target] != 1 {
				t.Fatalf("registered roots = %#v, err=%v; want only changed target", registry.Roots, err)
			}
		})
	}
}

func TestPythonStaticPathsIgnoreBlankAndDynamicAssignments(t *testing.T) {
	for _, command := range []string{"x = ", "x =", "x = unknown()\nx/'other/app.go'", "x = Path.cwd() + suffix\nx/'other/app.go'"} {
		if roots := pythonStaticPathRoots(command, ""); len(roots) != 0 {
			t.Fatalf("unsupported assignment selected roots: %q: %#v", command, roots)
		}
	}
}

func TestExplicitEditRootsTrackOnlyStructuredEditedRepositories(t *testing.T) {
	stateDir := retainedTestDir(t)
	t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
	external, _ := committedTestRepo(t, "package external\n")
	readOnly, _ := committedTestRepo(t, "package readonly\n")
	alias := filepath.Join(retainedTestDir(t), "external alias")
	if err := os.Symlink(external, alias); err != nil {
		t.Fatal(err)
	}

	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "external", "cwd": readOnly,
		"tool_input": map[string]any{"patch": "*** Update File: " + filepath.Join(alias, "app.go") + "\n@@\n"},
	})
	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "external", "cwd": readOnly,
		"tool_input":    map[string]any{"patch": "*** Update File: " + filepath.Join(alias, "app.go") + "\n@@\n"},
		"tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "Read", "tool_use_id": "read-only", "cwd": readOnly,
		"tool_input": map[string]any{"file_path": filepath.Join(readOnly, "app.go")},
	})
	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "content", "cwd": readOnly,
		"tool_input": map[string]any{"patch": "ordinary content mentioning " + filepath.Join(readOnly, "app.go")},
	})
	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "command", "cwd": readOnly,
		"tool_input": map[string]any{"command": "*** Add File: " + filepath.Join(alias, "new", "nested", "app.go") + "\n"},
	})
	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "raw", "cwd": readOnly,
		"tool_input": "*** Add File: " + filepath.Join(alias, "raw.go") + "\n",
	})

	registryPath, err := deliveryRootRegistryPath("session")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := loadDeliveryRootRegistry(registryPath, "session")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Roots) != 1 || registry.Roots[canonical] != 4 {
		t.Fatalf("registry roots = %#v, want only %q", registry.Roots, canonical)
	}
}

func TestOpaquePythonLoopTracksOnlyChangedFiniteTemplateRepositories(t *testing.T) {
	stateDir := retainedTestDir(t)
	t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
	parent, err := filepath.EvalSymlinks(retainedTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	garageTarget := filepath.Join(parent, "garage-boompay-ca")
	partnersTarget := filepath.Join(parent, "partners-boompay-ca")
	unrelatedTarget := filepath.Join(parent, "unrelated-boompay-ca")
	garageFile := committedNamedTestRepo(t, garageTarget, "package garage\n")
	partnersFile := committedNamedTestRepo(t, partnersTarget, "package partners\n")
	unrelatedFile := committedNamedTestRepo(t, unrelatedTarget, "package unrelated\n")
	// This repository is already dirty before the command. It is named by the
	// parent directory but is not a finite loop target, and must stay untracked.
	if err := os.WriteFile(unrelatedFile, []byte("package unrelated\n\nconst old = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "python3 - <<'PY'\nfrom pathlib import Path\nsource = Path('" + parent + "/boost-boompay-ca')\nfor filename in ('.deploy-it.json',):\n    for app in ('garage', 'partners'):\n        root = Path('" + parent + "') / (app + '-boompay-ca')\n        (root / filename).write_text('changed')\nPY"
	wrappedCommand := "text(await tools.exec_command({cmd:" + strconv.Quote(command) + ",workdir:\"/Users/aedev/dev/jenkins-local\"}));"
	driver, _ := committedTestRepo(t, "package driver\n")
	hook(t, stateDir, map[string]any{
		"session_id": "loop", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "functions.exec", "tool_use_id": "loop", "cwd": driver,
		"tool_input": map[string]any{"cmd": wrappedCommand},
	})
	for _, file := range []string{garageFile, partnersFile} {
		if err := os.WriteFile(file, []byte("package changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hook(t, stateDir, map[string]any{
		"session_id": "loop", "turn_id": "turn", "hook_event_name": "PostToolUse",
		"tool_name": "functions.exec", "tool_use_id": "loop", "cwd": driver,
		"tool_response": map[string]any{"exit_code": 0},
	})
	registryPath, err := deliveryRootRegistryPath("loop")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := loadDeliveryRootRegistry(registryPath, "loop")
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Roots) != 2 || registry.Roots[garageTarget] != 1 || registry.Roots[partnersTarget] != 1 || registry.Roots[unrelatedTarget] != 0 {
		t.Fatalf("registry roots = %#v, want only changed finite loop targets", registry.Roots)
	}
}

func committedNamedTestRepo(t *testing.T, repo, contents string) string {
	t.Helper()
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	path := filepath.Join(repo, "app.go")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "add", "app.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if output, err := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "baseline").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	return path
}

func TestExplicitEditRootsRejectTrashComponents(t *testing.T) {
	stateDir := retainedTestDir(t)
	t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
	repo, _ := committedTestRepo(t, "package example\n")
	trashPath := filepath.Join(repo, ".Trashes", "blocked.go")
	hook(t, stateDir, map[string]any{
		"session_id": "session", "turn_id": "turn", "hook_event_name": "PreToolUse",
		"tool_name": "Write", "tool_use_id": "blocked", "cwd": repo,
		"tool_input": map[string]any{"file_path": trashPath},
	})
	registryPath, err := deliveryRootRegistryPath("session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(registryPath); !os.IsNotExist(err) {
		t.Fatalf("Trash path created registry: %v", err)
	}
}

func TestForbiddenSymlinkTargetIsRejectedBeforeGitLookup(t *testing.T) {
	alias := filepath.Join(retainedTestDir(t), "safe-alias")
	// The target is deliberately absent. The resolver must reject its hidden
	// component from the link text, without traversing or reading it.
	if err := os.Symlink(filepath.Join(retainedTestDir(t), ".Trash", "missing"), alias); err != nil {
		t.Fatal(err)
	}
	if _, ok := canonicalEditedRoot(filepath.Join(alias, "new.go"), retainedTestDir(t)); ok {
		t.Fatal("forbidden symlink target was accepted")
	}
}

func TestTouchedRootLockDoesNotAgeSteal(t *testing.T) {
	path := filepath.Join(retainedTestDir(t), "registry.json")
	first, err := acquireDeliveryRootRegistryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first() }()
	acquired := make(chan func() error, 1)
	errs := make(chan error, 1)
	go func() {
		unlock, err := acquireDeliveryRootRegistryLock(path)
		if err != nil {
			errs <- err
			return
		}
		acquired <- unlock
	}()
	time.Sleep(1100 * time.Millisecond)
	select {
	case unlock := <-acquired:
		_ = unlock()
		t.Fatal("registry lock was stolen after one second")
	case err := <-errs:
		t.Fatalf("second lock failed before release: %v", err)
	default:
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-acquired:
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("second registry lock did not acquire after release")
	}
}

func TestFreeformCommandIsPreservedWithoutEvaluation(t *testing.T) {
	source := `text(await tools.exec_command({cmd:"echo example",workdir:"/example"}));`
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := commandFrom(encoded); got != source {
		t.Fatalf("command = %q", got)
	}
	for _, raw := range []string{`null`, `123`, `["ignored"]`, `{ "cmd":123 }`} {
		if got := commandFrom(json.RawMessage(raw)); got != "" {
			t.Fatalf("unexpected command: %q", got)
		}
	}
}
