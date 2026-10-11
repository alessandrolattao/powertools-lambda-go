package logging

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
)

// StackTracer is implemented by errors that remember where they were
// created, the convention of go-errors and similar packages: Callers returns
// program counters as runtime.Callers fills them. When the error logged
// under ErrorKey, or one it wraps, has such a method, the record carries the
// stack in its stack_trace key.
type StackTracer interface {
	Callers() []uintptr
}

// stackTrace is the stack_trace key, in Powertools Python's shape. Go has
// no source text to give a frame's statement, so frames have none.
type stackTrace struct {
	Type   string       `json:"type"`
	Value  string       `json:"value"`
	Module string       `json:"module,omitempty"`
	Frames []stackFrame `json:"frames"`
}

type stackFrame struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// exception is what a record tells of an error.
type exception struct {
	message string
	name    string
	module  string
	callers []uintptr
}

// describe reads an error the way the exception keys show it: its message,
// the name of its type and, when it carries one, its stack.
func describe(err error) exception {
	ex := exception{message: errorText(err), name: "error"}
	if p, ok := errors.AsType[*panicError](err); ok {
		ex.callers = p.callers
		if cause, ok := p.value.(error); ok {
			ex.name, ex.module = errorType(cause, "panic")
		} else {
			ex.name = "panic"
		}
		return ex
	}
	ex.name, ex.module = errorType(err, "error")
	var st StackTracer
	if errors.As(err, &st) {
		ex.callers = st.Callers()
	}
	return ex
}

// errorType names an error after the type of the first error in its chain
// that an application or a library defined (a ValidationError, a
// PathError), skipping the standard library's own wrappers and sentinels
// (wrapError, errorString, joinError), which say nothing about the failure.
func errorType(err error, fallback string) (name, module string) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		t := reflect.TypeOf(e)
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Name() != "" && t.PkgPath() != "errors" && t.PkgPath() != "fmt" {
			return t.Name(), t.PkgPath()
		}
	}
	return fallback, ""
}

// errorText is the error's message. fmt recovers from an Error method that
// panics, a nil pointer receiver for one.
func errorText(err error) string {
	return fmt.Sprint(err)
}

// trace builds the stack_trace key.
func (ex *exception) trace() *stackTrace {
	if len(ex.callers) == 0 {
		return nil
	}
	st := &stackTrace{Type: ex.name, Value: ex.message, Module: ex.module}
	frames := runtime.CallersFrames(ex.callers)
	for {
		f, more := frames.Next()
		if f.Function != "" {
			st.Frames = append(st.Frames, stackFrame{File: f.File, Line: f.Line, Function: f.Function})
		}
		if !more {
			return st
		}
	}
}

// panicError carries a recovered panic and the stack it unwound.
type panicError struct {
	value   any
	callers []uintptr
}

func (p *panicError) Error() string {
	return fmt.Sprint(p.value)
}

// Unwrap returns the panic value when it is an error.
func (p *panicError) Unwrap() error {
	err, _ := p.value.(error) //nolint:errcheck // a panic value that is not an error wraps nothing
	return err
}

// panicked captures the stack of the panic being recovered. Called from the
// deferred function that recovered, it skips the frames of the recovery and
// of the runtime's panic machinery, so that the stack starts where the
// panic happened.
func panicked(value any) *panicError {
	pcs := make([]uintptr, 64)
	pcs = pcs[:runtime.Callers(2, pcs)]
	start, inPanic := 0, false
	for i, pc := range pcs {
		name := funcName(pc)
		switch {
		case name == "runtime.gopanic":
			inPanic = true
			start = i + 1
		case inPanic && strings.HasPrefix(name, "runtime."):
			start = i + 1
		case inPanic:
			return &panicError{value: value, callers: pcs[start:]}
		}
	}
	return &panicError{value: value, callers: pcs[start:]}
}

// funcName names the function a program counter returned by runtime.Callers
// belongs to.
func funcName(pc uintptr) string {
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	return frame.Function
}
