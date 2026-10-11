package eventhandler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type todo struct {
	ID    int      `json:"id"`
	Title string   `json:"title" required:"true" minLength:"3"`
	Done  bool     `json:"done"`
	Tags  []string `json:"tags,omitempty" maxItems:"2"`
}

type paging struct {
	Limit  int  `query:"limit" default:"10" minimum:"1" maximum:"100"`
	Offset *int `query:"offset"`
}

type listTodos struct {
	paging

	ID       int        `path:"id" minimum:"1"`
	Fields   string     `query:"fields" enum:"all,title"`
	Tags     []string   `query:"tag"`
	Done     *bool      `query:"done"`
	Ratio    float64    `query:"ratio"`
	Count    uint8      `query:"count"`
	Since    time.Time  `query:"since"`
	Trace    string     `header:"X-Trace-Id" required:"true"`
	Accept   []string   `header:"X-Accept"`
	Session  string     `cookie:"session"`
	Ignored  string     // no tag: left alone
	internal string     //nolint:unused // unexported fields are skipped
	Names    []string   `query:"name" minItems:"1"`
	Pattern  string     `query:"code" pattern:"^[A-Z]{3}$"`
	Email    string     `query:"email" format:"email"`
	When     *time.Time `query:"when"`
}

func TestTypedOperationBinding(t *testing.T) {
	t.Parallel()
	app := New()
	var got listTodos
	Get(app, "/todos/{id}", func(ctx context.Context, in listTodos) (map[string]any, error) {
		got = in
		_, ok := RequestFromContext(ctx)
		return map[string]any{"ok": ok}, nil
	})
	rec := do(t, app, "GET",
		"/todos/7?limit=5&offset=2&fields=title&tag=a&tag=b&done=yes&ratio=0.5&count=3&since=2024-01-02T03:04:05Z&name=x&code=ABC&email=a@b.co",
		"", "X-Trace-Id", "abc", "X-Accept", "json, xml", "X-Accept", "csv", "Cookie", "session=s3cr3t; other=1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ok":true}`, rec.Body.String())
	assert.Equal(t, 7, got.ID)
	assert.Equal(t, 5, got.Limit)
	require.NotNil(t, got.Offset)
	assert.Equal(t, 2, *got.Offset)
	assert.Equal(t, "title", got.Fields)
	assert.Equal(t, []string{"a", "b"}, got.Tags)
	require.NotNil(t, got.Done)
	assert.True(t, *got.Done)
	assert.InDelta(t, 0.5, got.Ratio, 1e-9)
	assert.Equal(t, uint8(3), got.Count)
	assert.Equal(t, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), got.Since)
	assert.Equal(t, "abc", got.Trace)
	assert.Equal(t, []string{"json", "xml", "csv"}, got.Accept)
	assert.Equal(t, "s3cr3t", got.Session)
	assert.Nil(t, got.When)

	rec = do(t, app, "GET", "/todos/7?name=x", "", "X-Trace-Id", "abc")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 10, got.Limit, "default")
	assert.Nil(t, got.Offset)
}

func TestTypedOperationValidationErrors(t *testing.T) {
	t.Parallel()
	app := New()
	Get(app, "/todos/{id}", func(context.Context, listTodos) (string, error) { return "x", nil })

	tests := []struct {
		name   string
		target string
		header []string
		want   string
	}{
		{
			name:   "parsing and constraints, in Python's order",
			target: "/todos/0?limit=abc&fields=x&count=300&done=maybe&code=abc&email=nope&name=x",
			header: []string{"X-Trace-Id", "t"},
			want: `{"statusCode":422,"detail":[` +
				`{"loc":["path","id"],"type":"greater_than_equal"},` +
				`{"loc":["query","limit"],"type":"int_parsing"},` +
				`{"loc":["query","fields"],"type":"enum"},` +
				`{"loc":["query","done"],"type":"bool_parsing"},` +
				`{"loc":["query","count"],"type":"int_parsing"},` +
				`{"loc":["query","code"],"type":"string_pattern_mismatch"},` +
				`{"loc":["query","email"],"type":"value_error"}]}`,
		},
		{
			name:   "missing required header",
			target: "/todos/1?name=x",
			want:   `{"statusCode":422,"detail":[{"loc":["header","X-Trace-Id"],"type":"missing"}]}`,
		},
		{
			name:   "bounds",
			target: "/todos/1?limit=101&name=x&ratio=inf",
			header: []string{"X-Trace-Id", "t"},
			want: `{"statusCode":422,"detail":[{"loc":["query","limit"],"type":"less_than_equal"},` +
				`{"loc":["query","ratio"],"type":"finite_number"}]}`,
		},
		{
			name:   "list item",
			target: "/todos/1?when=yesterday&since=2024",
			header: []string{"X-Trace-Id", "t"},
			want: `{"statusCode":422,"detail":[{"loc":["query","since"],"type":"datetime_parsing"},` +
				`{"loc":["query","when"],"type":"datetime_parsing"}]}`,
		},
	}
	for _, tc := range tests {
		rec := do(t, app, "GET", tc.target, "", tc.header...)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, tc.name)
		pythonBody(t, tc.want, rec.Body.String(), tc.name)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	}
}

type createTodo struct {
	Body todo
}

type optionalBody struct {
	Body *todo
}

func TestTypedOperationJSONBody(t *testing.T) {
	t.Parallel()
	app := New()
	Post(app, "/todos", func(_ context.Context, in createTodo) (Response[todo], error) {
		in.Body.ID = 1
		return Response[todo]{
			StatusCode: http.StatusCreated,
			Header:     http.Header{"x-todo": {"1"}},
			Cookies:    []*http.Cookie{{Name: "last", Value: "1"}}, //nolint:gosec // G124: the attributes do not matter here
			Body:       in.Body,
		}, nil
	})
	Put(app, "/todos", func(_ context.Context, in optionalBody) (bool, error) { return in.Body == nil, nil })

	rec := do(t, app, "POST", "/todos", `{"title":"write tests","tags":["a"],"unknown":1}`, "Content-Type", "application/json; charset=utf-8")
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.JSONEq(t, `{"id":1,"title":"write tests","done":false,"tags":["a"]}`, rec.Body.String())
	assert.Equal(t, "1", rec.Header().Get("X-Todo"))
	assert.Equal(t, "last=1", rec.Header().Get("Set-Cookie"))

	tests := []struct{ name, body, contentType, want string }{
		{"missing body", "", "", `{"statusCode":422,"detail":[{"loc":["body"],"type":"missing"}]}`},
		{"invalid JSON", `{"title": "x"`, "", `{"statusCode":422,"detail":[{"loc":["body",13],"type":"json_invalid"}]}`},
		{
			"field errors", `{"title":"ab","done":"yes","id":"7","tags":["a","b","c"]}`, "application/json",
			`{"statusCode":422,"detail":[{"loc":["body","done"],"type":"bool_parsing"},` +
				`{"loc":["body","id"],"type":"int_parsing"},` +
				`{"loc":["body","tags"],"type":"too_long"},` +
				`{"loc":["body","title"],"type":"string_too_short"}]}`,
		},
		{"missing field", `{"done":true}`, "", `{"statusCode":422,"detail":[{"loc":["body","title"],"type":"missing"}]}`},
		{"wrong type", `[1]`, "", `{"statusCode":422,"detail":[{"loc":["body"],"type":"model_attributes_type"}]}`},
		{"nested item", `{"title":"abc","tags":["a",3]}`, "", `{"statusCode":422,"detail":[{"loc":["body","tags",1],"type":"string_type"}]}`},
	}
	for _, tc := range tests {
		var header []string
		if tc.contentType != "" {
			header = []string{"Content-Type", tc.contentType}
		}
		res := do(t, app, "POST", "/todos", tc.body, header...)
		assert.Equal(t, http.StatusUnprocessableEntity, res.Code, tc.name)
		pythonBody(t, tc.want, res.Body.String(), tc.name)
	}

	rec = do(t, app, "POST", "/todos", `title=x`, "Content-Type", "text/plain")
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	pythonBody(t, `{"statusCode":415,"detail":[{"loc":["body"],"type":"unsupported_content_type"}]}`, rec.Body.String())

	assert.Equal(t, "true", do(t, app, "PUT", "/todos", "").Body.String(), "a pointer body is optional")
}

type uploadForm struct {
	Name  string                `formData:"name" required:"true" minLength:"2"`
	Count int                   `formData:"count"`
	File  *multipart.FileHeader `formData:"file" required:"true"`
}

type urlForm struct {
	Name  string   `formData:"name" required:"true"`
	Items []string `formData:"item"`
}

func TestTypedOperationForms(t *testing.T) {
	t.Parallel()
	app := New()
	Post(app, "/upload", func(_ context.Context, in uploadForm) (string, error) {
		f, err := in.File.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return fmt.Sprintf("%s %d %s %s", in.Name, in.Count, in.File.Filename, data), err
	})
	Post(app, "/form", func(_ context.Context, in urlForm) (string, error) {
		return in.Name + strings.Join(in.Items, ","), nil
	})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("name", "report"))
	require.NoError(t, mw.WriteField("count", "2"))
	fw, err := mw.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("content"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	rec := do(t, app, "POST", "/upload", body.String(), "Content-Type", mw.FormDataContentType())
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"report 2 a.txt content"`, rec.Body.String())

	rec = do(t, app, "POST", "/upload", "x", "Content-Type", "multipart/form-data")
	pythonBody(t, `{"statusCode":422,"detail":[{"loc":["body"],"type":"multipart_invalid"}]}`, rec.Body.String())

	rec = do(t, app, "POST", "/form", "name=n&item=a&item=b", "Content-Type", "application/x-www-form-urlencoded")
	assert.Equal(t, `"na,b"`, rec.Body.String())
	rec = do(t, app, "POST", "/form", "item=a", "Content-Type", "application/x-www-form-urlencoded")
	pythonBody(t, `{"statusCode":422,"detail":[{"loc":["body","name"],"type":"missing"}]}`, rec.Body.String())
	rec = do(t, app, "POST", "/upload", "name=n", "Content-Type", "application/x-www-form-urlencoded")
	pythonBody(t, `{"statusCode":422,"detail":[{"loc":["body","name"],"type":"string_too_short"},{"loc":["body","file"],"type":"missing"}]}`, rec.Body.String())
	assert.Equal(t, http.StatusUnsupportedMediaType, do(t, app, "POST", "/form", "{}", "Content-Type", "application/json").Code)
}

