package eventhandler

import (
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

var closureName = regexp.MustCompile(`^func\d+$|^gowrap\d+$`)

// funcName returns the name of the function f for generated operationIds,
// "" for closures and values without one.
func funcName(f any) string {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func || v.IsNil() {
		return ""
	}
	fn := runtime.FuncForPC(v.Pointer())
	if fn == nil {
		return ""
	}
	name := strings.TrimSuffix(fn.Name(), "-fm")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	if closureName.MatchString(name) {
		return ""
	}
	return name
}

// handlerName returns the name of the function behind h, when h is an
// http.HandlerFunc.
func handlerName(h http.Handler) string {
	if f, ok := h.(http.HandlerFunc); ok {
		return funcName(f)
	}
	return ""
}

var (
	warned = sync.Map{}
	stderr = sync.OnceValue(func() *slog.Logger {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	})
)

// warnOnce writes a warning, Python's warnings.warn, once per key: write
// logs a JSON record at WARN on standard error.
func warnOnce(key string, write func(l *slog.Logger)) {
	if _, loaded := warned.LoadOrStore(key, true); loaded {
		return
	}
	write(stderr())
}
