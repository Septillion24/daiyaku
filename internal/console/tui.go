package console

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"daiyaku/internal/engine"
	"daiyaku/internal/neutral"
	"daiyaku/internal/sequence"
)

type TUI struct {
	eng        *engine.Engine
	provider   string
	sessionDir string
	recordPath string
}

func NewTUI(e *engine.Engine, provider, sessionDir, recordPath string) Console {
	return &TUI{eng: e, provider: provider, sessionDir: sessionDir, recordPath: recordPath}
}

func (t *TUI) Run(ctx context.Context) error {
	m := newModel(t)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))

	go func() {
		for {
			ex, err := t.eng.Next(ctx)
			if err != nil {
				return
			}
			p.Send(exchangeMsg{ex})
		}
	}()

	_, err := p.Run()
	if err == tea.ErrProgramKilled || err == context.Canceled {
		return nil
	}
	return err
}

type exchangeMsg struct{ ex *engine.Exchange }

type focusArea int

const (
	focusComposer focusArea = iota
	focusTools
	focusContext
)

type composeMode int

const (
	modeTool composeMode = iota
	modeText
)

var (
	cAccent  = lipgloss.Color("39")  // blue
	cGood    = lipgloss.Color("42")  // green
	cWarn    = lipgloss.Color("214") // amber
	cErr     = lipgloss.Color("203") // red
	cMuted   = lipgloss.Color("245")
	cDim     = lipgloss.Color("240")
	cWhite   = lipgloss.Color("231")
	stHeader = lipgloss.NewStyle().Bold(true).Foreground(cWhite).Background(cAccent).Padding(0, 1)
	stPane   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim)
	stPaneHi = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent)
	stTitle  = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stMuted  = lipgloss.NewStyle().Foreground(cMuted)
	stStatus = lipgloss.NewStyle().Foreground(cMuted)
)

const (
	composerMinRows = 2
	composerMaxRows = 10
	minTUIWidth     = 40
	minTUIHeight    = 14
)

type model struct {
	t             *TUI
	width, height int
	ready         bool

	ex        *engine.Exchange
	prevTools []string

	ctxView  viewport.Model
	composer textarea.Model
	tools    []neutral.ToolDef
	toolIdx  int

	leftInner    int
	rightInner   int
	bodyInner    int
	composerRows int

	focus       focusArea
	compose     composeMode
	showSystem  bool
	tooSmall    bool
	status      string
	statusStyle lipgloss.Style
	sent        int
}

func newModel(t *TUI) model {
	ta := textarea.New()
	ta.Placeholder = "type a command"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Prompt = "│ "
	ta.Focus()

	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	return model{
		t:            t,
		composer:     ta,
		ctxView:      vp,
		focus:        focusComposer,
		compose:      modeTool,
		composerRows: composerMinRows,
		status:       "waiting for the harness to connect…",
		statusStyle:  stStatus,
	}
}

func (m model) Init() tea.Cmd { return textarea.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		if m.ex != nil {
			m.refreshContext()
		}

	case tea.MouseMsg:
		// mouse wheel scrolls the context pane regardless of focus
		var cmd tea.Cmd
		m.ctxView, cmd = m.ctxView.Update(msg)
		return m, cmd

	case exchangeMsg:
		m.ex = msg.ex
		m.tools = msg.ex.Req.Tools
		m.toolIdx = pickDefaultTool(m.tools)
		m.status = fmt.Sprintf("AWAITING OPERATOR · request #%d", msg.ex.Req.Seq)
		m.statusStyle = lipgloss.NewStyle().Foreground(cGood).Bold(true)
		m.refreshContext()
		return m, nil

	case tea.KeyMsg:
		if nm, cmd, handled := m.handleKey(msg); handled {
			return nm, cmd
		}
	}

	switch m.focus {
	case focusComposer:
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		cmds = append(cmds, cmd)
		m.resizeComposer()
	case focusContext:
		var cmd tea.Cmd
		m.ctxView, cmd = m.ctxView.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *model) handleKey(msg tea.KeyMsg) (model, tea.Cmd, bool) {
	switch msg.String() {
	case "ctrl+c":
		return *m, tea.Quit, true
	case "enter":
		if m.focus == focusComposer {
			return *m, m.send(false), true
		}
	case "alt+enter":
		// Alt+Enter adds a newline and grows the box. Shift+Enter can't be used:
		// the terminal delivers it as a plain Enter, indistinguishable from send.
		if m.focus == focusComposer {
			m.composer.InsertString("\n")
			m.resizeComposer()
			return *m, nil, true
		}
	case "tab":
		m.focus = (m.focus + 1) % 3
		m.syncFocus()
		return *m, nil, true
	case "shift+tab":
		m.focus = (m.focus + 2) % 3
		m.syncFocus()
		return *m, nil, true
	case "ctrl+t":
		m.loadTemplate()
		return *m, nil, true
	case "ctrl+g":
		if m.compose == modeTool {
			m.compose = modeText
			m.composer.Placeholder = "type a reply"
		} else {
			m.compose = modeTool
			m.composer.Placeholder = "type a command"
		}
		return *m, nil, true
	case "ctrl+s":
		return *m, m.send(false), true
	case "ctrl+e":
		return *m, m.send(true), true
	case "ctrl+r":
		m.refreshContext()
		return *m, nil, true
	case "s":
		if m.focus != focusComposer {
			m.showSystem = !m.showSystem
			m.refreshContext()
			return *m, nil, true
		}
	case "pgup":
		m.ctxView.HalfViewUp()
		return *m, nil, true
	case "pgdown":
		m.ctxView.HalfViewDown()
		return *m, nil, true
	}

	if m.focus == focusTools {
		switch msg.String() {
		case "up", "k":
			if m.toolIdx > 0 {
				m.toolIdx--
			}
			return *m, nil, true
		case "down", "j":
			if m.toolIdx < len(m.tools)-1 {
				m.toolIdx++
			}
			return *m, nil, true
		case "enter":
			m.loadTemplate()
			m.focus = focusComposer
			m.syncFocus()
			return *m, nil, true
		}
	}
	return *m, nil, false
}

