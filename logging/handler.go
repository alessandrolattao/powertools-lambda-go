package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-lambda-go/lambdacontext"

	"github.com/alessandrolattao/powertools-lambda-go/internal/coldstart"
	"github.com/alessandrolattao/powertools-lambda-go/internal/env"
	"github.com/alessandrolattao/powertools-lambda-go/internal/output"
)

// Handler is a slog.Handler that writes Powertools JSON records: one JSON
// object per line, with the Powertools keys, the Lambda context of the
// invocation the record belongs to, and the keys, sampling decision and
// buffer of that invocation. Create it with NewHandler; use it with
// slog.New, slog.SetDefault, or any library that takes a slog.Handler.
//
// A Handler is safe for concurrent use. The handlers WithAttrs and
// WithGroup return share its configuration, level and buffers.
type Handler struct {
	core  *core
	chain []withScope
}

// withScope is one step of the attributes and groups added by WithAttrs and
// WithGroup: a group opened (except for the first step, the top level), then
// the attributes added inside it.
type withScope struct {
	group string
	attrs []slog.Attr
}

// core is what a Handler and the handlers derived from it share.
type core struct {
	service      string
	samplingRate float64
	sampling     bool
	random       func() float64
	level        slog.LevelVar
	sampled      atomic.Bool
	buffer       *bufferConfig
	alcBelowBuf  bool

	logPanics   bool
	stackTrace  bool
	timeLayout  string
	utc         bool
	levelName   func(slog.Level) string
	order       []string
	names       map[string]string
	dropped     map[string]bool
	replaceAttr func([]string, slog.Attr) slog.Attr
	protected   []string
	clock       func() time.Time
	coldStart   func(context.Context) bool
	function    functionInfo

	encoder *encoder
	out     io.Writer
	warns   warnings
}

// NewHandler returns a Handler configured by opts, then by the POWERTOOLS_*
// environment variables, then by Powertools' defaults.
func NewHandler(opts ...Option) *Handler {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	c := &core{
		service:     env.ServiceNameOr(env.DefaultServiceName),
		random:      cfg.random,
		logPanics:   cfg.logPanics,
		stackTrace:  cfg.stackTrace,
		timeLayout:  cfg.timeLayout,
		utc:         cfg.utc,
		levelName:   cfg.levelName,
		order:       slices.Clone(cfg.order),
		names:       cfg.names,
		dropped:     cfg.dropped,
		replaceAttr: cfg.replaceAttr,
		clock:       cfg.clock,
		coldStart:   cfg.coldStart,
		function:    functionInfo{name: lambdacontext.FunctionName, memory: lambdacontext.MemoryLimitInMB},
		out:         output.For(cfg.writer),
		warns:       warnings{out: output.For(cfg.warnings)},
	}
	if cfg.service != nil {
		c.service = *cfg.service
	}
	if cfg.function != nil {
		c.function = *cfg.function
	}
	if c.random == nil {
		c.random = rand.Float64
	}
	if c.coldStart == nil {
		c.coldStart = coldstart.Is
	}
	if c.levelName == nil {
		c.levelName = LambdaLevelName
	}
	c.protected = []string{c.name(KeyLevel), c.name(KeyLocation), c.name(KeyMessage), c.name(KeyTimestamp)}
	c.configureSampling(cfg.samplingRate)
	c.configureBuffer(cfg.buffer)
	c.setLevel(context.Background(), cfg.level)
	c.encoder = newEncoder(cfg.indent, cfg.jsonOptions)
	return &Handler{core: c, chain: []withScope{{}}}
}

func (c *core) configureSampling(explicit *float64) {
	switch {
	case explicit != nil:
		c.samplingRate, c.sampling = *explicit, true
	case env.Get(env.LoggerSampleRate) != "":
		rate := env.Float(env.LoggerSampleRate, math.NaN())
		c.samplingRate, c.sampling = rate, !math.IsNaN(rate)
	}
	c.sampled.Store(c.roll())
}

func (c *core) configureBuffer(cfg *bufferConfig) {
	if cfg == nil {
		return
	}
	buffer := *cfg
	if buffer.maxBytes <= 0 {
		c.warns.warn(context.Background(), fmt.Sprintf("Ignoring a buffer size of %d bytes: it must be positive.", buffer.maxBytes))
		buffer.maxBytes = defaultBufferConfig().maxBytes
	}
	if buffer.at >= slog.LevelError {
		c.warns.warn(context.Background(), fmt.Sprintf("Ignoring buffering at %s: records at ERROR and above are never buffered.",
			LambdaLevelName(buffer.at)))
		buffer.at = defaultBufferConfig().at
	}
	c.buffer = &buffer
}

