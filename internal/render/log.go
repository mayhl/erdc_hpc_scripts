package render

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/text"
)

// LevelOK is the house "success" tier — between Info and Warn in slog's scheme.
const LevelOK = slog.Level(2)

// eventLogPath is the curated event log (`mu`'s Step/Event/`mu log write`), read by
// `mu log`. It lives under XDG_STATE_HOME (logs/history that persist), not ~/.cache
// (disposable). MU_LOG_FILE overrides (config.toml [log].file wires it; tests set it).
func eventLogPath() string {
	if p := os.Getenv("MU_LOG_FILE"); p != "" {
		return p
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, _ := os.UserHomeDir()
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "mayhl_utils", "events.log")
}

// legacyEventLogPath is the pre-2026-07 location under ~/.cache, migrated once so old
// history follows the move. "" when MU_LOG_FILE is set (explicit path → no migration)
// or HOME is unknown.
func legacyEventLogPath() string {
	if os.Getenv("MU_LOG_FILE") != "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "mayhl_utils", "events.log")
}

var migrateOnce sync.Once

// EnsureEventLog migrates a pre-2026-07 ~/.cache event log into the state dir once, so
// existing history follows the location move. Best-effort and idempotent; a no-op when
// MU_LOG_FILE is set or the new file already exists. Logging must never break the tool.
func EnsureEventLog() {
	migrateOnce.Do(func() {
		newPath := eventLogPath()
		if _, err := os.Stat(newPath); err == nil {
			return // new log already present
		}
		old := legacyEventLogPath()
		if old == "" {
			return
		}
		if _, err := os.Stat(old); err != nil {
			return // nothing to migrate
		}
		if os.MkdirAll(filepath.Dir(newPath), 0o755) == nil {
			_ = os.Rename(old, newPath)
		}
	})
}

// houseHandler is the single log sink: renders house-style tiers to stderr and
// appends every record to framework.log as `[ts] [level] [scope] msg`. Future
// format/field/sink changes live here, never at call sites. Logging failures are
// swallowed — they must never break the tool.
type houseHandler struct {
	scope string
	mu    *sync.Mutex
	file  *os.File // nil if framework.log can't be opened
}

func newHouseHandler(scope string) *houseHandler {
	h := &houseHandler{scope: scope, mu: &sync.Mutex{}}
	EnsureEventLog() // migrate a legacy ~/.cache log into place before we open
	path := eventLogPath()
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			h.file = f
		}
	}
	return h
}

func (h *houseHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *houseHandler) Handle(_ context.Context, r slog.Record) error {
	scope := h.scope
	quiet := false // log-only: skip the terminal render (command owns its own UX)
	payload := ""
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "scope":
			scope = a.Value.String()
		case "quiet":
			quiet = a.Value.Bool()
		case "payload":
			payload = a.Value.String()
		}
		return true
	})
	if !quiet {
		renderTier(r.Level, r.Message)
	}
	h.append(r.Time, r.Level, scope, r.Message, payload)
	return nil
}

// WithAttrs binds a scope (the only attr we serialize today); the new handler
// shares the file + mutex (pointers) so all scopes append to one log.
func (h *houseHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	for _, a := range attrs {
		if a.Key == "scope" {
			nh.scope = a.Value.String()
		}
	}
	return &nh
}

func (h *houseHandler) WithGroup(string) slog.Handler { return h }

// append writes one event line: `[ts] [level] [scope] msg`, plus an optional
// tab-delimited JSON payload suffix (`…msg\t{json}`) when present. Old readers see
// the human message; structured readers split on the tab. Messages are single-line
// and tab-free, so the first tab cleanly delimits the payload.
func (h *houseHandler) append(t time.Time, level slog.Level, scope, msg, payload string) {
	if h.file == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	line := fmt.Sprintf("[%s] [%-5s] [%s] %s",
		t.Format("2006-01-02T15:04:05"), levelName(level), scope, msg)
	if payload != "" {
		line += "\t" + payload
	}
	_, _ = fmt.Fprintln(h.file, line)
}

func levelName(l slog.Level) string {
	switch l {
	case slog.LevelInfo:
		return "INFO"
	case LevelOK:
		return "OK"
	case slog.LevelWarn:
		return "WARN"
	case slog.LevelError:
		return "ERROR"
	default:
		return l.String()
	}
}

