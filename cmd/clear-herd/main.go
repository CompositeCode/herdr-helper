package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

type agent struct {
	Kind    string `json:"agent"`
	Name    string `json:"name"`
	PaneID  string `json:"pane_id"`
	Status  string `json:"agent_status"`
	Session *struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"agent_session"`
}

type profile struct {
	Clear  string   `json:"clear"`
	Exit   string   `json:"exit"`
	Resume []string `json:"resume"`
}

type result struct {
	Agent   string
	Pane    string
	Command string
	Err     error
}

type runner interface {
	Run(context.Context, ...string) (string, error)
}

type herdrCLI struct{ binary string }

func (h herdrCLI) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, h.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("herdr %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, herdrCLI{binary: "herdr"})) }

func run(args []string, out, errOut io.Writer, defaultRunner runner) int {
	flags := flag.NewFlagSet("clear-herd", flag.ContinueOnError)
	flags.SetOutput(errOut)
	dryRun := flags.Bool("dry-run", false, "list agents and resume commands without sending input")
	profilesPath := flags.String("profiles", "", "JSON file with additional or overridden agent profiles")
	herdrPath := flags.String("herdr", "herdr", "path to the Herdr CLI")
	timeout := flags.Duration("timeout", 15*time.Second, "time to wait for each agent to exit")
	version := flags.Bool("version", false, "print version")
	if e := flags.Parse(args); e == flag.ErrHelp {
		return 0
	} else if e != nil {
		return 2
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		fmt.Fprintln(errOut, "usage: clear-herd [--dry-run] [--profiles FILE] [--herdr PATH] [--timeout 15s]")
		return 2
	}
	if *version {
		fmt.Fprintln(out, "clear-herd dev")
		return 0
	}
	if h, ok := defaultRunner.(herdrCLI); ok {
		h.binary = *herdrPath
		defaultRunner = h
	}
	profiles, e := loadProfiles(*profilesPath)
	if e != nil {
		fmt.Fprintf(errOut, "profiles: %v\n", e)
		return 2
	}
	ctx := context.Background()
	agents, e := listAgents(ctx, defaultRunner)
	if e != nil {
		fmt.Fprintf(errOut, "agent list: %v\n", e)
		return 1
	}
	if len(agents) == 0 {
		fmt.Fprintln(out, "No Herdr agents found.")
		return 0
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].PaneID < agents[j].PaneID })
	results := make([]result, 0, len(agents))
	for _, a := range agents {
		p, ok := profiles[a.Kind]
		r := result{Agent: a.Kind, Pane: a.PaneID}
		if !ok {
			r.Err = fmt.Errorf("no command profile for %s", a.Kind)
		} else {
			r.Command = resumeFromSession(a, p)
			if *dryRun {
				if r.Command == "" {
					r.Err = errors.New("no resume command available from Herdr session metadata")
				}
			} else if a.PaneID == os.Getenv("HERDR_PANE_ID") {
				r.Err = errors.New("cannot exit the pane running clear-herd")
			} else if a.Status == "blocked" {
				r.Err = errors.New("agent is blocked; resolve its prompt before clearing")
			} else {
				if a.Status == "working" {
					_, r.Err = callWait(ctx, defaultRunner, a.PaneID, *timeout)
				}
				if r.Err == nil {
					r.Command, r.Err = clearAgent(ctx, defaultRunner, a, p, r.Command, *timeout)
				}
			}
		}
		results = append(results, r)
	}
	if *dryRun {
		fmt.Fprintln(out, "Preview (no input sent):")
	} else {
		fmt.Fprintln(out, "Resume commands:")
	}
	failed := false
	for _, r := range results {
		if r.Command != "" {
			fmt.Fprintf(out, "- %s [%s]: %s\n", r.Agent, r.Pane, r.Command)
		}
		if r.Err != nil {
			failed = true
			fmt.Fprintf(errOut, "error: %s [%s]: %v\n", r.Agent, r.Pane, r.Err)
		}
	}
	if failed {
		return 1
	}
	return 0
}

func loadProfiles(path string) (map[string]profile, error) {
	profiles := map[string]profile{
		"codex":  {Clear: "/clear", Exit: "/exit", Resume: []string{"codex", "resume", "{id}"}},
		"claude": {Clear: "/clear", Exit: "/exit", Resume: []string{"claude", "--resume", "{id}"}},
		"cursor": {Clear: "/clear", Exit: "/exit", Resume: []string{"agent", "--resume={id}"}},
	}
	if path == "" {
		return profiles, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var extra map[string]profile
	if err := json.Unmarshal(data, &extra); err != nil {
		return nil, err
	}
	for kind, p := range extra {
		if kind == "" || p.Clear == "" || p.Exit == "" || len(p.Resume) == 0 {
			return nil, fmt.Errorf("profile %q needs clear, exit, and resume fields", kind)
		}
		hasSession := false
		for _, arg := range p.Resume {
			if strings.Contains(arg, "{id}") || strings.Contains(arg, "{path}") || strings.Contains(arg, "{session}") {
				hasSession = true
			}
		}
		if !hasSession {
			return nil, fmt.Errorf("profile %q resume command needs a session placeholder", kind)
		}
		profiles[kind] = p
	}
	return profiles, nil
}

func listAgents(ctx context.Context, h runner) ([]agent, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := h.Run(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	var response struct {
		Result struct {
			Agents []agent `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("invalid Herdr JSON: %w", err)
	}
	for _, a := range response.Result.Agents {
		if a.PaneID == "" || a.Kind == "" {
			return nil, errors.New("Herdr returned an agent without pane_id or agent")
		}
	}
	return response.Result.Agents, nil
}

