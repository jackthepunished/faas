package edgevalidate_test

import (
	"context"
	"errors"
	"testing"

	edgevalidate "github.com/onebox-faas/faas/pkg/edgevalidate"
)

func TestManager_CompileSchema_PopulatesCache(t *testing.T) {
	t.Parallel()
	m := edgevalidate.NewManager(nil)
	compiled, err := m.CompileSchema([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	if compiled == nil {
		t.Fatal("nil compiled schema")
	}
	if m.Cache().Len() != 1 {
		t.Fatalf("cache Len after CompileSchema: want 1, got %d", m.Cache().Len())
	}
}

func TestManager_Validate_HappyPath(t *testing.T) {
	t.Parallel()
	m := edgevalidate.NewManager(nil)
	compiled, err := m.CompileSchema([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	res, err := m.Validate(context.Background(), &edgevalidate.In{
		Body:        []byte(`{"name":"alice"}`),
		ContentType: "application/json",
	}, &edgevalidate.Rule{SchemaDigest: compiled.Digest})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !res.OK {
		t.Fatalf("Validate: want OK=true, got FirstError=%+v", res.FirstError)
	}
	if res.SchemaDigest != compiled.Digest {
		t.Fatalf("SchemaDigest: want %x, got %x", compiled.Digest, res.SchemaDigest)
	}
}

func TestManager_Validate_FailureCarriesFieldError(t *testing.T) {
	t.Parallel()
	m := edgevalidate.NewManager(nil)
	compiled, err := m.CompileSchema([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	res, err := m.Validate(context.Background(), &edgevalidate.In{
		Body:        []byte(`{"name":123}`),
		ContentType: "application/json",
	}, &edgevalidate.Rule{SchemaDigest: compiled.Digest})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.OK {
		t.Fatal("Validate: want OK=false, got true")
	}
	if res.FirstError == nil {
		t.Fatal("Validate: FirstError is nil")
	}
	if res.FirstError.Field != "/name" {
		t.Fatalf("Field: want '/name', got %q", res.FirstError.Field)
	}
}

func TestManager_Validate_MissingDigestReturnsSchemaInvalid(t *testing.T) {
	t.Parallel()
	// Cold cache + unknown digest: the defensive miss path
	// should surface ErrSchemaInvalid (broken schema / deploy
	// bug). The handler maps this to 500.
	m := edgevalidate.NewManager(nil)
	var bogus [32]byte
	bogus[0] = 0xDE
	bogus[1] = 0xAD
	_, err := m.Validate(context.Background(), &edgevalidate.In{
		Body: []byte(`{}`),
	}, &edgevalidate.Rule{SchemaDigest: bogus})
	if !errors.Is(err, edgevalidate.ErrSchemaInvalid) {
		t.Fatalf("unknown digest: want ErrSchemaInvalid, got %v", err)
	}
}

func TestManager_Validate_NilReqOrRule(t *testing.T) {
	t.Parallel()
	m := edgevalidate.NewManager(nil)
	compiled, err := m.CompileSchema([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	if _, err := m.Validate(context.Background(), nil, &edgevalidate.Rule{SchemaDigest: compiled.Digest}); err == nil {
		t.Fatal("nil req: want error, got nil")
	}
	if _, err := m.Validate(context.Background(), &edgevalidate.In{Body: []byte(`{}`)}, nil); err == nil {
		t.Fatal("nil rule: want error, got nil")
	}
}

func TestManager_Validate_CancelledContext(t *testing.T) {
	t.Parallel()
	m := edgevalidate.NewManager(nil)
	compiled, err := m.CompileSchema([]byte(sampleSchema), false)
	if err != nil {
		t.Fatalf("CompileSchema: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = m.Validate(ctx, &edgevalidate.In{Body: []byte(`{}`)},
		&edgevalidate.Rule{SchemaDigest: compiled.Digest})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: want context.Canceled, got %v", err)
	}
}