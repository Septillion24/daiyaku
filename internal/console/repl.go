package console

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/chzyer/readline"
	"github.com/muesli/termenv"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/sequence"
)

var (
	sSend  = lipgloss.NewStyle().Foreground(cMuted)
	sTitle = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sSep   = lipgloss.NewStyle().Foreground(cDim)
	sMuted = lipgloss.NewStyle().Foreground(cMuted)
	sCmd   = lipgloss.NewStyle()                    // command keyword: default text color
	sArg   = lipgloss.NewStyle().Foreground(cMuted) // its args: gray
	sWarn  = lipgloss.NewStyle().Foreground(cWarn)
	sErr   = lipgloss.NewStyle().Foreground(cErr)
	sOK    = lipgloss.NewStyle().Foreground(cGood)
)

type REPL struct {
	Engine     *engine.Engine
	Provider   string
	SessionDir string
	RecordPath string

	in        *bufio.Reader
	out       io.Writer
	rl        *readline.Instance
	prevTools []string

	curReq     *neutral.Request
	shellMode  bool
	shellTool  string
	shellField string
	quit       bool
	lastAuto   int // AutoAnswered() total already reported, to show only new ones
}

func NewREPL(e *engine.Engine, provider, sessionDir, recordPath string) *REPL {
	return &REPL{
		Engine:     e,
		Provider:   provider,
		SessionDir: sessionDir,
		RecordPath: recordPath,
		in:         bufio.NewReader(os.Stdin),
		out:        os.Stdout,
	}
}

func (r *REPL) printf(f string, a ...interface{}) { fmt.Fprintf(r.out, f, a...) }
func (r *REPL) print(s string)                    { io.WriteString(r.out, s) }

func (r *REPL) readLine(prompt string) (line string, ok bool) {
	if r.rl != nil {
		r.rl.SetPrompt(prompt)
		s, err := r.rl.Readline()
		if err == readline.ErrInterrupt {
			// readline's raw mode eats the OS SIGINT, so Ctrl+C only surfaces here.
			// An empty line quits (otherwise there's no way out mid-request); a
			// half-typed line is just discarded.
			if strings.TrimSpace(s) == "" {
				return "", false
			}
			return "", true
		}
		if err != nil {
			return "", false
		}
		return s, true
	}
	r.printf("%s", prompt)
	s, err := r.in.ReadString('\n')
	if err != nil && s == "" {
		return "", false
	}
	return strings.TrimRight(s, "\r\n"), true
}

func (r *REPL) ensureReadline() {
	if r.rl != nil || !stdinIsTerminal() {
		return
	}
	rl, err := readline.NewEx(&readline.Config{
		AutoComplete:      r.completer(),
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistoryFile:       historyFile(),
		HistorySearchFold: true,
	})
	if err == nil {
		r.rl = rl
		// Keep r.out == os.Stdout on purpose. Routing color through rl.Stdout()
		// hits readline's Windows ANSI translator, which panics on the 256-color
		// SGR sequences lipgloss emits (ColorTableFg index out of range). Run()
		// enables virtual-terminal processing so the terminal renders our color;
		// readline only draws its plain prompt.
	}
}

func (r *REPL) closeReadline() {
	if r.rl != nil {
		r.rl.Close()
		r.rl = nil
	}
}

func (r *REPL) Run(ctx context.Context) error {
	// Enable virtual-terminal processing so Windows renders our 256-color output
	// (no-op elsewhere). This is what lets us bypass readline's crashing Win32
	// ANSI translator; see ensureReadline.
	if restore, err := termenv.EnableVirtualTerminalProcessing(termenv.NewOutput(os.Stdout)); err == nil && restore != nil {
		defer func() { _ = restore() }()
	}
	r.ensureReadline()
	defer r.closeReadline()

	hint := `"help" for help`
	if r.rl != nil {
		hint = `"help" or tab for help`
	}
	pcol := providerColor(r.Provider)
	title := lipgloss.NewStyle().Bold(true).Foreground(pcol).Render("daiyaku")
	prov := lipgloss.NewStyle().Foreground(pcol).Render(r.Provider)
	line := title + sSep.Render(" | ") + prov + sSep.Render(" | "+hint)
	r.print("\n" + line + "\n")
	r.print("\n" + sMuted.Render("waiting for the harness to connect...") + "\n\n")

	for {
		ex, err := r.Engine.Next(ctx)
		if err != nil {
			return nil
		}
		action := r.interact(ex.Req)
		r.report(action, ex.Respond(action))
		r.prevTools = ex.Req.ToolNames()
		if r.quit {
			return nil
		}
	}
}