type rawInput struct {
	Body []byte
}

func TestTypedOperationRawBodies(t *testing.T) {
	t.Parallel()
	app := New()
	Post(app, "/echo", func(_ context.Context, in rawInput) (Response[[]byte], error) {
		return Response[[]byte]{Header: http.Header{"Content-Type": {"image/png"}}, Body: in.Body}, nil
	})
	Get(app, "/bytes", func(context.Context, struct{}) ([]byte, error) { return []byte{0, 1, 2}, nil })

	rec := do(t, app, "POST", "/echo", "\x89PNG", "Content-Type", "image/png")
	assert.Equal(t, "\x89PNG", rec.Body.String())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "application/octet-stream", do(t, app, "GET", "/bytes", "").Header().Get("Content-Type"))
}

type badOutput struct {
	Name string `json:"name" minLength:"5"`
}

func TestResponseValidation(t *testing.T) {
	t.Parallel()
	short := func(context.Context, struct{}) (badOutput, error) { return badOutput{Name: "x"}, nil }

	app := New()
	Get(app, "/off", short)
	assert.Equal(t, http.StatusOK, do(t, app, "GET", "/off", "").Code, "off by default")

	app = New(WithResponseValidation(http.StatusInternalServerError))
	Get(app, "/app", short)
	Get(app, "/route", short, ResponseValidation(http.StatusBadGateway))
	rec := do(t, app, "GET", "/app", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	pythonBody(t, `{"statusCode":500,"detail":[{"loc":["response","name"],"type":"string_too_short"}]}`, rec.Body.String())
	assert.Equal(t, http.StatusBadGateway, do(t, app, "GET", "/route", "").Code, "the route's status wins")

	app = New()
	Get(app, "/default", short, ResponseValidation(http.StatusUnprocessableEntity))
	var caught *ResponseValidationError
	OnError(app, func(w http.ResponseWriter, r *http.Request, err *ResponseValidationError) error {
		caught = err
		return WriteJSON(w, r, http.StatusTeapot, nil)
	})
	assert.Equal(t, http.StatusTeapot, do(t, app, "GET", "/default", "").Code)
	require.NotNil(t, caught)
	assert.Equal(t, badOutput{Name: "x"}, caught.Body)
	assert.Contains(t, caught.Error(), "response.name: string_too_short")
}

func TestTypedOperationErrors(t *testing.T) {
	t.Parallel()
	app := New()
	Get(app, "/missing", func(context.Context, struct{}) (string, error) { return "", NotFound("todo 1 not found") })
	Get(app, "/teapot", func(context.Context, struct{}) (string, error) { return "", &teapotError{} })
	Delete(app, "/x", func(context.Context, struct{}) (string, error) { return "deleted", nil }, StatusCode(http.StatusNoContent))
	Patch(app, "/x", func(context.Context, struct{}) (string, error) { return "patched", nil })
	Head(app, "/x", func(context.Context, struct{}) (string, error) { return "", nil })
	OnError(app, func(w http.ResponseWriter, r *http.Request, _ *teapotError) error {
		return WriteJSON(w, r, http.StatusTeapot, map[string]string{"error": "teapot"})
	})
	OnError(app, func(w http.ResponseWriter, r *http.Request, err *RequestValidationError) error {
		return WriteJSON(w, r, http.StatusBadRequest, map[string]int{"errors": len(err.Errors)})
	})
	Get(app, "/n/{n}", func(_ context.Context, in struct {
		N int `path:"n"`
	},
	) (int, error) {
		return in.N, nil
	})

	assert.JSONEq(t, `{"statusCode":404,"message":"todo 1 not found"}`, do(t, app, "GET", "/missing", "").Body.String())
	assert.Equal(t, http.StatusTeapot, do(t, app, "GET", "/teapot", "").Code)
	rec := do(t, app, "DELETE", "/x", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, `"patched"`, do(t, app, "PATCH", "/x", "").Body.String())
	assert.Equal(t, http.StatusOK, do(t, app, "HEAD", "/x", "").Code)
	assert.JSONEq(t, `{"errors":1}`, do(t, app, "GET", "/n/x", "").Body.String(), "validation errors reach error handlers")
}

func TestRegisterMisuse(t *testing.T) {
	t.Parallel()
	app := New()
	op := func(context.Context, struct{}) (string, error) { return "", nil }
	assert.Panics(t, func() { Register(app, "/no-method", op) })
	assert.Panics(t, func() { Register(app, "GET bad", op) })
	assert.Panics(t, func() { Get(app, "/x/{id}", op) }, "path parameter not bound")
	assert.Panics(t, func() {
		Get(app, "/y", func(context.Context, struct {
			ID int `path:"id"`
		},
		) (string, error) {
			return "", nil
		})
	}, "path field without a wildcard")
	assert.Panics(t, func() {
		Get(app, "/z", func(context.Context, int) (string, error) { return "", nil })
	}, "input is not a struct")
	assert.Panics(t, func() {
		Get(app, "/z", func(context.Context, struct {
			M map[string]string `query:"m"`
		},
		) (string, error) {
			return "", nil
		})
	}, "a map cannot be a query parameter")
	assert.Panics(t, func() {
		Post(app, "/z", func(context.Context, struct {
			Body todo
			Name string `formData:"name"`
		},
		) (string, error) {
			return "", nil
		})
	}, "body and form")
	assert.Panics(t, func() {
		Get(app, "/z", func(context.Context, struct {
			F *multipart.FileHeader `query:"f"`
		},
		) (string, error) {
			return "", nil
		})
	}, "files are form fields")
	assert.Panics(t, func() {
		Get(app, "/z", func(context.Context, struct {
			R bool `query:"r" required:"maybe"` //nolint:revive // struct-tag: the bad value is the point
		},
		) (string, error) {
			return "", nil
		})
	}, "bad required tag")
	assert.Panics(t, func() { RegisterRegexp(app, "", regexp.MustCompile("/r"), op) })
}

func TestTypedRegexpOperation(t *testing.T) {
	t.Parallel()
	app := New()
	RegisterRegexp(app, http.MethodGet, regexp.MustCompile(`/v(?P<version>\d+)/items`), func(_ context.Context, in struct {
		Version int `path:"version" maximum:"3"`
	},
	) (int, error) {
		return in.Version, nil
	})
	assert.Equal(t, "2", do(t, app, "GET", "/v2/items", "").Body.String())
	pythonBody(t, `{"statusCode":422,"detail":[{"loc":["path","version"],"type":"less_than_equal"}]}`, do(t, app, "GET", "/v9/items", "").Body.String())
}

func TestIncludeTypedOperationsWithPrefix(t *testing.T) {
	t.Parallel()
	users := New()
	Get(users, "/{id}", func(_ context.Context, in struct {
		ID int `path:"id"`
	},
	) (int, error) {
		return in.ID, nil
	})
	app := New()
	app.Include(users, "/users")
	assert.Equal(t, "5", do(t, app, "GET", "/users/5", "").Body.String())
	assert.Panics(t, func() { New().Include(users, "/orgs/{org}") }, "the input does not bind the prefix's wildcard")
}

func TestCompressionChoiceOfResponse(t *testing.T) {
	t.Parallel()
	app := New()
	no := false
	Get(app, "/x", func(context.Context, struct{}) (Response[string], error) {
		return Response[string]{Body: strings.Repeat("x", 100), Compress: &no}, nil
	}, Compress())
	assert.Empty(t, do(t, app, "GET", "/x", "", "Accept-Encoding", "gzip").Header().Get("Content-Encoding"))
}

func TestCustomSerializers(t *testing.T) {
	t.Parallel()
	app := New(
		WithSerializer(func(any) ([]byte, error) { return []byte(`"custom"`), nil }),
		WithDeserializer(func([]byte, any) error { return errors.New("no") }),
	)
	Get(app, "/x", func(context.Context, struct{}) (int, error) { return 1, nil })
	Post(app, "/x", func(context.Context, createTodo) (int, error) { return 1, nil })
	assert.Equal(t, `"custom"`, do(t, app, "GET", "/x", "").Body.String())
	rec := do(t, app, "POST", "/x", `{"title":"abc"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, `"custom"`, rec.Body.String(), "error bodies use the serializer too")
}

type address struct {
	City string `json:"city" required:"true" minLength:"2"`
}

type profile struct {
	ID       string    `json:"id" format:"uuid"`
	Born     string    `json:"born" format:"date"`
	Seen     time.Time `json:"seen"`
	Site     string    `json:"site" format:"uri"`
	IP       string    `json:"ip" format:"ipv4"`
	Age      int       `json:"age"`
	Score    float64   `json:"score"`
	Admin    bool      `json:"admin"`
	Address  *address  `json:"address"`
	Labels   []string  `json:"labels"`
	Settings struct {
		Theme string `json:"theme" enum:"dark,light"`
	} `json:"settings"`
}

func TestBodyErrorTypes(t *testing.T) {
	t.Parallel()
	app := New()
	var caught *RequestValidationError
	OnError(app, func(_ http.ResponseWriter, _ *http.Request, err *RequestValidationError) error {
		caught = err
		return err
	})
	Post(app, "/p", func(context.Context, struct{ Body profile }) (bool, error) { return true, nil })

	rec := do(t, app, "POST", "/p", `{"id":"nope","born":"x","seen":"y","site":"::","ip":"300.1.1.1",`+
		`"age":1.5,"score":"high","admin":1,"address":{"city":"R"},"labels":"x","settings":{"theme":"blue"}}`)
	pythonBody(t, `{"statusCode":422,"detail":[`+
		`{"loc":["body","address","city"],"type":"string_too_short"},`+
		`{"loc":["body","admin"],"type":"bool_type"},`+
		`{"loc":["body","age"],"type":"int_from_float"},`+
		`{"loc":["body","born"],"type":"date_parsing"},`+
		`{"loc":["body","id"],"type":"uuid_parsing"},`+
		`{"loc":["body","ip"],"type":"ip_v4_address"},`+
		`{"loc":["body","labels"],"type":"list_type"},`+
		`{"loc":["body","score"],"type":"float_parsing"},`+
		`{"loc":["body","seen"],"type":"datetime_parsing"},`+
		`{"loc":["body","settings","theme"],"type":"enum"},`+
		`{"loc":["body","site"],"type":"url_parsing"}]}`, rec.Body.String())
	require.NotNil(t, caught)
	assert.Contains(t, caught.Error(), "body.address.city: string_too_short")
	assert.NotEmpty(t, caught.Errors[0].Msg, "messages stay for logs")
	assert.JSONEq(t, `{"id":"nope","born":"x","seen":"y","site":"::","ip":"300.1.1.1","age":1.5,"score":"high","admin":1,`+
		`"address":{"city":"R"},"labels":"x","settings":{"theme":"blue"}}`, string(caught.Body))

	rec = do(t, app, "POST", "/p", `{"address":null,"seen":"2024-01-02T03:04:05Z"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestInvalidJSONIsTheOnlyError(t *testing.T) {
	t.Parallel()
	app := New()
	Post(app, "/x/{id}", func(context.Context, struct {
		ID   int `path:"id"`
		Body todo
	},
	) (bool, error) {
		return true, nil
	})
	pythonBody(t, `{"statusCode":422,"detail":[{"loc":["body",1],"type":"json_invalid"}]}`,
		do(t, app, "POST", "/x/abc", "{]").Body.String())
}
