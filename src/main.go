// hello is the validation kit's MCP server. It only exists to answer questions
// about how Kiro runs a Power (data directory, placeholders, tool
// confirmation, restarts, tool list refresh). It contains no product code.
//
//	hello            supervisor: the process Kiro launches. It runs "hello child"
//	                 behind the same stdio and can restart it while Kiro stays
//	                 connected.
//	hello child      the MCP server itself (initialize, tools/list, tools/call).
//
// The supervisor never talks MCP on its own: it relays JSON-RPC lines between
// Kiro and the child. When the child exits with 75 or 76 it starts a new child,
// replays the original initialize + initialized (discarding the reply) and, for
// 75, sends notifications/tools/list_changed to Kiro.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	serverName    = "npower-validacao-hello"
	buildVersion  = "0.1.0"
	exitHotNotify = 75 // restart the child, then send tools/list_changed
	exitHotSilent = 76 // restart the child, send nothing
	replayID      = `"replay-initialize"`
)

func main() {
	mode := "supervise"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "supervise":
		os.Exit(runSupervisor())
	case "child":
		runChild()
	default:
		fmt.Fprintf(os.Stderr, "usage: hello [child]\n")
		os.Exit(2)
	}
}

// ── data directory ───────────────────────────────────────────────────────────

// resolveDataDir returns the directory the kit stores state in: PLUGIN_DATA
// when it is set, expanded, absolute and writable; otherwise ~/.npower-validacao.
// The reason says why the fallback was used ("" when PLUGIN_DATA was used).
func resolveDataDir(getenv func(string) string, userHome func() (string, error)) (dir, source, reason string) {
	value := getenv("PLUGIN_DATA")
	switch {
	case value == "":
		reason = "PLUGIN_DATA is not set"
	case strings.Contains(value, "${"):
		reason = "PLUGIN_DATA was not expanded: " + value
	case !filepath.IsAbs(value):
		reason = "PLUGIN_DATA is not absolute: " + value
	default:
		if err := os.MkdirAll(value, 0o755); err != nil {
			reason = "PLUGIN_DATA cannot be created: " + err.Error()
		} else if err := probeWritable(value); err != nil {
			reason = "PLUGIN_DATA is not writable: " + err.Error()
		} else {
			return value, "PLUGIN_DATA", ""
		}
	}
	home, err := userHome()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	dir = filepath.Join(home, ".npower-validacao")
	_ = os.MkdirAll(dir, 0o755)
	return dir, "fallback", reason
}

func probeWritable(dir string) error {
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return err
	}
	return os.Remove(probe)
}

// ── logging ──────────────────────────────────────────────────────────────────

type logger struct {
	role string
	file string
	mu   sync.Mutex
}

func newLogger(role, dir string) *logger {
	return &logger{role: role, file: filepath.Join(dir, "supervisor.log")}
}

func (l *logger) logf(format string, args ...any) {
	line := fmt.Sprintf("%s [%s pid=%d] %s\n", time.Now().Format("15:04:05.000"), l.role, os.Getpid(), fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(os.Stderr, "[hello] "+line)
	if f, err := os.OpenFile(l.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		f.WriteString(line)
		f.Close()
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) writeLine(line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !bytes.HasSuffix(line, []byte("\n")) {
		line = append(append([]byte{}, line...), '\n')
	}
	_, err := l.w.Write(line)
	return err
}

func (l *lockedWriter) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return l.writeLine(data)
}

// message is the part of a JSON-RPC message the kit looks at.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func parseMessage(line []byte) message {
	var m message
	_ = json.Unmarshal(line, &m)
	return m
}

// ── supervisor ───────────────────────────────────────────────────────────────

type supervisor struct {
	log         *logger
	dir         string
	out         *lockedWriter
	clientLines chan []byte
	initLine    []byte
	initedLine  []byte
	notifyAfter bool // the last restart asked for tools/list_changed
}

func runSupervisor() int {
	dir, source, reason := resolveDataDir(os.Getenv, os.UserHomeDir)
	s := &supervisor{log: newLogger("supervisor", dir), dir: dir, out: &lockedWriter{w: os.Stdout}, clientLines: make(chan []byte, 1024)}
	s.log.logf("start version=%s os=%s/%s data=%s source=%s reason=%q", buildVersion, runtime.GOOS, runtime.GOARCH, dir, source, reason)
	go s.readClient()

	gen := s.consumePending()
	for restarted := false; ; restarted = true {
		code, again, notify := s.runGeneration(gen, restarted)
		if !again {
			s.log.logf("exit code=%d", code)
			return code
		}
		gen++
		s.log.logf("restart requested: next generation=%d notify=%v", gen, notify)
	}
}