// setLevel applies the level precedence of Powertools Python to explicit, an
// explicit level or nil.
func (c *core) setLevel(ctx context.Context, explicit *slog.Level) {
	level, warnings := resolveLevel(explicit)
	for _, w := range warnings {
		c.warns.warn(ctx, w)
	}
	c.level.Set(level)
	if alc, ok := alcLevel(); ok && c.buffer != nil && alc > c.buffer.at {
		c.alcBelowBuf = true
		c.warns.warn(ctx, "Advanced Logging Controls (ALC) Log Level is less verbose than Log Buffering Log Level. "+
			"Buffered logs will be filtered by ALC")
	}
}

// roll draws a sampling decision: true for the given fraction of draws.
func (c *core) roll() bool {
	return c.sampling && c.samplingRate > 0 && c.random() <= c.samplingRate
}

// Enabled reports whether a record at level would be written, or buffered,
// in the invocation ctx belongs to.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	c := h.core
	if level >= c.level.Level() {
		return true
	}
	if c.buffer == nil && !c.sampling {
		return false
	}
	if level >= slog.LevelError && c.buffer != nil && c.buffer.flushOnError {
		return true // Handle flushes the buffer, even for a record it then drops
	}
	e := c.entry(ctx)
	return level >= c.minLevel(e) || c.bufferable(e, level)
}

// minLevel is the level of the records written directly: the logger's, or
// DEBUG for an invocation drawn by sampling.
func (c *core) minLevel(e *entry) slog.Level {
	level := c.level.Level()
	if !c.sampling {
		return level
	}
	sampled := c.sampled.Load()
	if e != nil {
		sampled = e.sampled.Load()
	}
	if sampled {
		return min(level, slog.LevelDebug)
	}
	return level
}

// bufferable reports whether a record at level goes to the invocation's
// buffer rather than out.
func (c *core) bufferable(e *entry, level slog.Level) bool {
	return e != nil && e.buffer != nil && !e.closed.Load() && level <= c.buffer.at
}

// entry returns the state the handler keeps for the invocation ctx belongs
// to, creating it the first time the handler sees the invocation. It is nil
// outside an invocation wrapped by InjectContext.
func (c *core) entry(ctx context.Context) *entry {
	inv := invocationFrom(ctx)
	if inv == nil {
		return nil
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return c.entryLocked(inv)
}

func (c *core) entryLocked(inv *invocation) *entry {
	if e, ok := inv.loggers[c]; ok {
		return e
	}
	e := &entry{}
	e.sampled.Store(c.roll())
	if c.buffer != nil {
		e.buffer = newBuffer(c.buffer.maxBytes)
	}
	if inv.loggers == nil {
		inv.loggers = map[*core]*entry{}
	}
	inv.loggers[c] = e
	return e
}

// Handle writes r, or keeps it in the invocation's buffer.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	return h.handle(ctx, r, nil)
}

// handle is Handle with, for the event InjectContext logs, a message that is
// a JSON value rather than a string.
func (h *Handler) handle(ctx context.Context, r slog.Record, message any) error {
	c := h.core
	e := c.entry(ctx)
	// A failed flush must not cost the record that triggered it: the error
	// record is the one that matters most.
	var flushErr error
	if r.Level >= slog.LevelError && c.buffer != nil && c.buffer.flushOnError {
		flushErr = c.flush(ctx, e)
	}
	buffered := c.bufferable(e, r.Level)
	if !buffered && r.Level < c.minLevel(e) {
		return flushErr
	}
	line, err := h.encode(ctx, r, message)
	if err != nil {
		return errors.Join(flushErr, err)
	}
	if buffered && c.keep(ctx, e, line) {
		return flushErr
	}
	return errors.Join(flushErr, c.write(line))
}

// keep buffers line for the invocation. A line larger than the whole buffer
// is not kept, with a warning, and goes out at once.
func (c *core) keep(ctx context.Context, e *entry, line []byte) bool {
	inv := invocationFrom(ctx)
	inv.mu.Lock()
	kept := !e.closed.Load() && e.buffer.add(line)
	closed := e.closed.Load()
	inv.mu.Unlock()
	if !kept && !closed {
		c.warns.warn(ctx, fmt.Sprintf("Cannot add item to the buffer. Item size exceeds total cache size %d bytes",
			c.buffer.maxBytes))
	}
	return kept
}

func (c *core) write(line []byte) error {
	if _, err := c.out.Write(line); err != nil {
		return fmt.Errorf("logging: write a record: %w", err)
	}
	return nil
}

// flush writes the records buffered for the invocation, in the order they
// were logged, with their original time and location.
func (c *core) flush(ctx context.Context, e *entry) error {
	if e == nil || e.buffer == nil {
		return nil
	}
	inv := invocationFrom(ctx)
	inv.mu.Lock()
	lines, evicted := e.buffer.drain()
	inv.mu.Unlock()
	if len(lines) == 0 {
		return nil
	}
	if c.alcBelowBuf {
		c.warns.warn(ctx, "Advanced Logging Controls (ALC) Log Level is less verbose than Log Buffering Log Level. "+
			"Some logs might be missing")
	}
	for _, line := range lines {
		if err := c.write(line); err != nil {
			return err
		}
	}
	if evicted {
		c.warns.warn(ctx, "Some logs are not displayed because they were evicted from the buffer. "+
			"Increase buffer size to store more logs in the buffer")
	}
	return nil
}