func (r *REPL) completer() readline.AutoCompleter {
	tools := func(string) []string { return r.completeToolNames() }
	dyn := func() *readline.PrefixCompleter { return readline.PcItemDynamic(tools) }
	return readline.NewPrefixCompleter(
		readline.PcItem("call", dyn()),
		readline.PcItem("schema", dyn()),
		readline.PcItem("template", dyn()),
		readline.PcItem("shell", dyn()),
		readline.PcItem("tools"),
		readline.PcItem("sys"),
		readline.PcItem("ctx"),
		readline.PcItem("history"),
		readline.PcItem("last"),
		readline.PcItem("raw"),
		readline.PcItem("reply"),
		readline.PcItem("say"),
		readline.PcItem("text"),
		readline.PcItem("end"),
		readline.PcItem("help"),
		readline.PcItem("exit"),
		readline.PcItem("quit"),
	)
}

func (r *REPL) completeToolNames() []string {
	if r.curReq == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range r.curReq.Tools {
		if !seen[t.Name] {
			seen[t.Name] = true
			out = append(out, t.Name)
		}
	}
	return out
}

func (r *REPL) interact(req *neutral.Request) neutral.Action {
	r.curReq = req
	if r.shellMode {
		return r.shellInteract(req)
	}
	return r.normalInteract(req)
}

func (r *REPL) normalInteract(req *neutral.Request) neutral.Action {
	r.print(sSep.Render(strings.Repeat("─", 72)) + "\n")
	r.printQueueStatus()
	r.print(sTitle.Render("REQUEST ") + Summarize(req) + "\n")
	r.print(RenderTools(req, r.prevTools))
	if w := SideCallWarning(req); w != "" {
		r.print(sWarn.Render("! "+w) + "\n")
	}
	r.print(menuLine("call", "<tool> <json|text>", "run a tool (loop continues)") + "\n")
	r.print(menuLine("reply", "<msg>", "answer in words (ends the turn)") + "\n")
	r.print(menuLine("shell", "", "command mode for a shell/exec tool") + "\n")
	r.print(menuLine("help", "[tool]", "list commands, or one tool's description") + "\n")

	for {
		line, ok := r.readLine(r.opPrompt(req.Seq))
		if !ok {
			r.printf("\n(ending turn and quitting)\n")
			r.quit = true
			return neutral.Action{Kind: neutral.ActionEnd}
		}
		cmd, rest := split2(strings.TrimSpace(line))
		if action, ret := r.handleNormalCmd(req, cmd, rest); ret {
			return action
		}
	}
}

// printQueueStatus surfaces what happened out of band before this request: any
// side-channel calls the engine auto-graded (so the operator knows they went by),
// and how many requests are already queued behind this one (so a backlog is never
// silent — an unanswered request is what stalled the harness in earlier runs).
func (r *REPL) printQueueStatus() {
	if r.Engine == nil {
		return
	}
	if n := r.Engine.AutoAnswered(); n > r.lastAuto {
		r.print(sMuted.Render(fmt.Sprintf("  ⟳ auto-graded %d safety-classifier request(s) since the last turn", n-r.lastAuto)) + "\n")
		r.lastAuto = n
	}
	if w := r.Engine.Waiting() - 1; w > 0 {
		r.print(sWarn.Render(fmt.Sprintf("  ⏳ %d more request(s) waiting behind this one", w)) + "\n")
	}
}

// handleNormalCmd dispatches one normal-mode command. ret=true means the returned
// action should be sent (leaving the prompt loop); ret=false continues the loop.
func (r *REPL) handleNormalCmd(req *neutral.Request, cmd, rest string) (neutral.Action, bool) {
	switch cmd {
	case "call":
		if action, ok := r.buildCall(req, rest); ok {
			return action, true
		}
	case "reply", "say", "text", "end", "msg":
		return neutral.Action{Kind: neutral.ActionEnd, Text: rest}, true
	case "exit", "quit":
		r.quit = true
		return neutral.Action{Kind: neutral.ActionEnd, Text: rest}, true
	case "shell", "sh", "cmd", "pwsh", "powershell":
		if r.enterShell(req, rest) {
			return r.shellInteract(req), true
		}
	case "":
		// blank line: re-prompt
	default:
		if !r.handleToolQuery(req, cmd, rest) {
			r.handleStateQuery(req, cmd)
		}
	}
	return neutral.Action{}, false
}

