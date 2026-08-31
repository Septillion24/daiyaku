package console

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	queue     []*engine.Exchange // requests that arrived while another was unanswered
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
		// A harness can have several calls in flight (subagents, side-channel
		// calls). Queue them: overwriting the one on screen would leave its HTTP
		// handler blocked until the harness timed out, with no sign anything was
		// lost.
		if m.ex != nil {
			m.queue = append(m.queue, msg.ex)
			m.status = fmt.Sprintf("AWAITING OPERATOR · request #%d%s", m.ex.Req.Seq, m.queueSuffix())
			return m, nil
		}
		m.adopt(msg.ex)
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
	if msg.String() == "ctrl+c" {
		return *m, tea.Quit, true
	}
	if cmd, ok := m.handleComposerKey(msg); ok {
		return *m, cmd, true
	}
	if cmd, ok := m.handleNavKey(msg); ok {
		return *m, cmd, true
	}
	if m.focus == focusTools {
		if cmd, ok := m.handleToolsKey(msg); ok {
			return *m, cmd, true
		}
	}
	return *m, nil, false
}

// handleComposerKey handles send/compose keys, which act only while the composer
// is the focus (enter/alt+enter) or regardless of focus (mode toggle, ctrl+s/e).
func (m *model) handleComposerKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "enter":
		if m.focus == focusComposer {
			return m.send(false), true
		}
	case "alt+enter":
		// Alt+Enter adds a newline and grows the box. Shift+Enter can't be used:
		// the terminal delivers it as a plain Enter, indistinguishable from send.
		if m.focus == focusComposer {
			m.composer.InsertString("\n")
			m.resizeComposer()
			return nil, true
		}
	case "ctrl+g":
		if m.compose == modeTool {
			m.compose = modeText
			m.composer.Placeholder = "type a reply"
		} else {
			m.compose = modeTool
			m.composer.Placeholder = "type a command"
		}
		return nil, true
	case "ctrl+s":
		return m.send(false), true
	case "ctrl+e":
		return m.send(true), true
	}
	return nil, false
}

// handleNavKey handles focus, template, refresh, system-toggle and scroll keys.
func (m *model) handleNavKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "tab":
		m.focus = (m.focus + 1) % 3
		m.syncFocus()
		return nil, true
	case "shift+tab":
		m.focus = (m.focus + 2) % 3
		m.syncFocus()
		return nil, true
	case "ctrl+t":
		m.loadTemplate()
		return nil, true
	case "ctrl+r":
		m.refreshContext()
		return nil, true
	case "s":
		if m.focus != focusComposer {
			m.showSystem = !m.showSystem
			m.refreshContext()
			return nil, true
		}
	case "pgup":
		m.ctxView.HalfViewUp()
		return nil, true
	case "pgdown":
		m.ctxView.HalfViewDown()
		return nil, true
	}
	return nil, false
}

// handleToolsKey handles list navigation, active only while the tools pane is focused.
func (m *model) handleToolsKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "up", "k":
		if m.toolIdx > 0 {
			m.toolIdx--
		}
		return nil, true
	case "down", "j":
		if m.toolIdx < len(m.tools)-1 {
			m.toolIdx++
		}
		return nil, true
	case "enter":
		m.loadTemplate()
		m.focus = focusComposer
		m.syncFocus()
		return nil, true
	}
	return nil, false
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