// renderTier prints one house-style status line to stderr (glyph + color),
// preserving the pre-slog mu_log appearance.
func renderTier(level slog.Level, msg string) {
	switch level {
	case LevelOK:
		logLine("✓", "[OK]", text.Colors{text.FgGreen, text.Bold}, msg)
	case slog.LevelWarn:
		logLine("!", "[WARN]", text.Colors{text.FgYellow, text.Bold}, msg)
	case slog.LevelError:
		logLine("✗", "[ERROR]", text.Colors{text.FgRed, text.Bold}, msg)
	default: // Info and anything unmapped
		logLine("→", "[INFO]", text.Colors{text.FgCyan}, msg)
	}
}

var (
	rootLogger *slog.Logger
	loggerOnce sync.Once
)

func base() *slog.Logger {
	loggerOnce.Do(func() { rootLogger = slog.New(newHouseHandler("mu")) })
	return rootLogger
}

// ResetLoggerForTest re-inits the sink so a test can repoint MU_LOG_FILE.
func ResetLoggerForTest() {
	loggerOnce = sync.Once{}
	rootLogger = nil
}

// Info, OK, Warn, Err print a house-style tier line to stderr — terminal only,
// NOT recorded. They're ephemeral UI messages; the curated event log is fed only
// by the scoped/event paths (Scoped, Step, Event*, `mu log write`). Rule of thumb:
// scoped ⇒ logged event; unscoped ⇒ ephemeral message.
func Info(msg string) {
	if IsQuiet() {
		return // -q keeps only results (OK) + warnings + errors
	}
	renderTier(slog.LevelInfo, msg)
}
func OK(msg string)   { renderTier(LevelOK, msg) }
func Warn(msg string) { renderTier(slog.LevelWarn, msg) }
func Err(msg string)  { renderTier(slog.LevelError, capErr(msg)) }

// errTailCap bounds the dimmed continuation block under an error line. Long remote
// stderr (compiler spew, a qsub traceback) keeps its LAST lines — the end is where
// shells and compilers put the verdict — with the full text dumped to a file.
const errTailCap = 25

// capErr enforces errTailCap: an over-long tail is cut to its last errTailCap lines
// behind an "… N line(s) omitted" marker, and the full message lands in an
// err-<id>.log beside the event log (best-effort, like CrashDump — an unwritable
// dump just drops the pointer line).
func capErr(msg string) string {
	lines := strings.Split(msg, "\n")
	if len(lines) <= errTailCap+1 { // head + a within-cap tail: untouched
		return msg
	}
	out := make([]string, 0, errTailCap+3)
	out = append(out, lines[0], fmt.Sprintf("… %d line(s) omitted", len(lines)-1-errTailCap))
	out = append(out, lines[len(lines)-errTailCap:]...)
	if path := errDump(msg); path != "" {
		out = append(out, "full output: "+path)
	}
	return strings.Join(out, "\n")
}

// errDump writes the uncut error text to err-<id>.log beside the event log. No
// breadcrumb event on purpose: Err is an EPHEMERAL tier (unscoped ⇒ not recorded)
// and the dump is just its overflow, not a curated event.
func errDump(msg string) string {
	dir := filepath.Dir(eventLogPath())
	path := filepath.Join(dir, "err-"+newEventID()+".log")
	if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(path, []byte(msg+"\n"), 0o644) != nil {
		return ""
	}
	return path
}

// Logger is a scope-bound emitter — its records are recorded in the event log.
type Logger struct{ l *slog.Logger }

// Scoped returns a Logger whose records carry the given subsystem scope.
func Scoped(scope string) *Logger {
	return &Logger{base().With(slog.String("scope", scope))}
}

func (x *Logger) Info(msg string) { x.l.Log(context.Background(), slog.LevelInfo, msg) }
func (x *Logger) OK(msg string)   { x.l.Log(context.Background(), LevelOK, msg) }
func (x *Logger) Warn(msg string) { x.l.Log(context.Background(), slog.LevelWarn, msg) }
func (x *Logger) Err(msg string)  { x.l.Log(context.Background(), slog.LevelError, msg) }

