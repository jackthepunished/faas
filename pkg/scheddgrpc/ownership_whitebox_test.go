// ownership_whitebox_test.go — fill pkg/scheddgrpc coverage of the
// ownership guard helpers (authorizeApp, authorizeInstance,
// OwnerNodeID.String) and the Server.WithOwner wiring seam.
//
// Whitebox `package scheddgrpc` so we can drive authorizeApp /
// authorizeInstance directly with a fake AppResolver and assert
// the gRPC status code + message — the bufconn tests above cover
// the happy-path integration but don't drive the guard branches
// independently.

package scheddgrpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/onebox-faas/faas/pkg/state"
)

// fakeResolver is an in-memory AppResolver for the ownership guard
// tests. Both methods are independently controllable so the test
// can pin a specific failure shape (NotFound, internal, mismatch).
type fakeResolver struct {
	apps     map[string]state.App
	insts    map[string]state.Instance
	appErr   error
	instErr  error
	appCalls int
}

func (f *fakeResolver) AppByID(_ context.Context, id string) (state.App, error) {
	f.appCalls++
	if f.appErr != nil {
		return state.App{}, f.appErr
	}
	return f.apps[id], nil
}

func (f *fakeResolver) InstanceByID(_ context.Context, id string) (state.Instance, error) {
	if f.instErr != nil {
		return state.Instance{}, f.instErr
	}
	return f.insts[id], nil
}

// --- OwnerNodeID.String ------------------------------------------

func TestOwnerNodeID_String_EmptyReturnsLegacyMarker(t *testing.T) {
	if got := OwnerNodeID("").String(); got != "<legacy single-box>" {
		t.Errorf("String() = %q, want legacy marker", got)
	}
}

func TestOwnerNodeID_String_NonEmptyReturnsValue(t *testing.T) {
	if got := OwnerNodeID("node-A").String(); got != "node-A" {
		t.Errorf("String() = %q, want node-A", got)
	}
}

// --- authorizeApp -----------------------------------------------

// Empty OwnerNodeID short-circuits to in-process without touching
// the resolver (Phase 2 / Gate A legacy posture).
func TestAuthorizeApp_EmptyOwnerShortCircuits(t *testing.T) {
	res := &fakeResolver{}
	_, err := authorizeApp(context.Background(), "", res, "app-1")
	if err != nil {
		t.Fatalf("empty owner: err = %v, want nil", err)
	}
	if res.appCalls != 0 {
		t.Errorf("resolver.AppByID called %d times; want 0 on short-circuit", res.appCalls)
	}
}

// AppResolver returns NotFound → authorizeApp maps to gRPC NotFound.
func TestAuthorizeApp_AppResolverNotFound(t *testing.T) {
	res := &fakeResolver{appErr: errors.New("sql: no rows in result set")}
	_, err := authorizeApp(context.Background(), "node-A", res, "app-1")
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code = %v, want NotFound", got)
	}
	if !strings.Contains(status.Convert(err).Message(), "app-1") {
		t.Errorf("err = %v, want substring app-1", err)
	}
}

// app.NodeID != owner → FailedPrecondition with both ids in the
// message (operator-triageable per the comment at ownership.go:67).
func TestAuthorizeApp_AppResolverWrongNode(t *testing.T) {
	appID := uuid.New().String()
	res := &fakeResolver{apps: map[string]state.App{
		appID: {ID: appID, NodeID: "node-B"},
	}}
	_, err := authorizeApp(context.Background(), "node-A", res, appID)
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", got)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "node-B") || !strings.Contains(msg, "node-A") {
		t.Errorf("err msg = %q, want both node ids", msg)
	}
}

// app.NodeID == owner → success, no error.
func TestAuthorizeApp_AppResolverCorrectNode(t *testing.T) {
	appID := uuid.New().String()
	res := &fakeResolver{apps: map[string]state.App{
		appID: {ID: appID, NodeID: "node-A"},
	}}
	app, err := authorizeApp(context.Background(), "node-A", res, appID)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if app.ID != appID {
		t.Errorf("app.ID = %q, want %q", app.ID, appID)
	}
}