// handleToolQuery handles the inspect commands that name a tool. It reports
// whether it recognized cmd.
func (r *REPL) handleToolQuery(req *neutral.Request, cmd, rest string) bool {
	switch cmd {
	case "help", "?":
		if rest != "" {
			r.print(RenderToolHelp(req, rest) + "\n")
		} else {
			r.help()
		}
	case "tools":
		r.print(RenderTools(req, r.prevTools))
	case "schema":
		if rest != "" {
			r.print(RenderSchema(req, rest) + "\n")
		} else {
			r.printf("usage: schema <ToolName>\n")
		}
	case "template", "tpl":
		if rest != "" {
			r.print(Template(req, rest) + "\n")
		} else {
			r.printf("usage: template <ToolName>\n")
		}
	default:
		return false
	}
	return true
}

// handleStateQuery handles the inspect commands that show request state, and
// reports unknown commands.
func (r *REPL) handleStateQuery(req *neutral.Request, cmd string) {
	switch cmd {
	case "sys", "system":
		if req.System == "" {
			r.printf("(no system prompt)\n")
		} else {
			r.printf("%s\n", req.System)
		}
	case "ctx", "context", "history", "hist":
		r.print(RenderContext(req, cmd == "ctx" || cmd == "context"))
	case "last":
		r.printLast(req)
	case "raw":
		r.printf("%s\n", string(req.Raw))
	default:
		r.print(sWarn.Render(fmt.Sprintf("unknown command %q (try 'help')", cmd)) + "\n")
	}
}

func (r *REPL) enterShell(req *neutral.Request, rest string) bool {
	name, field := strings.TrimSpace(rest), ""
	if name == "" {
		n, f, found := findShellTool(req)
		if !found {
			r.print(sWarn.Render("! no shell/exec tool offered this turn. Offered: "+strings.Join(req.ToolNames(), ", ")) + "\n")
			r.print(sSep.Render("  force one with:  shell <ToolName>") + "\n")
			return false
		}
		name, field = n, f
	} else if t := req.FindTool(name); t != nil {
		field = primaryField(t.Schema)
	}
	if field == "" {
		// Without a string field we would send {"":"<cmd>"}, which is wrong.
		r.print(sErr.Render(fmt.Sprintf("! %q has no obvious command field; cannot use as a shell (try: template %s)", name, name)) + "\n")
		return false
	}
	r.shellMode, r.shellTool, r.shellField = true, name, field
	r.print(sOK.Render(fmt.Sprintf("→ shell mode via %q (field %q)", name, field)) + "\n")
	r.print(sSep.Render("  ':' prefixes meta-commands (:exit, :help)") + "\n")
	return true
}

func (r *REPL) shellInteract(req *neutral.Request) neutral.Action {
	r.curReq = req
	if req.FindTool(r.shellTool) == nil {
		r.printf("! shell tool %q not offered this turn; leaving shell mode.\n", r.shellTool)
		r.shellMode = false
		return r.normalInteract(req)
	}
	// The prior command's result arrives on this request.
	if res := req.LastResult(); res != nil {
		if res.IsError {
			r.print(sErr.Render("[tool reported error]") + "\n")
		}
		r.printf("%s\n", res.Content)
	}

	for {
		line, ok := r.readLine(r.shellPrompt(r.shellTool, req.Seq))
		if !ok {
			r.printf("\n(ending turn and quitting)\n")
			r.quit = true
			return neutral.Action{Kind: neutral.ActionEnd}
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			if action, ret := r.shellMeta(req, trimmed[1:]); ret {
				return action
			}
			continue
		}
		b, _ := json.Marshal(map[string]string{r.shellField: line})
		return neutral.Action{Kind: neutral.ActionToolCall, ToolName: r.shellTool, ToolInput: b}
	}
}

// ret=true means return the action (or leave the loop); ret=false means continue.
func (r *REPL) shellMeta(req *neutral.Request, s string) (neutral.Action, bool) {
	cmd, rest := split2(s)
	switch cmd {
	case "exit", "leave":
		r.shellMode = false
		r.print(sSep.Render("<- left shell mode.") + "\n")
		return r.normalInteract(req), true
	case "quit":
		r.quit = true
		return neutral.Action{Kind: neutral.ActionEnd, Text: rest}, true
	case "reply", "say", "text", "end":
		return neutral.Action{Kind: neutral.ActionEnd, Text: rest}, true
	case "call":
		if action, ok := r.buildCall(req, rest); ok {
			return action, true
		}
	default:
		r.shellMetaInspect(req, cmd)
	}
	return neutral.Action{}, false
}