// EventLogPath is the curated event-log file that `mu log` reads.
func EventLogPath() string { return eventLogPath() }

// CrashDump persists a panic backtrace to a crash-<id>.log file beside the event log
// and drops a breadcrumb event pointing at it, returning the crash id and dump path.
// Best-effort: if the file can't be written it returns an empty path (the event still
// lands under that id) so the caller can fall back to an inline backtrace. reason is a
// one-line summary; stack is the full formatted trace. Reporting a crash must never
// itself break — write failures are swallowed.
func CrashDump(reason, stack string) (id, path string) {
	id = newEventID()
	dir := filepath.Dir(eventLogPath())
	path = filepath.Join(dir, "crash-"+id+".log")
	body := "mu crash " + id + "\n" + reason + "\n\n" + stack + "\n"
	if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(path, []byte(body), 0o644) != nil {
		path = "" // couldn't write the file; the event breadcrumb still records the crash
	}
	Emit("crash", "error", reason, map[string]any{"id": id, "file": path})
	return id, path
}

// emit is the shared structured-event core: it logs the record (and renders unless
// quiet) with an optional payload, returning the event id ("" when no payload). The id
// is written into the payload under "id" unless the caller already set one (letting a
// caller pass a shared correlation id).
func emit(level slog.Level, scope, msg string, quiet bool, payload map[string]any) string {
	attrs := []slog.Attr{slog.String("scope", scope), slog.Bool("quiet", quiet)}
	id := ""
	if len(payload) > 0 {
		if s, ok := payload["id"].(string); ok && s != "" {
			id = s // caller-supplied correlation id
		} else {
			id = newEventID()
		}
		rec := make(map[string]any, len(payload)+1)
		for k, v := range payload {
			rec[k] = v
		}
		rec["id"] = id
		if b, err := json.Marshal(rec); err == nil {
			attrs = append(attrs, slog.String("payload", capPayload(b, id)))
		}
	}
	base().LogAttrs(context.Background(), level, msg, attrs...)
	return id
}

// payloadCap bounds an inline event payload. The curated log stays single-digit MB
// over years only if no single record can balloon it — an over-cap payload spills
// to payload-<id>.json beside the log and the record keeps a pointer stub (id,
// spill path, byte count). Mirrors the Err tail-cap pattern (err-<id>.log).
const payloadCap = 8 * 1024

// capPayload returns the inline payload string for a marshaled record: verbatim
// within the cap, else the stub after spilling the full JSON. A failed spill keeps
// the payload inline — logging must never lose data it was handed.
func capPayload(b []byte, id string) string {
	if len(b) <= payloadCap {
		return string(b)
	}
	dir := filepath.Dir(eventLogPath())
	path := filepath.Join(dir, "payload-"+id+".json")
	if os.MkdirAll(dir, 0o755) != nil || os.WriteFile(path, append(b, '\n'), 0o644) != nil {
		return string(b)
	}
	stub, err := json.Marshal(map[string]any{"id": id, "spill": path, "bytes": len(b)})
	if err != nil {
		return string(b)
	}
	return string(stub)
}

// event appends a log-only record (quiet — no terminal render) for commands that
// print their own richer UX (progress bars, summaries) but still want a durable entry.
func event(level slog.Level, scope, msg string) { emit(level, scope, msg, true, nil) }

// Emit appends a log-only STRUCTURED event: a message plus an optional JSON payload
// (nil = none), returning the event id ("" when no payload). The id lands in the
// payload under "id" (a caller-set "id" is kept as a correlation key). level is a
// name: info | ok | warn | error.
func Emit(scope, level, msg string, payload map[string]any) string {
	return emit(levelFromString(level), scope, msg, true, payload)
}