// FlushBuffer writes the records buffered for the invocation ctx belongs to,
// and empties the buffer: Powertools Python's flush_buffer.
func (h *Handler) FlushBuffer(ctx context.Context) error {
	return h.core.flush(ctx, h.core.entry(ctx))
}

// ClearBuffer drops the records buffered for the invocation ctx belongs to:
// Powertools Python's clear_buffer.
func (h *Handler) ClearBuffer(ctx context.Context) {
	e := h.core.entry(ctx)
	if e == nil || e.buffer == nil {
		return
	}
	inv := invocationFrom(ctx)
	inv.mu.Lock()
	defer inv.mu.Unlock()
	e.buffer.reset()
}

// WithAttrs returns a Handler whose records carry attrs.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.withAttrs(attrs)
}

func (h *Handler) withAttrs(attrs []slog.Attr) *Handler {
	if len(attrs) == 0 {
		return h
	}
	chain := slices.Clone(h.chain)
	last := &chain[len(chain)-1]
	last.attrs = append(slices.Clip(last.attrs), attrs...)
	return &Handler{core: h.core, chain: chain}
}

// WithGroup returns a Handler that puts the attributes added from now on,
// and those of each record, in the group name.
func (h *Handler) WithGroup(name string) slog.Handler {
	return h.withGroup(name)
}

func (h *Handler) withGroup(name string) *Handler {
	if name == "" {
		return h
	}
	return &Handler{core: h.core, chain: append(slices.Clip(h.chain), withScope{group: name})}
}

// SetLevel changes the minimum level, with the precedence NewHandler applies:
// Lambda's Advanced Logging Controls still win, as with Powertools Python's
// setLevel.
func (h *Handler) SetLevel(level slog.Level) {
	h.core.setLevel(context.Background(), &level)
}

// Level returns the minimum level of the records written, sampling aside.
func (h *Handler) Level() slog.Level {
	return h.core.level.Level()
}

// Service returns the service name the records carry.
func (h *Handler) Service() string {
	return h.core.service
}

// RefreshSampleRateCalculation draws the sampling decision again: for the
// invocation ctx belongs to, or outside any invocation, for the records
// logged outside one. InjectContext draws a decision for every invocation;
// call this when you do not use it.
func (h *Handler) RefreshSampleRateCalculation(ctx context.Context) {
	c := h.core
	inv := invocationFrom(ctx)
	if inv == nil {
		c.sampled.Store(c.roll())
		return
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	c.entryLocked(inv).sampled.Store(c.roll())
}

// begin starts an invocation for the handler: the state in the context,
// shared with the other loggers that wrap the same invocation unless fresh
// asks for a new one, and the handler's own entry in it. owner tells the
// caller that the entry is its own, to end when the invocation ends.
func (c *core) begin(ctx context.Context, fresh bool) (_ context.Context, owner bool) {
	inv := invocationFrom(ctx)
	if inv == nil || fresh {
		inv = &invocation{}
		if _, ok := lambdacontext.FromContext(ctx); ok {
			inv.coldStart = c.coldStart(ctx)
		}
		ctx = context.WithValue(ctx, invocationKey{}, inv)
		if fresh {
			ctx = context.WithValue(ctx, scopeKey{}, (*scope)(nil))
		}
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if _, ok := inv.loggers[c]; ok {
		return ctx, false
	}
	c.entryLocked(inv)
	return ctx, true
}

// end closes the handler's entry: its buffer is dropped, and records logged
// later in the context, by goroutines that outlive the invocation, are
// written directly.
func (c *core) end(ctx context.Context) {
	inv := invocationFrom(ctx)
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if e, ok := inv.loggers[c]; ok {
		e.closed.Store(true)
		if e.buffer != nil {
			e.buffer.reset()
		}
	}
}

// warnings writes Powertools' warnings: JSON records at WARN on standard
// error, once per cause, as Python's warnings module shows them.
type warnings struct {
	out  io.Writer
	seen sync.Map
}

func (w *warnings) warn(ctx context.Context, message string) {
	if _, seen := w.seen.LoadOrStore(message, true); seen || w.out == nil {
		return
	}
	r := slog.NewRecord(time.Now(), slog.LevelWarn, "powertools: "+message, 0)
	_ = slog.NewJSONHandler(w.out, nil).Handle(ctx, r) //nolint:errcheck // a warning that cannot be written has nowhere else to go
}
