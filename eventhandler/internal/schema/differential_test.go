package schema_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	jsv "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/schema"
	"github.com/alessandrolattao/powertools-lambda-go/eventhandler/internal/schema/schematest"
)

// agree reports whether both validators agree on the instance, or why not.
func agree(schemaJSON, instanceJSON []byte) error {
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
		return nil //nolint:nilerr // a schema the reference rejects (a recursive $ref) is not a test case
	}
	ours, err := decodeSchema(schemaJSON)
	if errors.Is(err, schema.ErrCycle) {
		return nil // the reference reports the cycle when it validates
	}
	if err != nil {
		return err
	}
	inst, err := jsv.UnmarshalJSON(bytes.NewReader(instanceJSON))
	if err != nil {
		return fmt.Errorf("reference instance: %w", err)
	}
	mine, err := schema.Decode(instanceJSON)
	if err != nil {
		return fmt.Errorf("instance: %w", err)
	}
	refValid := ref.Validate(inst) == nil
	failures := ours.Validate(mine)
	if refValid != (len(failures) == 0) {
		return fmt.Errorf("reference valid=%v, ours %v", refValid, failures)
	}
	return nil
}

func decodeSchema(data []byte) (*schema.Schema, error) {
	doc, err := schema.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode the schema: %w", err)
	}
	s, err := schema.NewCompiler(doc).Compile("#")
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return s, nil
}

func FuzzAgainstReference(f *testing.F) {
	for _, seed := range []string{"", "\x01\x02\x03", "\xff\x10\x20\x30\x40\x50\x60\x70\x80\x90", "abcdefghijklmnopqrstuvwxyz0123456789"} {
		f.Add([]byte(seed), []byte(seed+"x"))
	}
	f.Fuzz(func(t *testing.T, schemaBytes, instanceBytes []byte) {
		schemaJSON, instanceJSON, err := schematest.Case(schemaBytes, instanceBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := agree(schemaJSON, instanceJSON); err != nil {
			t.Fatalf("schema %s\ninstance %s\n%v", schemaJSON, instanceJSON, err)
		}
	})
}

// FuzzDecode compares the decoder of request bodies with the reference's.
func FuzzDecode(f *testing.F) {
	for _, seed := range []string{`{"a":[1,2.5e3,"x",null,true]}`, `{"a":1,"a":2}`, "[1,", `"é"`, " 12345678901234567890 ", "{} x"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ref, refErr := jsv.UnmarshalJSON(bytes.NewReader(data))
		ours, err := schema.Decode(data)
		if (refErr == nil) != (err == nil) {
			t.Fatalf("%q: reference error %v, ours %v", data, refErr, err)
		}
		if err == nil && fmt.Sprint(ref) != fmt.Sprint(ours) {
			t.Fatalf("%q: reference %v, ours %v", data, ref, ours)
		}
	})
}