// send delivers what the operator composed. asText forces the plain-words path
// (Ctrl+E) whatever the composer mode is: without it the key that is documented
// as "end the turn in words" would fall through to the tool path and execute the
// operator's sentence as a shell command.
func (m *model) send(asText bool) tea.Cmd {
	if m.ex == nil {
		m.flash(cWarn, "no pending request")
		return nil
	}
	var action neutral.Action

	if m.compose == modeText || asText {
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
	delivered := ex.Respond(action)
	m.composer.Reset()
	m.resizeComposer()
	if !delivered {
		// The harness disconnected or timed out while this was being composed.
		// Nothing ran, so nothing is recorded: a recorded step claims the harness
		// executed it, and that claim would be false.
		m.dropCurrent()
		m.flash(cErr, "NOT DELIVERED: harness stopped waiting. Nothing ran, nothing recorded.")
		m.ctxView.SetContent(m.wrap(m.undeliveredSummary(action)))
		m.ctxView.GotoTop()
		return nil
	}
	m.record(action)
	m.sent++
	m.ctxView.SetContent(m.wrap(m.sentSummary(action)))
	m.ctxView.GotoTop()
	m.dropCurrent()
	return nil
}

// dropCurrent releases the answered request and pulls up the next one the
// harness has queued behind it, if any.
func (m *model) dropCurrent() {
	m.ex = nil
	m.tools = nil
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		m.adopt(next)
		return
	}
	m.status = "waiting for the harness…"
	m.statusStyle = stStatus
}

// adopt puts an exchange on screen as the one awaiting an answer.
func (m *model) adopt(ex *engine.Exchange) {
	m.ex = ex
	m.tools = ex.Req.Tools
	m.toolIdx = pickDefaultTool(m.tools)
	m.status = fmt.Sprintf("AWAITING OPERATOR · request #%d%s", ex.Req.Seq, m.queueSuffix())
	m.statusStyle = lipgloss.NewStyle().Foreground(cGood).Bold(true)
	if w := SideCallWarning(ex.Req); w != "" {
		m.flash(cWarn, "no tools offered: "+w)
	}
	m.refreshContext()
}

func (m *model) queueSuffix() string {
	if len(m.queue) == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d more queued", len(m.queue))
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

// undeliveredSummary explains a reply that never reached the harness, so the
// pane does not read as if the action ran.
func (m *model) undeliveredSummary(a neutral.Action) string {
	var b strings.Builder
	b.WriteString("! NOT DELIVERED\n\n")
	b.WriteString("The harness stopped waiting for this reply (it disconnected or\ntimed out) before you sent it. Nothing ran, nothing was recorded.\n\n")
	switch a.Kind {
	case neutral.ActionToolCall:
		fmt.Fprintf(&b, "discarded: %s %s\n", a.ToolName, string(a.ToolInput))
	default:
		fmt.Fprintf(&b, "discarded reply: %q\n", a.Text)
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

	m.layoutComposer()
	m.bodyInner = m.height - m.composerRows - 9
	if m.bodyInner < 1 {
		m.bodyInner = 1
	}
	m.layoutPanes()
	m.ctxView.Width = m.leftInner
	m.ctxView.Height = m.bodyInner
}

// layoutComposer sizes the composer: it grows from composerMinRows as lines are
// added, capped by composerMaxRows and by whatever height leaves the body pane
// at least one row.
func (m *model) layoutComposer() {
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
}

// layoutPanes splits the width into the left context pane and right tools pane,
// enforcing minimum widths for each.
func (m *model) layoutPanes() {
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
	queue := ""
	if m.t.eng != nil {
		// Requests queued behind the one on screen (which is itself in-flight while
		// m.ex is set), and side-channel calls the engine graded automatically.
		q := m.t.eng.Waiting()
		if m.ex != nil && q > 0 {
			q--
		}
		if q > 0 {
			queue += fmt.Sprintf("  q=%d", q)
		}
		if a := m.t.eng.AutoAnswered(); a > 0 {
			queue += fmt.Sprintf("  auto=%d", a)
		}
	}
	rightPlain := fmt.Sprintf("model=%s  req=#%d  sent=%d%s%s  ·  Tab focus · Ctrl+G act/reply · Ctrl+C quit", model, seq, m.sent, queue, rec)
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
	removed := m.removedTools()
	listRows := m.bodyInner - 4
	if len(removed) > 0 {
		listRows-- // the "no longer offered" line is paid for out of the list
	}
	if listRows < 1 {
		listRows = 1
	}
	visible := listRows
	if len(m.tools) > listRows {
		visible = listRows - 1 // one row pays for the scroll marker
		if visible < 1 {
			visible = 1
		}
	}
	first := m.toolListStart(visible)
	last := first + visible
	if last > len(m.tools) {
		last = len(m.tools)
	}
	for i := first; i < last; i++ {
		b.WriteString(m.toolRow(i, m.tools[i], prev, w))
	}
	if first > 0 || last < len(m.tools) {
		b.WriteString(stMuted.Render(fmt.Sprintf("  %d above · %d below (j/k)",
			first, len(m.tools)-last)) + "\n")
	}
	if len(removed) > 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(cWarn).Render(
			truncate("- gone: "+strings.Join(removed, ", "), w-2)) + "\n")
	}
	b.WriteString(m.toolDetail(w))
	return fitBorder(b.String(), w, m.bodyInner+1, m.focus == focusTools)
}

