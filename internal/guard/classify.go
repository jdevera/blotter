package guard

import (
	"bytes"
	"encoding/json"
)

func Classify(harness string, rc int, stdout []byte) string {
	if rc != 0 && rc != 2 {
		return "error"
	}
	if rc == 2 {
		return "block"
	}
	text := bytes.TrimSpace(stdout)
	if len(text) == 0 || text[0] != '{' {
		return "pass"
	}
	var root map[string]any
	if json.Unmarshal(text, &root) != nil {
		return "error"
	}
	for _, k := range []string{"continue", "suppressOutput"} {
		if v, ok := root[k]; ok {
			if _, ok = v.(bool); !ok {
				return "error"
			}
		}
	}
	for _, k := range []string{"stopReason", "systemMessage", "reason"} {
		if v, ok := root[k]; ok {
			if _, ok = v.(string); !ok {
				return "error"
			}
		}
	}
	if harness == "codex" {
		for _, k := range []string{"continue", "stopReason", "suppressOutput"} {
			if _, ok := root[k]; ok {
				return "error"
			}
		}
	}
	decision := ""
	if v, ok := root["hookSpecificOutput"]; ok {
		obj, ok := v.(map[string]any)
		if !ok {
			return "error"
		}
		if obj["hookEventName"] != "PreToolUse" {
			return "error"
		}
		if v, ok := obj["permissionDecision"]; ok {
			var valid bool
			decision, valid = v.(string)
			if !valid {
				return "error"
			}
			if decision != "allow" && decision != "deny" && decision != "ask" {
				return "error"
			}
		}
		for _, k := range []string{"permissionDecisionReason", "additionalContext"} {
			if v, ok := obj[k]; ok {
				if _, ok = v.(string); !ok {
					return "error"
				}
			}
		}
		if v, ok := obj["updatedInput"]; ok {
			if _, ok = v.(map[string]any); !ok {
				return "error"
			}
		}
	}
	if v, ok := root["decision"]; ok && v != "block" && v != "approve" {
		return "error"
	}
	if harness == "codex" && decision == "ask" {
		return "error"
	}
	if decision == "deny" || root["decision"] == "block" || (harness == "claude" && root["continue"] == false) {
		return "block"
	}
	if decision == "ask" {
		return "ask"
	}
	return "pass"
}