// --- authorizeInstance ------------------------------------------

// Empty OwnerNodeID short-circuits the same way as authorizeApp.
func TestAuthorizeInstance_EmptyOwnerShortCircuits(t *testing.T) {
	res := &fakeResolver{}
	_, err := authorizeInstance(context.Background(), "", res, "i-1")
	if err != nil {
		t.Fatalf("empty owner: err = %v, want nil", err)
	}
}

// InstanceByID error → gRPC NotFound.
func TestAuthorizeInstance_InstanceNotFound(t *testing.T) {
	res := &fakeResolver{instErr: errors.New("sql: no rows in result set")}
	_, err := authorizeInstance(context.Background(), "node-A", res, "i-1")
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code = %v, want NotFound", got)
	}
	if !strings.Contains(status.Convert(err).Message(), "i-1") {
		t.Errorf("err = %v, want substring i-1", err)
	}
}

// Instance found but parent AppByID errors → gRPC NotFound with
// the parent-app id in the message.
func TestAuthorizeInstance_ParentAppNotFound(t *testing.T) {
	instID := uuid.New().String()
	instAppID := uuid.New().String()
	res := &fakeResolver{
		insts: map[string]state.Instance{
			instID: {ID: instID, AppID: instAppID},
		},
		appErr: errors.New("sql: no rows in result set"),
	}
	_, err := authorizeInstance(context.Background(), "node-A", res, instID)
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("code = %v, want NotFound", got)
	}
	if !strings.Contains(status.Convert(err).Message(), instAppID) {
		t.Errorf("err = %v, want parent app id in msg", err)
	}
}

// Instance + parent app found, but parent app is owned by a
// different node → FailedPrecondition with all three ids.
func TestAuthorizeInstance_WrongNode(t *testing.T) {
	instID := uuid.New().String()
	instAppID := uuid.New().String()
	res := &fakeResolver{
		insts: map[string]state.Instance{instID: {ID: instID, AppID: instAppID}},
		apps:  map[string]state.App{instAppID: {ID: instAppID, NodeID: "node-B"}},
	}
	_, err := authorizeInstance(context.Background(), "node-A", res, instID)
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", got)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "node-B") || !strings.Contains(msg, "node-A") {
		t.Errorf("err msg = %q, want both node ids", msg)
	}
}

// Happy path: instance + parent app both belong to this owner.
func TestAuthorizeInstance_CorrectNode(t *testing.T) {
	instID := uuid.New().String()
	instAppID := uuid.New().String()
	res := &fakeResolver{
		insts: map[string]state.Instance{instID: {ID: instID, AppID: instAppID}},
		apps:  map[string]state.App{instAppID: {ID: instAppID, NodeID: "node-A"}},
	}
	ins, err := authorizeInstance(context.Background(), "node-A", res, instID)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ins.ID != instID {
		t.Errorf("ins.ID = %q, want %q", ins.ID, instID)
	}
}

// --- Server.WithOwner -------------------------------------------

// WithOwner on a nil receiver is a defensive no-op (so a future
// caller that dials s.WithOwner before s is fully constructed
// doesn't panic).
func TestServer_WithOwner_NilReceiver(t *testing.T) {
	var s *Server
	got := s.WithOwner("node-A", &fakeResolver{})
	if got != nil {
		t.Errorf("WithOwner(nil receiver) = %v, want nil", got)
	}
}

// WithOwner stamps the owner + resolver on the receiver.
func TestServer_WithOwner_SetsOwnerAndResolver(t *testing.T) {
	res := &fakeResolver{}
	s := New(nil, nil, nil).WithOwner("node-A", res)
	if string(s.owner) != "node-A" {
		t.Errorf("owner = %q, want node-A", string(s.owner))
	}
	if s.resolver == nil {
		t.Errorf("resolver not set")
	}
}