func resumeFromSession(a agent, p profile) string {
	if a.Session == nil || a.Session.Value == "" {
		return ""
	}
	argv := make([]string, len(p.Resume))
	for i, part := range p.Resume {
		if strings.Contains(part, "{id}") && a.Session.Kind != "id" {
			return ""
		}
		if strings.Contains(part, "{path}") && a.Session.Kind != "path" {
			return ""
		}
		part = strings.ReplaceAll(part, "{id}", a.Session.Value)
		part = strings.ReplaceAll(part, "{path}", a.Session.Value)
		argv[i] = strings.ReplaceAll(part, "{session}", a.Session.Value)
	}
	return joinCommand(argv)
}

func joinCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_./:=+-@", r))
		}) == -1 {
			parts[i] = arg
		} else if runtime.GOOS == "windows" {
			parts[i] = "'" + strings.ReplaceAll(arg, "'", "''") + "'"
		} else {
			parts[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}
	}
	return strings.Join(parts, " ")
}

func clearAgent(ctx context.Context, h runner, a agent, p profile, known string, timeout time.Duration) (string, error) {
	if _, err := call(ctx, h, "pane", "run", a.PaneID, p.Clear); err != nil {
		return known, fmt.Errorf("clear agent: %w", err)
	}
	// Slash commands can redraw the screen asynchronously. Give Herdr time to settle.
	time.Sleep(500 * time.Millisecond)
	if _, err := call(ctx, h, "pane", "run", a.PaneID, p.Exit); err != nil {
		return known, fmt.Errorf("exit agent: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		live, err := listAgents(ctx, h)
		if err != nil {
			return known, fmt.Errorf("verify exit: %w", err)
		}
		found := false
		for _, current := range live {
			if current.PaneID == a.PaneID {
				found = true
				break
			}
		}
		if !found {
			break
		}
		if time.Now().After(deadline) {
			return known, errors.New("agent did not exit before timeout")
		}
		time.Sleep(200 * time.Millisecond)
	}
	output, readErr := call(ctx, h, "pane", "read", a.PaneID, "--source", "recent-unwrapped", "--lines", "120")
	if known == "" && readErr == nil {
		known = resumeFromOutput(output, a.Kind)
	}
	clearCommand := "clear"
	if runtime.GOOS == "windows" {
		clearCommand = "cls"
	}
	_, clearErr := call(ctx, h, "pane", "run", a.PaneID, clearCommand)
	var problems []error
	if readErr != nil {
		problems = append(problems, fmt.Errorf("read exit output: %w", readErr))
	}
	if clearErr != nil {
		problems = append(problems, fmt.Errorf("clear terminal: %w", clearErr))
	}
	if known == "" {
		problems = append(problems, errors.New("no resume command was provided"))
	}
	return known, errors.Join(problems...)
}

func call(ctx context.Context, h runner, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return h.Run(ctx, args...)
}

func callWait(ctx context.Context, h runner, pane string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	output, err := h.Run(ctx, "agent", "wait", pane, "--until", "idle", "--until", "done", "--timeout", fmt.Sprint(timeout.Milliseconds()))
	if err != nil {
		return "", fmt.Errorf("wait for idle agent: %w", err)
	}
	return output, nil
}

func resumeFromOutput(output, kind string) string {
	patterns := map[string]string{
		"codex":  `\bcodex resume [A-Za-z0-9_-]+\b`,
		"claude": `\bclaude (?:--resume|-r) [A-Za-z0-9_-]+\b`,
		"cursor": `\b(?:agent|cursor-agent) --resume(?:=| )[A-Za-z0-9_-]+\b`,
	}
	pattern, ok := patterns[kind]
	if !ok {
		return ""
	}
	matches := regexp.MustCompile(pattern).FindAllString(output, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}