func (m *model) syncFocus() {
	if m.focus == focusComposer {
		m.composer.Focus()
	} else {
		m.composer.Blur()
	}
}

func (m *model) resizeComposer() {
	if !m.ready || m.tooSmall {
		return
	}
	prev := m.composerRows
	m.layout()
	if m.composerRows != prev && m.ex != nil {
		m.refreshContext()
	}
}

func (m *model) selectedTool() *neutral.ToolDef {
	if m.toolIdx >= 0 && m.toolIdx < len(m.tools) {
		return &m.tools[m.toolIdx]
	}
	return nil
}

func (m *model) loadTemplate() {
	if m.ex == nil || m.compose != modeTool {
		return
	}
	if t := m.selectedTool(); t != nil {
		m.composer.SetValue(TemplateCompact(m.ex.Req, t.Name))
		m.resizeComposer()
	}
}

func (m *model) send(end bool) tea.Cmd {
	if m.ex == nil {
		m.flash(cWarn, "no pending request")
		return nil
	}
	var action neutral.Action

	if m.compose == modeText {
		// A plain-words reply always ends the turn; only a tool call continues the loop.
		action = neutral.Action{Kind: neutral.ActionEnd, Text: m.composer.Value()}
	} else {
		t := m.selectedTool()
		if t == nil {
			m.flash(cWarn, "select a tool first (Tab, then j/k)")
			return nil
		}
		raw := strings.TrimSpace(m.composer.Value())
		var input json.RawMessage
		switch {
		case raw == "":
			input = json.RawMessage("{}")
		case strings.HasPrefix(raw, "{"):
			var probe interface{}
			if err := json.Unmarshal([]byte(raw), &probe); err != nil {
				m.flash(cErr, "invalid JSON: "+err.Error())
				return nil
			}
			input = json.RawMessage(raw)
		default:
			field := primaryField(t.Schema)
			if field == "" {
				m.flash(cErr, t.Name+" needs JSON input (no obvious text field); Ctrl+T for a template")
				return nil
			}
			b, _ := json.Marshal(map[string]string{field: raw})
			input = b
		}
		action = neutral.Action{Kind: neutral.ActionToolCall, ToolName: t.Name, ToolInput: input}
	}

	ex := m.ex
	m.prevTools = ex.Req.ToolNames()
	ex.Respond(action)
	m.record(action)
	m.sent++
	m.ex = nil
	m.tools = nil
	m.composer.Reset()
	m.resizeComposer()
	m.status = "waiting for the harness…"
	m.statusStyle = stStatus
	m.ctxView.SetContent(m.wrap(m.sentSummary(action)))
	m.ctxView.GotoTop()
	return nil
}

func (m *model) record(a neutral.Action) {
	if m.t.recordPath == "" {
		return
	}
	_ = sequence.AppendStep(m.t.recordPath, sequence.FromAction(a, ""))
}

func (m *model) flash(c lipgloss.Color, s string) {
	m.status = s
	m.statusStyle = lipgloss.NewStyle().Foreground(c).Bold(true)
}

