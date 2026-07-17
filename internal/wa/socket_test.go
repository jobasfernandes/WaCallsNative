package wa

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/store"
)

func setCallHandler(cli *whatsmeow.Client, fn func(context.Context, *waBinary.Node)) error {
	handlers, err := nodeHandlersValue(cli)
	if err != nil {
		return err
	}
	wrapped := reflect.MakeFunc(handlers.Type().Elem(), func(args []reflect.Value) []reflect.Value {
		fn(args[0].Interface().(context.Context), args[1].Interface().(*waBinary.Node))
		return nil
	})
	handlers.SetMapIndex(reflect.ValueOf("call"), wrapped)
	return nil
}

func getCallHandler(cli *whatsmeow.Client) (func(context.Context, *waBinary.Node), error) {
	handlers, err := nodeHandlersValue(cli)
	if err != nil {
		return nil, err
	}
	v := handlers.MapIndex(reflect.ValueOf("call"))
	if !v.IsValid() {
		return nil, errors.New("no call handler registered")
	}
	return func(ctx context.Context, node *waBinary.Node) {
		v.Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(node)})
	}, nil
}

func TestInstallCallInterceptor(t *testing.T) {
	cli := whatsmeow.NewClient(&store.Device{}, nil)
	s := NewSocket(cli)

	var origCalled, fnCalled bool
	claim := false

	if err := setCallHandler(cli, func(_ context.Context, _ *waBinary.Node) { origCalled = true }); err != nil {
		t.Fatalf("test setup: %v", err)
	}
	if err := s.InstallCallInterceptor(func(node *waBinary.Node) bool {
		fnCalled = true
		return claim
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	handler, err := getCallHandler(cli)
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}

	node := &waBinary.Node{Tag: "call"}
	claim = true
	handler(context.Background(), node)
	if !fnCalled || origCalled {
		t.Fatalf("claimed node must not reach the original handler (fn=%v orig=%v)", fnCalled, origCalled)
	}

	fnCalled, origCalled, claim = false, false, false
	handler(context.Background(), node)
	if !fnCalled || !origCalled {
		t.Fatalf("unclaimed node must pass through (fn=%v orig=%v)", fnCalled, origCalled)
	}
}

func TestCallInterceptorAvailable(t *testing.T) {
	cli := whatsmeow.NewClient(&store.Device{}, nil)
	if !CallInterceptorAvailable(cli) {
		t.Fatal("interceptor seam must be available on the pinned whatsmeow")
	}
}