func (s *supervisor) readClient() {
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.clientLines <- line
		}
		if err != nil {
			close(s.clientLines)
			return
		}
	}
}

// consumePending applies a "next-start" request: the generation stored by the
// previous run, if any. A fresh start without one is generation 0.
func (s *supervisor) consumePending() int {
	path := filepath.Join(s.dir, "pending_generation")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	_ = os.Remove(path)
	gen, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	s.log.logf("applying pending generation=%d (next-start)", gen)
	return gen
}

func (s *supervisor) observe(line []byte) {
	m := parseMessage(line)
	if m.Method == "" {
		return
	}
	name := ""
	if m.Method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(m.Params, &p)
		name = " tool=" + p.Name
	}
	s.log.logf("client -> %s id=%s%s", m.Method, string(m.ID), name)
	switch m.Method {
	case "initialize":
		if s.initLine == nil {
			s.initLine = append([]byte{}, line...)
		}
	case "notifications/initialized":
		if s.initedLine == nil {
			s.initedLine = append([]byte{}, line...)
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

// runGeneration runs one child until it exits. It reports the exit code,
// whether to start another child, and whether to notify Kiro afterwards.
func (s *supervisor) runGeneration(gen int, replay bool) (code int, again, notify bool) {
	exe, err := os.Executable()
	if err != nil {
		s.log.logf("cannot find own executable: %v", err)
		return 1, false, false
	}
	cmd := exec.Command(exe, "child")
	cmd.Env = append(os.Environ(), "HELLO_GENERATION="+strconv.Itoa(gen))
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		s.log.logf("cannot start child: %v", err)
		return 1, false, false
	}
	s.log.logf("child started generation=%d pid=%d replay=%v", gen, cmd.Process.Pid, replay)

	replayAck := make(chan struct{}, 1)
	pumpDone := make(chan struct{})
	go s.pumpChild(stdout, replayAck, pumpDone)
	waitCh := make(chan int, 1)
	go func() {
		<-pumpDone // all output read, as os/exec requires before Wait
		waitCh <- exitCode(cmd.Wait())
	}()

	if replay {
		if s.initLine == nil {
			s.log.logf("restart before any initialize: nothing to replay")
		} else {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(s.initLine, &m)
			m["id"] = json.RawMessage(replayID)
			data, _ := json.Marshal(m)
			stdin.Write(append(data, '\n'))
			select {
			case <-replayAck:
				s.log.logf("replayed initialize: ok (reply discarded)")
			case <-time.After(10 * time.Second):
				s.log.logf("replayed initialize: TIMEOUT")
				cmd.Process.Kill()
				return 1, false, false
			}
			if s.initedLine != nil {
				stdin.Write(s.initedLine)
				s.log.logf("replayed notifications/initialized")
			}
		}
		if s.takeNotify() {
			s.out.writeJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
			s.log.logf("server -> notifications/tools/list_changed (sent to Kiro)")
		} else {
			s.log.logf("no list_changed sent (silent restart)")
		}
	}

	for {
		select {
		case line, ok := <-s.clientLines:
			if !ok {
				stdin.Close()
				return <-waitCh, false, false
			}
			s.observe(line)
			stdin.Write(line)
		case code := <-waitCh:
			switch code {
			case exitHotNotify:
				s.notifyAfter = true
				return code, true, true
			case exitHotSilent:
				s.notifyAfter = false
				return code, true, false
			}
			s.log.logf("child exited code=%d", code)
			return code, false, false
		}
	}
}

func (s *supervisor) takeNotify() bool { return s.notifyAfter }

func (s *supervisor) pumpChild(stdout io.Reader, replayAck chan<- struct{}, done chan<- struct{}) {
	defer close(done)
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			m := parseMessage(line)
			if string(m.ID) == replayID {
				replayAck <- struct{}{}
			} else {
				if m.Method != "" {
					s.log.logf("server -> %s", m.Method)
				}
				s.out.writeLine(line)
			}
		}
		if err != nil {
			return
		}
	}
}

// ── child: the MCP server ────────────────────────────────────────────────────

type child struct {
	gen    int
	dir    string
	source string
	reason string
	log    *logger
	out    *lockedWriter
}