func (m *model) sentSummary(a neutral.Action) string {
	var b strings.Builder
	switch a.Kind {
	case neutral.ActionToolCall:
		b.WriteString("→ sent\n\n")
		fmt.Fprintf(&b, "%s\n%s\n", a.ToolName, string(a.ToolInput))
		b.WriteString("\nwaiting for the next request…")
	default:
		b.WriteString("→ reply sent (turn ends)\n\n")
		fmt.Fprintf(&b, "%q\n", a.Text)
	}
	return b.String()
}

func (m *model) refreshContext() {
	if m.ex == nil {
		return
	}
	body := RenderContext(m.ex.Req, m.showSystem)
	if !m.showSystem && m.ex.Req.System != "" {
		body = fmt.Sprintf("(system prompt hidden, %d chars, press 's' to view)\n\n%s",
			len(m.ex.Req.System), body)
	}
	m.ctxView.SetContent(m.wrap(body))
	m.ctxView.GotoBottom()
}

func (m *model) wrap(s string) string { return wrapText(s, m.leftInner) }

// Vertical budget: header(1) + body(border2+title1+inner) + composer(border2+title1
// +rows) + status(1), plus one safety line, so bodyInner = height - composerRows - 9.
func (m *model) layout() {
	if m.width < minTUIWidth || m.height < minTUIHeight {
		m.tooSmall = true
		return
	}
	m.tooSmall = false

	// Grow from composerMinRows as lines are added, capped by composerMaxRows and
	// by whatever height leaves the body pane at least one row.
	m.composer.SetWidth(m.width - 4)
	maxRows := m.height - 10
	if maxRows > composerMaxRows {
		maxRows = composerMaxRows
	}
	if maxRows < composerMinRows {
		maxRows = composerMinRows
	}
	rows := m.composer.LineCount()
	if rows < composerMinRows {
		rows = composerMinRows
	}
	if rows > maxRows {
		rows = maxRows
	}
	m.composerRows = rows
	m.composer.SetHeight(rows)

	m.bodyInner = m.height - m.composerRows - 9
	if m.bodyInner < 1 {
		m.bodyInner = 1
	}
	leftBox := m.width * 6 / 10
	if leftBox < 24 {
		leftBox = 24
	}
	rightBox := m.width - leftBox
	if rightBox < 18 {
		rightBox = 18
		leftBox = m.width - rightBox
	}
	m.leftInner = leftBox - 2
	m.rightInner = rightBox - 2
	if m.leftInner < 8 {
		m.leftInner = 8
	}
	if m.rightInner < 8 {
		m.rightInner = 8
	}
	m.ctxView.Width = m.leftInner
	m.ctxView.Height = m.bodyInner
}

func (m model) View() string {
	if !m.ready {
		return "starting operator console…"
	}
	if m.tooSmall {
		msg := fmt.Sprintf("daiyaku: terminal too small (%dx%d).\nResize to at least %dx%d, or use --mode repl.",
			m.width, m.height, minTUIWidth, minTUIHeight)
		return lipgloss.NewStyle().Width(max(1, m.width)).Render(msg)
	}
	header := m.headerView()
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.contextPane(), m.toolsPane())
	composer := m.composerPane()
	status := m.statusStyle.Render(" " + truncate(oneLine(m.status), m.width-2))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, composer, status)
}