// shellMetaInspect handles the read-only shell meta-commands, and reports unknown ones.
func (r *REPL) shellMetaInspect(req *neutral.Request, cmd string) {
	switch cmd {
	case "tools":
		r.print(RenderTools(req, r.prevTools))
	case "ctx", "context", "history":
		r.print(RenderContext(req, true))
	case "sys":
		r.printf("%s\n", req.System)
	case "last":
		r.printLast(req)
	case "raw":
		r.printf("%s\n", string(req.Raw))
	case "help", "?":
		r.shellHelp()
	default:
		r.printf("unknown meta ':%s' (try :help)\n", cmd)
	}
}

func (r *REPL) printLast(req *neutral.Request) {
	if res := req.LastResult(); res != nil {
		if res.IsError {
			r.print(sErr.Render("(error)") + "\n")
		}
		r.printf("%s\n", res.Content)
	} else {
		r.print(sSep.Render("(no tool result yet)") + "\n")
	}
}

func (r *REPL) buildCall(req *neutral.Request, rest string) (neutral.Action, bool) {
	name, jsonPart := split2(rest)
	if name == "" {
		r.printf("usage: call <ToolName> <json-input>   (try: template <ToolName>)\n")
		return neutral.Action{}, false
	}
	if req.FindTool(name) == nil {
		r.print(sWarn.Render(fmt.Sprintf("! warning: %q was not offered this turn; sending anyway", name)) + "\n")
	}
	jsonPart = strings.TrimSpace(jsonPart)
	var input json.RawMessage
	switch {
	case jsonPart == "":
		input = json.RawMessage("{}")
	case strings.HasPrefix(jsonPart, "{"):
		var probe interface{}
		if err := json.Unmarshal([]byte(jsonPart), &probe); err != nil {
			r.print(sErr.Render("! invalid JSON input: "+err.Error()) + "\n")
			return neutral.Action{}, false
		}
		input = json.RawMessage(jsonPart)
	default:
		field := ""
		if t := req.FindTool(name); t != nil {
			field = primaryField(t.Schema)
		}
		if field == "" {
			r.print(sErr.Render(fmt.Sprintf("! %s needs JSON input (no obvious text field); try: template %s", name, name)) + "\n")
			return neutral.Action{}, false
		}
		b, _ := json.Marshal(map[string]string{field: jsonPart})
		input = b
		r.print(sSep.Render("  (shorthand -> "+string(b)+")") + "\n")
	}
	return neutral.Action{Kind: neutral.ActionToolCall, ToolName: name, ToolInput: input}, true
}

// report tells the operator what actually happened to the action they authored
// and appends it to the --record chain. An action the harness never received (it
// disconnected or timed out while the operator was composing) is called out and
// deliberately NOT recorded: a recorded step is a claim that the harness ran it,
// and that claim would be false.
func (r *REPL) report(a neutral.Action, delivered bool) {
	if !delivered {
		r.print(sErr.Render("! NOT DELIVERED: the harness stopped waiting for this reply before you sent it.") + "\n")
		r.print(sSep.Render("  Nothing ran and nothing was recorded. Re-author it on the next request.") + "\n")
		return
	}
	switch a.Kind {
	case neutral.ActionToolCall:
		r.print(sSend.Render(fmt.Sprintf("-> sent  %s %s", a.ToolName, string(a.ToolInput))) + "\n")
	default:
		r.print(sSend.Render(fmt.Sprintf("-> reply sent (turn ends)  %q", a.Text)) + "\n")
	}
	if r.RecordPath != "" {
		if err := sequence.AppendStep(r.RecordPath, sequence.FromAction(a, "")); err != nil {
			r.print(sErr.Render("! failed to record step: "+err.Error()) + "\n")
		}
	}
}

func (r *REPL) help() {
	r.print("\n" + menuHead("ACT", "loop continues") + "\n")
	r.print(menuLine("call", "<tool> <json|text>", "run a tool; bare text fills its main field") + "\n")
	r.print(menuLine("shell", "[tool]", "type straight to a shell/exec tool") + "\n")

	r.print("\n" + menuHead("REPLY", "ends the turn") + "\n")
	r.print(menuLine("reply", "<msg>", "aliases: say, text, end") + "\n")

	r.print("\n" + menuHead("INSPECT", "no effect on the harness") + "\n")
	r.print(menuLine("tools", "", "offered tool names (changes flagged)") + "\n")
	r.print(menuLine("help", "[tool]", "this menu, or one tool's description") + "\n")
	r.print(menuLine("schema", "<tool>", "a tool's full input schema") + "\n")
	r.print(menuLine("template", "<tool>", "JSON skeleton for a tool's input") + "\n")
	r.print(menuLine("sys", "", "system prompt") + "\n")
	r.print(menuLine("ctx", "| history", "conversation (ctx includes system prompt)") + "\n")
	r.print(menuLine("last", "", "most recent tool result") + "\n")
	r.print(menuLine("raw", "", "raw inbound request JSON") + "\n")
	r.print(menuLine("exit", "| quit", "end the turn and quit") + "\n")

	r.print("\n" + sSep.Render("  Tab completes commands and tool names; Ctrl+C on an empty line, or Ctrl+D, quits.") + "\n\n")
}

