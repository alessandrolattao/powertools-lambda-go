package appsyncevents

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/lambda/messages"
)

// FuzzResolve feeds arbitrary payloads to a router: whatever AppSync (or
// anyone allowed to invoke the function) sends, Resolve answers valid JSON
// or fails with one of its errors, and never panics.
func FuzzResolve(f *testing.F) {
	f.Add([]byte(`{"info":{"operation":"PUBLISH","channel":{"path":"/default/a"}},"events":[{"id":"1","payload":{"text":"x"}}]}`))
	f.Add([]byte(`{"info":{"operation":"PUBLISH","channel":{"path":"/agg/a"}},"events":[{"id":"1","payload":{"text":1}},{"id":"2"}]}`))
	f.Add([]byte(`{"info":{"operation":"PUBLISH","channel":{"path":"/none"}},"events":"None"}`))
	f.Add([]byte(`{"info":{"operation":"SUBSCRIBE","channel":{"path":"/deny/x"}},"identity":"None","prev":"None"}`))
	f.Add([]byte(`{"identity":{"sub":"s","claims":{"a":[1,{"b":null}]}},"info":{"operation":"SUBSCRIBE"}}`))
	f.Add([]byte(`[]`))

	type message struct {
		Text string `json:"text"`
	}
	app := New(WithLogger(slog.New(slog.DiscardHandler)))
	OnPublish(app, DefaultPath, func(ctx context.Context, msg message) (any, error) {
		ev, _ := CurrentEvent(ctx)
		if msg.Text == "deny" {
			return nil, &UnauthorizedError{}
		}
		return []any{msg.Text, len(ev.Events), ev.Identity}, nil
	}, WithConcurrency(2))
	OnPublishAggregate(app, "/agg/*", func(_ context.Context, events []Event[message]) ([]Result[string], error) {
		results := make([]Result[string], len(events))
		for i, ev := range events {
			results[i] = Result[string]{ID: ev.ID, Payload: ev.Payload.Text}
		}
		return results, nil
	})
	app.OnSubscribe("/deny/*", func(context.Context) error { return &UnauthorizedError{} })
	app.OnSubscribe("/default/*", func(context.Context) error { return errors.New("no") })

	f.Fuzz(func(t *testing.T, payload []byte) {
		out, err := app.Resolve(t.Context(), payload)
		if err != nil {
			if invalid, ok := errors.AsType[*InvalidEventError](err); ok && invalid != nil {
				return
			}
			if invokeErr, ok := errors.AsType[messages.InvokeResponse_Error](err); ok && invokeErr.Type == unauthorizedType {
				return
			}
			t.Fatalf("payload %q: unexpected error %T: %v", payload, err, err)
		}
		if !out.IsValid() {
			t.Fatalf("payload %q: invalid JSON response %q", payload, out)
		}
	})
}

// validPath is Powertools Python's PATH_REGEX, the reference parsePattern
// must agree with.
var validPath = regexp.MustCompile(`^/([^/*]+)(/[^/*]+)*(/\*)?$`)

// FuzzPattern checks the channel path patterns against references written
// as regular expressions: validation against Powertools Python's, matching
// against Python's with the segment boundary fixed, and precedence
// against its definition.
func FuzzPattern(f *testing.F) {
	for _, seed := range [][2]string{
		{"/*", "/a/b"},
		{"/default/*", "/default/a"},
		{"/default/*", "/defaults/a"},
		{"/default/a", "/default/a"},
		{"/a/b/*", "/a/b"},
		{"/a/*/c", "/a/b/c"},
		{"/", "/"},
		{"//", "/a"},
		{"/a\n", "/a\n"},
		{"/\xff/*", "/\xff/x"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, path, channel string) {
		p, err := parsePattern(path)
		if valid := path == "/*" || validPath.MatchString(path); valid != (err == nil) {
			// regexp reads invalid UTF-8 as U+FFFD; the parser works on bytes.
			if utf8.ValidString(path) {
				t.Fatalf("pattern %q: valid %v, error %v", path, valid, err)
			}
			return
		}
		if err == nil {
			checkMatch(t, p, channel)
		}
	})
}

func checkMatch(t *testing.T, p pattern, channel string) {
	t.Helper()
	want := channel == p.path
	if p.wildcard {
		want = p.prefix == "" || channel == p.prefix || strings.HasPrefix(channel, p.prefix+"/")
		if p.prefix != "" && utf8.ValidString(p.prefix) && utf8.ValidString(channel) {
			reference := regexp.MustCompile(`^` + regexp.QuoteMeta(p.prefix) + `(?s:/.*)?$`)
			if reference.MatchString(channel) != want {
				t.Fatalf("reference disagrees for %q on %q", p.path, channel)
			}
		}
	}
	if got := p.matches(channel); got != want {
		t.Fatalf("pattern %q, channel %q: matches %v, want %v", p.path, channel, got, want)
	}

	// An exact pattern for the channel wins over any wildcard that matches
	// it.
	if exact, err := parsePattern(channel); err == nil && !exact.wildcard && p.wildcard && p.matches(channel) {
		if !exact.moreSpecific(p) || p.moreSpecific(exact) {
			t.Fatalf("exact %q does not win over %q", channel, p.path)
		}
	}
}
