package eventhandler

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	jsv "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/schema"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/schema/schematest"
)

// The event handler validated typed operations with
// github.com/santhosh-tekuri/jsonschema/v6 before its own validator. The
// fuzz test below checks that both give the same details (location and
// type) on the schemas the validator supports, with the mapping the event
// handler applied to the library's errors.

func referenceDetails(ve *jsv.ValidationError, instance any, out *[]ValidationErrorDetail) {
	loc := referenceLoc(instance, ve.InstanceLocation)
	switch k := ve.ErrorKind.(type) {
	case *kind.Required:
		for _, name := range k.Missing {
			*out = append(*out, ValidationErrorDetail{Loc: append(slices.Clone(loc), name), Type: errTypeMissing})
		}
	case *kind.AdditionalProperties:
		for _, name := range k.Properties {
			*out = append(*out, ValidationErrorDetail{Loc: append(slices.Clone(loc), name), Type: "extra_forbidden"})
		}
	case *kind.Type:
		*out = append(*out, ValidationErrorDetail{Loc: loc, Type: typeError(k.Got, k.Want)})
	case *kind.Format:
		*out = append(*out, ValidationErrorDetail{Loc: loc, Type: formatError(k.Want)})
	case *kind.AnyOf, *kind.OneOf:
		causes := slices.DeleteFunc(slices.Clone(ve.Causes), referenceNullOnly)
		if len(causes) == 0 {
			*out = append(*out, ValidationErrorDetail{Loc: loc, Type: errTypeValue})
		}
		for _, c := range causes {
			referenceDetails(c, instance, out)
		}
	default:
		if len(ve.Causes) == 0 {
			typ := errTypeValue
			if kp := ve.ErrorKind.KeywordPath(); len(kp) > 0 {
				if t, ok := keywordErrors[kp[0]]; ok {
					typ = t
				}
			}
			*out = append(*out, ValidationErrorDetail{Loc: loc, Type: typ})
			return
		}
		for _, c := range ve.Causes {
			referenceDetails(c, instance, out)
		}
	}
}

func referenceNullOnly(ve *jsv.ValidationError) bool {
	if t, ok := ve.ErrorKind.(*kind.Type); ok {
		return len(t.Want) == 1 && t.Want[0] == typeNull
	}
	if len(ve.Causes) == 1 {
		return referenceNullOnly(ve.Causes[0])
	}
	return false
}

func referenceLoc(instance any, path []string) []any {
	loc := make([]any, 0, len(path))
	cur := instance
	for _, seg := range path {
		switch c := cur.(type) {
		case []any:
			if i, err := strconv.Atoi(seg); err == nil && i >= 0 && i < len(c) {
				loc = append(loc, i)
				cur = c[i]
				continue
			}
			loc, cur = append(loc, seg), nil
		case map[string]any:
			loc, cur = append(loc, seg), c[seg]
		default:
			loc, cur = append(loc, seg), nil
		}
	}
	return loc
}

// publicView is what the client sees of details, sorted as the event
// handler sorts them when nothing else orders them.
func publicView(details []ValidationErrorDetail) []string {
	out := make([]string, len(details))
	for i, d := range details {
		out[i] = fmt.Sprint(d.Loc) + " " + d.Type
	}
	slices.SortStableFunc(out, cmp.Compare)
	return out
}

func compareWithReference(schemaJSON, instanceJSON []byte) error {
	doc, err := jsv.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return fmt.Errorf("reference decoder: %w", err)
	}
	c := jsv.NewCompiler()
	c.DefaultDraft(jsv.Draft2020)
	c.AssertFormat()
	if err = c.AddResource("mem://schema.json", doc); err != nil {
		return fmt.Errorf("reference resource: %w", err)
	}
	ref, err := c.Compile("mem://schema.json")
	if err != nil {
		return nil //nolint:nilerr // a schema the reference rejects is not a test case
	}
	mine, err := schema.Decode(schemaJSON)
	if err != nil {
		return fmt.Errorf("decode the schema: %w", err)
	}
	ours, err := schema.NewCompiler(mine).Compile("#")
	if errors.Is(err, schema.ErrCycle) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	refInstance, err := jsv.UnmarshalJSON(bytes.NewReader(instanceJSON))
	if err != nil {
		return fmt.Errorf("reference instance: %w", err)
	}
	instance, err := schema.Decode(instanceJSON)
	if err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	var want []ValidationErrorDetail
	if err := ref.Validate(refInstance); err != nil {
		ve, ok := errors.AsType[*jsv.ValidationError](err)
		if !ok {
			return fmt.Errorf("reference error %T", err)
		}
		if _, cycle := ve.ErrorKind.(*kind.RefCycle); cycle {
			return nil
		}
		referenceDetails(ve, refInstance, &want)
	}
	got := details(ours.Validate(instance), nil)
	if w, g := publicView(want), publicView(got); !slices.Equal(w, g) {
		return fmt.Errorf("reference %v\nours      %v", w, g)
	}
	return nil
}

func FuzzDetailsAgainstReference(f *testing.F) {
	for _, seed := range []string{"", "\x01\x02\x03", "\xff\x10\x20\x30\x40\x50\x60\x70\x80\x90", "abcdefghijklmnopqrstuvwxyz0123456789"} {
		f.Add([]byte(seed), []byte(seed+"x"))
	}
	f.Fuzz(func(t *testing.T, schemaBytes, instanceBytes []byte) {
		schemaJSON, instanceJSON, err := schematest.Case(schemaBytes, instanceBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := compareWithReference(schemaJSON, instanceJSON); err != nil {
			t.Fatalf("schema %s\ninstance %s\n%v", schemaJSON, instanceJSON, err)
		}
	})
}