func (r *REPL) shellHelp() {
	r.print("\n" + menuHead("SHELL MODE", "each line runs through "+r.shellTool) + "\n")
	r.print(menuLine(":exit", "", "back to the normal console") + "\n")
	r.print(menuLine(":reply", "[msg]", "answer in words, end the turn (:say :text :end)") + "\n")
	r.print(menuLine(":call", "<tool> <input>", "one-off call to a different tool") + "\n")
	r.print(menuLine(":tools", "| :ctx", "offered tools / conversation") + "\n")
	r.print(menuLine(":last", "| :raw | :sys", "last result / raw request / system prompt") + "\n")
	r.print(menuLine(":quit", "", "quit the console") + "\n\n")
}

func findShellTool(req *neutral.Request) (name, field string, found bool) {
	shells := map[string]bool{
		"bash": true, "exec_command": true, "shell": true, "run_command": true,
		"run_terminal_cmd": true, "execute": true, "terminal": true, "powershell": true,
	}
	for _, t := range req.Tools {
		if shells[strings.ToLower(t.Name)] {
			return t.Name, primaryField(t.Schema), true
		}
	}
	return "", "", false
}

// providerColor tints the header by provider so the operator can tell at a
// glance which harness a session is driving. This is drawn via r.print with VT
// processing on, not through the readline prompt, so full 256-color is safe.
func providerColor(p string) lipgloss.Color {
	switch strings.ToLower(p) {
	case "anthropic":
		return lipgloss.Color("208") // orange
	case "gemini", "google":
		return lipgloss.Color("37") // teal
	case "openai":
		return lipgloss.Color("42") // green
	default:
		return cAccent // blue
	}
}

// menuHead renders a section heading for the command menu: the section name in
// the accent color, with a short parenthetical note in gray.
func menuHead(name, note string) string {
	line := sTitle.Render(name)
	if note != "" {
		line += sSep.Render("  (" + note + ")")
	}
	return line
}

// menuLine renders one command-menu row: the command keyword in white, its
// argument placeholder and description in gray, with the descriptions aligned.
func menuLine(cmd, args, desc string) string {
	left := sCmd.Render(cmd)
	if args != "" {
		left += " " + sArg.Render(args)
	}
	pad := 26 - lipgloss.Width(left)
	if pad < 1 {
		pad = 1
	}
	return "  " + left + strings.Repeat(" ", pad) + sSep.Render(desc)
}

// opPrompt is the normal prompt: "#<seq> >", where <seq> matches the REQUEST
// header just above so console lines cross-reference the transcript. Color is
// added only in interactive (readline) mode, and only with 16-color ANSI: the
// Windows readline translator panics on the 256-color codes lipgloss emits.
func (r *REPL) opPrompt(seq int) string {
	if r.rl == nil {
		return fmt.Sprintf("#%d > ", seq)
	}
	return fmt.Sprintf("\x1b[90m#%d\x1b[0m \x1b[1;36m>\x1b[0m ", seq)
}

// shellPrompt is the shell-mode prompt: "<tool> #<seq> $", the tool name and $
// in green to signal you are typing straight to a shell/exec tool.
func (r *REPL) shellPrompt(tool string, seq int) string {
	if r.rl == nil {
		return fmt.Sprintf("%s #%d $ ", tool, seq)
	}
	return fmt.Sprintf("\x1b[1;32m%s\x1b[0m\x1b[90m #%d\x1b[0m \x1b[1;32m$\x1b[0m ", tool, seq)
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func split2(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// historyFile keeps command history in the user's config dir rather than the
// per-run evidence directory. Under runs/<ts>/ every session started with an
// empty history (the point of having it is recalling last run's commands), and
// it left a non-evidence file in the folder handed to the client.
func historyFile() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return "" // readline treats empty as "no history file"
	}
	dir := filepath.Join(base, "daiyaku")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return filepath.Join(dir, "repl_history")
}
