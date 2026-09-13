// Package fileguard provides fast, conservative pre-tool deletion guardrails.
package fileguard

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var destructive = regexp.MustCompile(`(?i)(\b(rm|rmdir|unlink|srm|gfind)\b|\bfind\b[^\n]*-delete\b|\brsync\b[^\n]*--delete|\bgit\s+clean\b|\b(shutil\s*\.\s*rmtree|os\s*\.\s*(remove|unlink|rmdir)|fs\s*\.\s*(rm|rmSync|unlink|unlinkSync|rmdir|rmdirSync))\s*\(|\b(removeItemAtPath|removeItemAtURL|RemoveAll|deleteRecursively)\b|\b(diskutil\s+(erase|zero|secureErase)|mkfs|newfs)\b)`)
var emptyTrash = regexp.MustCompile(`(?i)(\b(empty|delete|erase|clean)[^\n]{0,80}\btrash\b|\btrash\b[^\n]{0,80}\b(empty|delete|erase|clean)\b)`)
var interactive = regexp.MustCompile(`(?i)(^|[;&|]\s*)(?:exec\s+)?(?:/[^\s]+/)?(ba|z|da|fi|k|c|tc)?sh\s*(?:-i\s*)?$`)

const removalAdvice = "Permanent deletion is blocked. For ordinary files or directories, use macOS Move to Trash or the host OS Recycle Bin. If the operation fails, report the exact error."

func TrashPath(path string) bool {
	for _, part := range strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".trash" || part == ".trashes" {
			return true
		}
	}
	return false
}

// pathReason checks lexical components before resolving symlinks, so the
// guard never needs to enumerate or read the protected Trash directory.
func pathReason(path, cwd string) string {
	if TrashPath(path) {
		return "Access to macOS Trash is blocked."
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	for n := 0; n < 40; n++ {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		current, changed := "/", false
		for i, part := range parts {
			current = filepath.Join(current, part)
			if TrashPath(current) {
				return "Access to macOS Trash through a path alias is blocked."
			}
			link, err := os.Readlink(current)
			if err != nil {
				continue
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(current), link)
			}
			path = filepath.Join(append([]string{link}, parts[i+1:]...)...)
			if TrashPath(path) {
				return "Access to macOS Trash through a symlink is blocked."
			}
			changed = true
			break
		}
		if !changed {
			return ""
		}
	}
	return "Unable to safely resolve the tool path."
}

func Inspect(tool string, input any, cwd string) string {
	if reason := pathReason(cwd, cwd); reason != "" {
		return reason
	}
	name := strings.ToLower(tool)
	executableInput := strings.Contains(name, "bash") || strings.Contains(name, "shell") || strings.Contains(name, "exec") || strings.Contains(name, "python")
	if strings.Contains(name, "delete") || strings.Contains(name, "remove_file") || strings.Contains(name, "remove_directory") || strings.Contains(name, "empty_trash") {
		return removalAdvice
	}
	var walk func(any, string) string
	walk = func(value any, key string) string {
		switch v := value.(type) {
		case map[string]any:
			for k, child := range v {
				if reason := walk(child, k); reason != "" {
					return reason
				}
			}
		case []any:
			for _, child := range v {
				if reason := walk(child, key); reason != "" {
					return reason
				}
			}
		case bool:
			if v && (key == "tty" || key == "interactive") {
				return "Interactive terminals bypass subsequent tool checks; use a bounded noninteractive command."
			}
		case string:
			if strings.Contains(name, "apply_patch") {
				for _, line := range strings.Split(v, "\n") {
					if strings.HasPrefix(line, "*** Delete File:") {
						return removalAdvice
					}
					for _, prefix := range []string{"*** Add File:", "*** Update File:", "*** Move to:"} {
						if strings.HasPrefix(line, prefix) {
							if reason := pathReason(strings.TrimSpace(strings.TrimPrefix(line, prefix)), cwd); reason != "" {
								return reason
							}
						}
					}
				}
				return ""
			}
			isCommand := executableInput || key == "command" || key == "cmd" || key == "code" || key == "source"
			isPath := strings.Contains(strings.ToLower(key), "path") || key == "cwd" || key == "workdir" || key == "working_directory" || key == "file"
			lower := strings.ToLower(v)
			if (isCommand || isPath) && (strings.Contains(lower, ".trash") || emptyTrash.MatchString(v)) {
				return "Access to macOS Trash and empty-Trash operations are blocked."
			}
			if isCommand && destructiveCommand(v) || strings.Contains(v, "*** Delete File:") {
				return removalAdvice
			}
			if isCommand && interactive.MatchString(strings.TrimSpace(v)) {
				return "Interactive shells bypass subsequent tool checks; use a bounded noninteractive command."
			}
			if isPath {
				if reason := pathReason(v, cwd); reason != "" {
					return reason
				}
			}
			// Also inspect literal shell arguments for symlink aliases. Dynamic
			// expressions are deliberately left to the OS filesystem policy.
			if key == "command" || key == "cmd" {
				for _, token := range strings.Fields(v) {
					token = strings.Trim(token, "\"';")
					if strings.Contains(token, "/") {
						if reason := pathReason(token, cwd); reason != "" {
							return reason
						}
					}
				}
			}
		}
		return ""
	}
	return walk(input, "")
}

func Hook(in io.Reader, out io.Writer, surface string) error {
	var event map[string]any
	reason := ""
	decoder := json.NewDecoder(io.LimitReader(in, 4<<20))
	if err := decoder.Decode(&event); err != nil {
		reason = "File guard could not parse the hook payload; tool execution denied."
	}
	tool, _ := event["tool_name"].(string)
	cwd, _ := event["cwd"].(string)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	input := event["tool_input"]
	if input == nil {
		input = event
	}
	if encoded, ok := input.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(encoded), &decoded) == nil {
			input = decoded
		}
	}
	changed := false
	if reason == "" {
		name := strings.ToLower(tool)
		if strings.Contains(name, "bash") || strings.Contains(name, "shell") || strings.Contains(name, "exec_command") || tool == "" {
			home, _ := os.UserHomeDir()
			updated, rewritten, err := rewrittenInput(input, filepath.Join(home, ".local", "bin", "agent-file-guard"))
			if err != nil {
				reason = err.Error()
			} else if rewritten {
				if surface == "cursor-shell" {
					reason = "This legacy Cursor hook cannot rewrite commands. Use the preToolUse hook to route removal through Move to Trash."
				} else {
					input, changed = updated, true
				}
			}
		}
		if reason == "" {
			reason = Inspect(tool, input, cwd)
		}
	}
	response := map[string]any{}
	if strings.HasPrefix(surface, "cursor") {
		response["permission"] = "allow"
	}
	if changed && reason == "" {
		message := "Removal is routed through macOS Move to Trash. Report the tool's per-file Undo commands in your reply; do not claim a move succeeded before the tool returns."
		if strings.HasPrefix(surface, "cursor") {
			response["updated_input"] = input
			response["user_message"] = message
			response["agent_message"] = message
		} else {
			response["hookSpecificOutput"] = map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": input, "additionalContext": message}
			response["systemMessage"] = "Removal will use Move to Trash and print an Undo command."
		}
	}
	if reason != "" {
		if strings.HasPrefix(surface, "cursor") {
			response = map[string]any{"permission": "deny", "user_message": reason, "agent_message": reason}
		} else {
			response["hookSpecificOutput"] = map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}
		}
	}
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return fmt.Errorf("write guard decision: %w", err)
	}
	return nil
}