// levelFromString maps a level name to its slog level (default Info).
func levelFromString(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "OK":
		return LevelOK
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERR", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newEventID returns a short random hex id for a structured event (its correlation key).
func newEventID() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// EventInfo/OK/Warn/Err append a log-only event under the given scope.
func EventInfo(scope, msg string) { event(slog.LevelInfo, scope, msg) }
func EventOK(scope, msg string)   { event(LevelOK, scope, msg) }
func EventWarn(scope, msg string) { event(slog.LevelWarn, scope, msg) }
func EventErr(scope, msg string)  { event(slog.LevelError, scope, msg) }

// LogRow is one event-log entry for LogBlock — a domain-free mirror of the reader's
// parsed line. Time is the parsed timestamp (zero → RawTS shown, and the row is
// grouped under no date header).
type LogRow struct {
	Time    time.Time
	RawTS   string
	Level   string
	Scope   string
	Msg     string
	Payload string // raw JSON payload suffix; "" when the event carries none
}

// levelGlyphColor maps a level name to its single-column house glyph (ASCII-safe)
// and tier color — the glyph carries the level, per the color policy.
func levelGlyphColor(level string) (string, text.Colors) {
	switch strings.ToUpper(level) {
	case "OK":
		return glyph("✓", "+"), text.Colors{text.FgGreen, text.Bold}
	case "WARN", "WARNING":
		return glyph("!", "!"), text.Colors{text.FgYellow, text.Bold}
	case "ERROR", "ERR":
		return glyph("✗", "x"), text.Colors{text.FgRed, text.Bold}
	default:
		return glyph("→", ">"), text.Colors{text.FgCyan}
	}
}

// LogBlock renders event-log rows as an aligned, date-grouped block for `mu log`: a
// bold-cyan title, a dim day line whenever the date changes, then one line per event —
// "<time> <glyph> <scope> <msg>" with the time blue, scope magenta, and the tier glyph
// carrying the level. Time is time-only; the day header holds the date. An event with a
// structured payload gets a dim ⊕ marker in the pretty view; in plain mode the raw JSON
// is appended tab-delimited so it passes through pipes intact.
func LogBlock(title string, rows []LogRow) string {
	scopeW := 5
	for _, r := range rows {
		if n := len(r.Scope); n > scopeW {
			scopeW = n
		}
	}
	if scopeW > 10 {
		scopeW = 10
	}
	dim := text.Colors{text.FgHiBlack}
	off := plainMode() || colorOff() // stdout view: plain when piped/--plain, or NO_COLOR

	var b strings.Builder
	if title != "" {
		if off {
			b.WriteString(title + "\n")
		} else {
			b.WriteString(append(tc(HueID), text.Bold).Sprint(title) + "\n") // bold cyan, like table titles
		}
	}
	curDay := ""
	for _, r := range rows {
		day, tm := logTimeParts(r)
		if day != "" && day != curDay {
			curDay = day
			if off {
				b.WriteString(" " + day + "\n")
			} else {
				b.WriteString(" " + dim.Sprint(day) + "\n") // dim — section header is chrome
			}
		}
		g, col := levelGlyphColor(r.Level)
		tmCell := fmt.Sprintf("%-8s", tm)
		scopeCell := fmt.Sprintf("%-*s", scopeW, trunc(r.Scope, scopeW))
		msg := r.Msg
		switch {
		case r.Payload == "":
		case off:
			msg += "\t" + r.Payload // plain: pass the raw payload through, tab-delimited and pipe-friendly
		default:
			msg += "  " + dim.Sprint(glyph("⊕", "+")) // pretty: a dim marker; the payload shows in -i / --json
		}
		if !off {
			tmCell = tc(HueLoc).Sprint(tmCell)        // blue — time column
			g = col.Sprint(g)                         // tier color on the glyph
			scopeCell = tc(HueUser).Sprint(scopeCell) // magenta — scope/tag column
		}
		b.WriteString(fmt.Sprintf("  %s  %s  %s  %s\n", tmCell, g, scopeCell, msg))
	}
	return strings.TrimRight(b.String(), "\n")
}

// logTimeParts splits a row into its date-header string and time-of-day cell. A row
// whose timestamp didn't parse has no day (ungrouped) and shows its raw stamp.
func logTimeParts(r LogRow) (day, tm string) {
	if r.Time.IsZero() {
		return "", r.RawTS
	}
	return r.Time.Format("2 Jan 2006"), r.Time.Format("15:04:05")
}

// WriteEvent renders + logs an event chosen by a level string (for `mu log write`),
// with an optional payload; returns the event id ("" when no payload).
func WriteEvent(scope, level, msg string, payload map[string]any) string {
	return emit(levelFromString(level), scope, msg, false, payload)
}