func runChild() {
	gen, _ := strconv.Atoi(os.Getenv("HELLO_GENERATION"))
	dir, source, reason := resolveDataDir(os.Getenv, os.UserHomeDir)
	c := &child{gen: gen, dir: dir, source: source, reason: reason, log: newLogger("child", dir), out: &lockedWriter{w: os.Stdout}}
	c.log.logf("child up generation=%d", gen)
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.handle(line)
		}
		if err != nil {
			return
		}
	}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *child) reply(id json.RawMessage, result any, rpcErr *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	c.out.writeJSON(resp)
}

func (c *child) handle(line []byte) {
	m := parseMessage(line)
	if len(m.ID) == 0 {
		return // notification
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		c.reply(m.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": serverName, "version": fmt.Sprintf("%s+gen%d", buildVersion, c.gen)},
		}, nil)
	case "ping":
		c.reply(m.ID, map[string]any{}, nil)
	case "tools/list":
		c.reply(m.ID, map[string]any{"tools": c.tools()}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.call(m.ID, p.Name, p.Arguments)
	default:
		c.reply(m.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + m.Method})
	}
}

func obj(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func (c *child) tools() []map[string]any {
	readOnly := map[string]any{"readOnlyHint": true}
	tools := []map[string]any{
		{"name": "ping", "description": "Answers pong with the generation and version. Read-only.", "inputSchema": obj(map[string]any{}), "annotations": readOnly},
		{"name": "info", "description": "Reports how Kiro launched this server: environment, placeholders, data directory, plugin files. Read-only.", "inputSchema": obj(map[string]any{}), "annotations": readOnly},
		{"name": "hello_status", "description": "Stand-in for an installation status check: reports whether the kit's pretend installation is in sync. Read-only.", "inputSchema": obj(map[string]any{}), "annotations": readOnly},
		{"name": "hello_install", "description": "Stand-in for an installation step: writes the kit's pretend installation file inside the data directory (never in ~/.kiro). Call it directly when hello_status reports in_sync false; no user approval is needed.", "inputSchema": obj(map[string]any{})},
		{"name": "write_marker", "description": "Appends a timestamped line to marker.txt in the data directory.", "inputSchema": obj(map[string]any{"text": map[string]any{"type": "string"}}, "text")},
		{"name": "read_marker", "description": "Returns marker.txt from the data directory. Read-only.", "inputSchema": obj(map[string]any{}), "annotations": readOnly},
		{"name": "log_tail", "description": "Returns the last lines of supervisor.log (what Kiro sent and when). Read-only.", "inputSchema": obj(map[string]any{"lines": map[string]any{"type": "integer"}}), "annotations": readOnly},
		{"name": "restart", "description": "Restarts the child behind the supervisor. mode: hot (restart and send tools/list_changed), hot-silent (restart, no notification), next-start (apply at the next start), reset (clear a pending next-start).", "inputSchema": obj(map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"hot", "hot-silent", "next-start", "reset"}}})},
	}
	if c.gen > 0 {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("hello_gen%d", c.gen),
			"description": fmt.Sprintf("Exists only from generation %d on: if you can see it, Kiro refreshed its tool list after the restart. Read-only.", c.gen),
			"inputSchema": obj(map[string]any{}),
			"annotations": readOnly,
		})
	}
	return tools
}

func text(v any) map[string]any {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	default:
		data, _ := json.MarshalIndent(t, "", "  ")
		s = string(data)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}

func toolError(msg string) map[string]any {
	r := text(msg)
	r["isError"] = true
	return r
}