// removedTools returns the tools offered last turn but not this one. A tool
// disappearing is as much a finding as one appearing, but it has no row of its
// own to carry a marker, so the pane reports the set on one line instead.
func (m model) removedTools() []string {
	if len(m.prevTools) == 0 {
		return nil
	}
	cur := make(map[string]bool, len(m.tools))
	for _, t := range m.tools {
		cur[t.Label()] = true
	}
	var out []string
	for _, n := range m.prevTools {
		if !cur[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// toolRow renders one row of the offered-tools list: a selection marker or a
// "new this turn" marker, the (truncated) label, and any non-function kind tag.
func (m model) toolRow(i int, t neutral.ToolDef, prev map[string]bool, w int) string {
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
		return cur + line + suffix + "\n"
	}
	return newMark + line + suffix + "\n"
}

// toolDetail renders the footer describing the currently selected tool, or "" if none.
func (m model) toolDetail(w int) string {
	t := m.selectedTool()
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n" + stMuted.Render(strings.Repeat("─", max(4, w-2))) + "\n")
	b.WriteString(stTitle.Render(truncate(t.Label(), w-2)) + "\n")
	if f := primaryField(t.Schema); f != "" {
		b.WriteString(stMuted.Render("bare text → \""+f+"\"") + "\n")
	}
	b.WriteString(wrapText(oneLine(t.Description), w-2))
	return b.String()
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

type propType struct {
	Type string `json:"type"`
}

func primaryField(schema json.RawMessage) string {
	var s struct {
		Properties map[string]propType `json:"properties"`
		Required   []string            `json:"required"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		return ""
	}
	for _, r := range s.Required {
		if schemaTextField(s.Properties, r) {
			return r
		}
	}
	for _, n := range []string{"command", "cmd", "prompt", "query", "content", "path", "file_path", "url"} {
		if schemaTextField(s.Properties, n) {
			return n
		}
	}
	// Last resort: the first string property in sorted order. Ranging the map
	// directly made the same tool map bare text into a different field on
	// different turns, silently changing what got executed.
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if s.Properties[n].Type == "string" {
			return n
		}
	}
	return ""
}

// schemaTextField reports whether the named property exists and is a string
// (or untyped, treated as a string) field the operator can fill with bare text.
func schemaTextField(props map[string]propType, name string) bool {
	p, ok := props[name]
	return ok && (p.Type == "string" || p.Type == "")
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

// toolListStart picks the first row of the visible tool window so the selected
// tool is always drawn. Without it, j past the bottom of the pane moved the
// highlight into rows that were never rendered and the operator lost track of
// what the composer was about to call.
func (m model) toolListStart(visible int) int {
	if visible < 1 || m.toolIdx < visible {
		return 0
	}
	first := m.toolIdx - visible + 1
	if maxFirst := len(m.tools) - visible; first > maxFirst {
		first = maxFirst
	}
	if first < 0 {
		first = 0
	}
	return first
}
