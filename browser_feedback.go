package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	pythonCommandRE    = regexp.MustCompile(`^python(?:[0-9]+(?:\.[0-9]+)*)?$`)
	heredocDelimiterRE = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
)

// playwrightBrowserCommand recognizes an interpreter command anywhere in a
// shell script. This supports scripts that write a temporary browser check and
// run it in a later heredoc line. Plain text tools cannot trigger the hint.
func playwrightBrowserCommand(command string) bool {
	heredocTerminator := ""
	for _, line := range strings.Split(command, "\n") {
		if heredocTerminator != "" {
			if strings.TrimSpace(line) == heredocTerminator {
				heredocTerminator = ""
			}
			continue
		}
		if match := heredocDelimiterRE.FindStringSubmatch(line); len(match) == 2 {
			heredocTerminator = match[1]
			line = line[:strings.Index(line, match[0])]
		}
		for _, segment := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ';' || r == '&' || r == '|'
		}) {
			fields := strings.Fields(segment)
			for len(fields) > 0 && (filepath.Base(fields[0]) == "env" || strings.Contains(fields[0], "=")) {
				fields = fields[1:]
			}
			if len(fields) < 2 {
				continue
			}
			name := strings.ToLower(filepath.Base(fields[0]))
			if pythonCommandRE.MatchString(name) || oneOf(name, "node", "nodejs", "npx", "npm", "pnpm", "yarn", "bun") {
				return true
			}
		}
	}
	return false
}

// chromeStartupCrashFeedback requires either the observed Playwright launch
// failure or the complete macOS registration abort. It deliberately does not
// classify ordinary page or app errors.
func chromeStartupCrashFeedback(raw json.RawMessage) string {
	for _, output := range commandResponseOutputs(raw) {
		lower := strings.ToLower(output)
		playwrightStartupFailure := strings.Contains(lower, "browsertype.launch") && strings.Contains(lower, "target page, context or browser has been closed") && strings.Contains(lower, "/applications/google chrome.app/") && strings.Contains(lower, "<launched> pid=")
		registrationAbort := strings.Contains(lower, "sigabrt") && strings.Contains(output, "_RegisterApplication") && strings.Contains(output, "TransformProcessType")
		if playwrightStartupFailure || registrationAbort {
			return "Native browser startup failed. Do not rerun the same native launch unchanged. Follow the installed one-shot-tally skill's Browser startup failures guidance, then use an alternate browser runtime or service for this check. If that route is unavailable, report the observed browser startup failure with its command and output."
		}
	}
	return ""
}

// commandResponseOutputs reads native command result envelopes and raw stdout
// supplied by local command hooks. It never examines arbitrary nested stdout.
func commandResponseOutputs(raw json.RawMessage) []string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	if output, ok := value.(string); ok {
		return []string{output}
	}
	var outputs []string
	for _, object := range nativeResponseObjects(value) {
		if _, ok := decodeNativeResponseObject(object); !ok {
			continue
		}
		if output, ok := object["output"].(string); ok {
			outputs = append(outputs, output)
		}
	}
	return outputs
}