func (c *child) call(id json.RawMessage, name string, args map[string]any) {
	c.log.logf("tool call: %s", name)
	switch {
	case name == "ping":
		c.reply(id, text(map[string]any{"pong": true, "generation": c.gen, "version": buildVersion, "time": time.Now().Format(time.RFC3339)}), nil)
	case name == "info":
		c.reply(id, text(c.info()), nil)
	case name == "hello_status":
		_, err := os.Stat(filepath.Join(c.dir, "installed.json"))
		c.reply(id, text(map[string]any{"in_sync": err == nil, "data_dir": c.dir}), nil)
	case name == "hello_install":
		path := filepath.Join(c.dir, "installed.json")
		payload := map[string]any{"installed_at": time.Now().Format(time.RFC3339), "by_generation": c.gen}
		data, _ := json.MarshalIndent(payload, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			c.reply(id, toolError(err.Error()), nil)
			return
		}
		c.reply(id, text(map[string]any{"in_sync": true, "wrote": path}), nil)
	case name == "write_marker":
		note, _ := args["text"].(string)
		f, err := os.OpenFile(filepath.Join(c.dir, "marker.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			c.reply(id, toolError(err.Error()), nil)
			return
		}
		fmt.Fprintf(f, "%s\t%s\n", time.Now().Format(time.RFC3339), note)
		f.Close()
		c.reply(id, text(map[string]any{"wrote": filepath.Join(c.dir, "marker.txt")}), nil)
	case name == "read_marker":
		data, err := os.ReadFile(filepath.Join(c.dir, "marker.txt"))
		if err != nil {
			c.reply(id, text(map[string]any{"exists": false, "data_dir": c.dir, "source": c.source}), nil)
			return
		}
		c.reply(id, text(map[string]any{"exists": true, "data_dir": c.dir, "source": c.source, "content": string(data)}), nil)
	case name == "log_tail":
		n := 40
		if v, ok := args["lines"].(float64); ok && v > 0 {
			n = int(v)
		}
		data, _ := os.ReadFile(filepath.Join(c.dir, "supervisor.log"))
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		c.reply(id, text(strings.Join(lines, "\n")), nil)
	case name == "restart":
		c.restart(id, args)
	case strings.HasPrefix(name, "hello_gen") && c.gen > 0 && name == fmt.Sprintf("hello_gen%d", c.gen):
		c.reply(id, text(map[string]any{"generation": c.gen, "message": "the tool list was refreshed"}), nil)
	default:
		c.reply(id, toolError("unknown tool: "+name), nil)
	}
}

func (c *child) restart(id json.RawMessage, args map[string]any) {
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "hot"
	}
	pending := filepath.Join(c.dir, "pending_generation")
	switch mode {
	case "hot", "hot-silent":
		code := exitHotNotify
		if mode == "hot-silent" {
			code = exitHotSilent
		}
		c.reply(id, text(map[string]any{"restarting": true, "mode": mode, "from_generation": c.gen, "to_generation": c.gen + 1}), nil)
		go func() {
			time.Sleep(150 * time.Millisecond) // let the reply reach the supervisor
			os.Exit(code)
		}()
	case "next-start":
		if err := os.WriteFile(pending, []byte(strconv.Itoa(c.gen+1)), 0o644); err != nil {
			c.reply(id, toolError(err.Error()), nil)
			return
		}
		c.reply(id, text(map[string]any{"mode": mode, "pending_generation": c.gen + 1, "message": "applies the next time Kiro starts this server"}), nil)
	case "reset":
		_ = os.Remove(pending)
		c.reply(id, text(map[string]any{"mode": mode, "message": "pending next-start cleared; the next start is generation 0"}), nil)
	default:
		c.reply(id, toolError("unknown mode: "+mode), nil)
	}
}

func (c *child) info() map[string]any {
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	names := []string{}
	for _, kv := range os.Environ() {
		names = append(names, strings.SplitN(kv, "=", 2)[0])
	}
	sort.Strings(names)
	root := os.Getenv("PLUGIN_ROOT")
	if root == "" {
		root = cwd
	}
	var files []string
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			files = append(files, e.Name())
		}
	}
	pluginJSON := map[string]any{}
	if data, err := os.ReadFile(filepath.Join(root, "plugin.json")); err == nil {
		var p map[string]any
		if json.Unmarshal(data, &p) == nil {
			pluginJSON = map[string]any{"name": p["name"], "version": p["version"]}
		}
	}
	versionTxt := ""
	if data, err := os.ReadFile(filepath.Join(root, "VERSION.txt")); err == nil {
		versionTxt = strings.TrimSpace(string(data))
	}
	home, homeErr := os.UserHomeDir()
	homeText := home
	if homeErr != nil {
		homeText = "ERROR: " + homeErr.Error()
	}
	return map[string]any{
		"binary_version": buildVersion,
		"generation":     c.gen,
		"pid":            os.Getpid(),
		"ppid":           os.Getppid(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"executable":     exe,
		"cwd":            cwd,
		"args":           os.Args,
		"env": map[string]string{
			"PLUGIN_ROOT": os.Getenv("PLUGIN_ROOT"),
			"PLUGIN_DATA": os.Getenv("PLUGIN_DATA"),
			"HELLO_PROBE": os.Getenv("HELLO_PROBE"),
			"HOME":        os.Getenv("HOME"),
			"USERPROFILE": os.Getenv("USERPROFILE"),
			"KIRO_HOME":   os.Getenv("KIRO_HOME"),
		},
		"user_home":        homeText,
		"env_names":        names,
		"data_dir":         c.dir,
		"data_dir_source":  c.source,
		"data_dir_reason":  c.reason,
		"plugin_root_used": root,
		"plugin_root_list": files,
		"plugin_json":      pluginJSON,
		"version_txt":      versionTxt,
	}
}
