package edgevalidate_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	edgevalidate "github.com/onebox-faas/faas/pkg/edgevalidate"
)

// schemaFor is a small helper that returns a freshly-formatted
// schema with the given properties block. Used by the table
// tests below to keep the test bodies compact.
func schemaFor(props map[string]string) string {
	var b bytes.Buffer
	b.WriteString(`{"type":"object","properties":{`)
	first := true
	for k, v := range props {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteByte('"')
		b.WriteString(k)
		b.WriteString(`":{"type":"`)
		b.WriteString(v)
		b.WriteString(`"}`)
	}
	b.WriteString(`},"required":["name"]}`)
	return b.String()
}

func TestCompile_HappyPath(t *testing.T) {
	t.Parallel()
	raw := []byte(schemaFor(map[string]string{"name": "string", "age": "integer"}))
	c, err := edgevalidate.Compile(raw, false)
	if err != nil {
		t.Fatalf("Compile: unexpected error %v", err)
	}
	if c == nil || c.Schema == nil {
		t.Fatal("Compile: nil compiled schema")
	}
	if c.Digest == [32]byte{} {
		t.Fatal("Compile: zero digest")
	}
}

func TestCompile_RejectsEmpty(t *testing.T) {
	t.Parallel()
	_, err := edgevalidate.Compile(nil, false)
	if !errors.Is(err, edgevalidate.ErrSchemaEmpty) {
		t.Fatalf("nil schema: want ErrSchemaEmpty, got %v", err)
	}
	_, err = edgevalidate.Compile([]byte{}, false)
	if !errors.Is(err, edgevalidate.ErrSchemaEmpty) {
		t.Fatalf("empty schema: want ErrSchemaEmpty, got %v", err)
	}
}

func TestCompile_RejectsTooLarge(t *testing.T) {
	t.Parallel()
	// One byte over the cap.
	huge := make([]byte, edgevalidate.MaxSchemaBytes+1)
	for i := range huge {
		huge[i] = ' '
	}
	// Trivially-valid JSON shape that overflows.
	huge[0] = '{'
	huge[huge[len(huge)-1]] = '}'
	_, err := edgevalidate.Compile(huge, false)
	if !errors.Is(err, edgevalidate.ErrSchemaTooLarge) {
		t.Fatalf("oversize schema: want ErrSchemaTooLarge, got %v", err)
	}
}

func TestCompile_RejectsExternalRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		schema string
	}{
		{"http-ref", `{"type":"object","$ref":"http://internal.example.com/x.json"}`},
		{"https-ref", `{"type":"object","$ref":"https://internal.example.com/x.json"}`},
		{"https-id", `{"$id":"https://internal.example.com/x.json","type":"object"}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := edgevalidate.Compile([]byte(tc.schema), false)
			if !errors.Is(err, edgevalidate.ErrSchemaExternalRef) {
				t.Fatalf("want ErrSchemaExternalRef, got %v", err)
			}
		})
	}
}

func TestCompile_AcceptsInternalJSONPointerRef(t *testing.T) {
	t.Parallel()
	// JSON Pointer refs are local; should not trigger the strip.
	schema := `{
		"type": "object",
		"definitions": {"name": {"type": "string"}},
		"$ref": "#/definitions/name"
	}`
	if _, err := edgevalidate.Compile([]byte(schema), false); err != nil {
		t.Fatalf("internal $ref: want nil error, got %v", err)
	}
}

func TestCompile_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := edgevalidate.Compile([]byte(`{"type":"object",`), false)
	if !errors.Is(err, edgevalidate.ErrSchemaInvalid) {
		t.Fatalf("malformed JSON: want ErrSchemaInvalid, got %v", err)
	}
}

func TestValidate_HappyPath(t *testing.T) {
	t.Parallel()
	schema := []byte(schemaFor(map[string]string{"name": "string"}))
	c, err := edgevalidate.Compile(schema, false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	fe, err := c.Validate([]byte(`{"name":"alice"}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if fe != nil {
		t.Fatalf("expected nil FieldError on success, got %+v", fe)
	}
}

func TestValidate_TypeMismatch(t *testing.T) {
	t.Parallel()
	schema := []byte(schemaFor(map[string]string{"name": "string"}))
	c, err := edgevalidate.Compile(schema, false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	fe, err := c.Validate([]byte(`{"name":123}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if fe == nil {
		t.Fatal("expected FieldError on type mismatch, got nil")
	}
	if !strings.Contains(fe.Expected, "string") && fe.Expected != "type" {
		t.Fatalf("expected Expected to mention 'string' or 'type', got %q", fe.Expected)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	t.Parallel()
	schema := []byte(schemaFor(map[string]string{"name": "string"}))
	c, err := edgevalidate.Compile(schema, false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	fe, err := c.Validate([]byte(`{}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if fe == nil {
		t.Fatal("expected FieldError on missing required, got nil")
	}
	if fe.Field != "/name" && fe.Field != "" {
		t.Fatalf("expected Field='/name' or '', got %q", fe.Field)
	}
}

func TestValidate_NonJSONBody(t *testing.T) {
	t.Parallel()
	schema := []byte(schemaFor(map[string]string{"name": "string"}))
	c, err := edgevalidate.Compile(schema, false)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	fe, err := c.Validate([]byte(`not json at all`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if fe == nil || fe.Expected != "json" {
		t.Fatalf("expected FieldError{Expected:'json'}, got %+v", fe)
	}
}

func TestValidate_NilCompiledSchema(t *testing.T) {
	t.Parallel()
	var c *edgevalidate.CompiledSchema
	_, err := c.Validate([]byte(`{}`))
	if !errors.Is(err, edgevalidate.ErrSchemaInvalid) {
		t.Fatalf("nil schema: want ErrSchemaInvalid, got %v", err)
	}
}