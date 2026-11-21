package install

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type CodexHook struct {
	Key        string `json:"key"`
	Hash       string `json:"currentHash"`
	Trust      string `json:"trustStatus"`
	Enabled    bool   `json:"enabled"`
	SourcePath string `json:"sourcePath"`
	Event      string `json:"eventName"`
}
type CodexListing struct {
	Hooks  []CodexHook `json:"hooks"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Warnings []string `json:"warnings"`
}

// CodexInspect asks the installed engine to resolve configuration layers and
// managed requirements. It makes no model request and runs no guard.
func CodexInspect(dir string) ([]CodexListing, map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server")
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}
	defer func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 8<<20)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		b, e := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
		if e != nil {
			return nil, e
		}
		if _, e = in.Write(append(b, '\n')); e != nil {
			return nil, e
		}
		for scan.Scan() {
			var reply struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scan.Bytes(), &reply) != nil || reply.ID == nil || *reply.ID != id {
				continue
			}
			if len(reply.Error) > 0 {
				return nil, fmt.Errorf("codex %s: %s", method, reply.Error)
			}
			return reply.Result, nil
		}
		if e = scan.Err(); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("codex %s ended without a response", method)
	}
	if _, err = call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "blotter", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return nil, nil, err
	}
	if _, err = in.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		return nil, nil, err
	}
	raw, err := call(2, "hooks/list", map[string]any{"cwds": []string{dir}})
	if err != nil {
		return nil, nil, err
	}
	var list struct {
		Data []CodexListing `json:"data"`
	}
	if err = json.Unmarshal(raw, &list); err != nil {
		return nil, nil, err
	}
	raw, err = call(3, "config/read", map[string]any{"cwd": dir, "includeLayers": true})
	if err != nil {
		return nil, nil, err
	}
	var config map[string]any
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, nil, err
	}
	return list.Data, config, nil
}