func (m model) headerView() string {
	model := "-"
	seq := 0
	if m.ex != nil {
		model = m.ex.Req.Model
		seq = m.ex.Req.Seq
	}
	rec := ""
	if m.t.recordPath != "" {
		rec = " · REC ●"
	}
	left := stHeader.Render(fmt.Sprintf("daiyaku · %s", m.t.provider))
	rightPlain := fmt.Sprintf("model=%s  req=#%d  sent=%d%s  ·  Tab focus · Ctrl+G act/reply · Ctrl+C quit", model, seq, m.sent, rec)
	avail := m.width - lipgloss.Width(left) - 1
	right := stMuted.Render(truncate(rightPlain, max(0, avail)))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// fitBorder sizes content to exactly w×h (lipgloss MaxHeight guarantees a pane
// never overflows its rows/cols), then draws the pane border.
func fitBorder(content string, w, h int, focused bool) string {
	inner := lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(content)
	st := stPane
	if focused {
		st = stPaneHi
	}
	return st.Render(inner)
}

func fitBorderColor(content string, w, h int, borderColor lipgloss.Color) string {
	inner := lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(content)
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Render(inner)
}

func (m model) contextPane() string {
	title := stTitle.Render("context")
	sysHint := "  s:show system"
	if m.showSystem {
		sysHint = "  s:hide system"
	}
	if m.focus == focusContext {
		title += stMuted.Render("  ↑/↓ PgUp/PgDn scroll" + sysHint)
	} else {
		title += stMuted.Render("  (Tab here to scroll" + sysHint + ")")
	}
	return fitBorder(title+"\n"+m.ctxView.View(), m.leftInner, m.bodyInner+1, m.focus == focusContext)
}

func (m model) toolsPane() string {
	w := m.rightInner
	var b strings.Builder
	title := stTitle.Render("offered tools")
	if m.focus == focusTools {
		title += stMuted.Render("  j/k · enter")
	}
	b.WriteString(title + "\n")
	if m.ex == nil {
		b.WriteString(stMuted.Render("(no request yet)"))
		return fitBorder(b.String(), w, m.bodyInner+1, m.focus == focusTools)
	}
	prev := map[string]bool{}
	for _, n := range m.prevTools {
		prev[n] = true
	}
	listRows := m.bodyInner - 4
	if listRows < 1 {
		listRows = 1
	}
	for i, t := range m.tools {
		if i >= listRows {
			fmt.Fprintf(&b, stMuted.Render("  … %d more (j/k)")+"\n", len(m.tools)-i)
			break
		}
		label := t.Label()
		suffix := ""
		if t.Kind != "" && t.Kind != "function" {
			suffix = " " + stMuted.Render("["+t.Kind+"]")
		}
		newMark := "  "
		if len(m.prevTools) > 0 && !prev[label] {
			newMark = lipgloss.NewStyle().Foreground(cGood).Render("+ ")
		}
		line := truncate(label, w-4)
		if i == m.toolIdx {
			cur := lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("▸ ")
			line = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Bold(true).Render(line)
			b.WriteString(cur + line + suffix + "\n")
		} else {
			b.WriteString(newMark + line + suffix + "\n")
		}
	}
	if t := m.selectedTool(); t != nil {
		b.WriteString("\n" + stMuted.Render(strings.Repeat("─", max(4, w-2))) + "\n")
		b.WriteString(stTitle.Render(truncate(t.Label(), w-2)) + "\n")
		if f := primaryField(t.Schema); f != "" {
			b.WriteString(stMuted.Render("bare text → \""+f+"\"") + "\n")
		}
		b.WriteString(wrapText(oneLine(t.Description), w-2))
	}
	return fitBorder(b.String(), w, m.bodyInner+1, m.focus == focusTools)
}

func (m model) composerPane() string {
	// Frame + label color track the mode: blue = ACT, amber = REPLY.
	label := "ACT - call a tool"
	hints := "  Enter run · Ctrl+G reply · Alt+Enter newline"
	modeColor := cAccent
	if m.compose == modeText {
		label = "REPLY - answer in words (ends the turn)"
		hints = "  Enter send · Ctrl+G act · Alt+Enter newline"
		modeColor = cWarn
	}
	sel := ""
	if t := m.selectedTool(); t != nil && m.compose == modeTool {
		sel = stMuted.Render("  · calls ") + stTitle.Foreground(modeColor).Render(t.Label())
	}
	border := modeColor
	if m.focus != focusComposer {
		border = cDim
	}
	title := stTitle.Foreground(modeColor).Render(label) + stMuted.Render(hints) + sel
	return fitBorderColor(title+"\n"+m.composer.View(), m.width-2, m.composerRows+1, border)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func wrapText(s string, w int) string {
	if w < 8 {
		w = 8
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for len([]rune(line)) > w {
			r := []rune(line)
			cut := w
			// break at the last space in the window
			for i := w; i > w/2; i-- {
				if r[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, string(r[:cut]))
			line = strings.TrimPrefix(string(r[cut:]), " ")
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func primaryField(schema json.RawMessage) string {
	var s struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		return ""
	}
	for _, r := range s.Required {
		if p, ok := s.Properties[r]; ok && (p.Type == "string" || p.Type == "") {
			return r
		}
	}
	for _, n := range []string{"command", "cmd", "prompt", "query", "content", "path", "file_path", "url"} {
		if p, ok := s.Properties[n]; ok && (p.Type == "string" || p.Type == "") {
			return n
		}
	}
	for n, p := range s.Properties {
		if p.Type == "string" {
			return n
		}
	}
	return ""
}

func pickDefaultTool(tools []neutral.ToolDef) int {
	shells := map[string]bool{
		"bash": true, "exec_command": true, "shell": true, "run_command": true,
		"run_terminal_cmd": true, "execute": true, "terminal": true, "powershell": true,
	}
	for i, t := range tools {
		if shells[strings.ToLower(t.Name)] {
			return i
		}
	}
	return 0
}
