package wa

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unsafe"

	"wacalls/internal/voip/signaling"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

type Socket struct {
	cli *whatsmeow.Client
}

func NewSocket(cli *whatsmeow.Client) *Socket { return &Socket{cli: cli} }

var _ signaling.Socket = (*Socket)(nil)

func (s *Socket) di() *whatsmeow.DangerousInternalClient { return s.cli.DangerousInternals() }

// nodeHandlersValue reaches whatsmeow's unexported nodeHandlers map through reflection and
// returns it as a settable reflect.Value. The map element type is an unexported named func
// type, so callers work through reflect.MakeFunc / MapIndex rather than a type assertion.
func nodeHandlersValue(cli *whatsmeow.Client) (reflect.Value, error) {
	v := reflect.ValueOf(cli).Elem().FieldByName("nodeHandlers")
	if !v.IsValid() {
		return reflect.Value{}, errors.New("whatsmeow client has no nodeHandlers field")
	}
	m := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
	if m.Kind() != reflect.Map || m.IsNil() {
		return reflect.Value{}, errors.New("whatsmeow nodeHandlers is not a live map")
	}
	elem := m.Type().Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	nodeType := reflect.TypeOf((*waBinary.Node)(nil))
	if elem.Kind() != reflect.Func || elem.NumIn() != 2 || elem.NumOut() != 0 ||
		elem.In(0) != ctxType || elem.In(1) != nodeType {
		return reflect.Value{}, errors.New("whatsmeow nodeHandlers has an unexpected element type")
	}
	return m, nil
}

// InstallCallInterceptor wraps whatsmeow's raw <call> handler so fn sees every inbound call
// node first; returning true claims the node, so whatsmeow's generic typeless ack is never
// sent (a <video> upgrade needs a typed ack, which the generic one does not satisfy). It
// reaches an unexported field through reflection, so a whatsmeow bump can break it: the error
// return lets callers fall back to the UnknownCallEvent dual-ack path. Call before Connect.
func (s *Socket) InstallCallInterceptor(fn func(node *waBinary.Node) bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("call interceptor install: %v", r)
		}
	}()
	handlers, hErr := nodeHandlersValue(s.cli)
	if hErr != nil {
		return fmt.Errorf("call interceptor install: %w", hErr)
	}
	key := reflect.ValueOf("call")
	orig := handlers.MapIndex(key)
	wrapped := reflect.MakeFunc(handlers.Type().Elem(), func(args []reflect.Value) []reflect.Value {
		node := args[1].Interface().(*waBinary.Node)
		if fn(node) {
			return nil
		}
		if orig.IsValid() && !orig.IsNil() {
			orig.Call(args)
		}
		return nil
	})
	handlers.SetMapIndex(key, wrapped)
	return nil
}

// CallInterceptorAvailable reports whether the whatsmeow client exposes the nodeHandlers seam
// the interceptor needs, without mutating anything (used by the doctor).
func CallInterceptorAvailable(cli *whatsmeow.Client) bool {
	_, err := nodeHandlersValue(cli)
	return err == nil
}

// CallInterceptorSeamPresent statically reports whether whatsmeow's Client still has the
// nodeHandlers field with the expected element type, without constructing a client. Used by
// the doctor to surface a silent degradation to the dual-ack fallback after a whatsmeow bump.
func CallInterceptorSeamPresent() bool {
	f, ok := reflect.TypeOf(whatsmeow.Client{}).FieldByName("nodeHandlers")
	if !ok || f.Type.Kind() != reflect.Map {
		return false
	}
	elem := f.Type.Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	nodeType := reflect.TypeOf((*waBinary.Node)(nil))
	return elem.Kind() == reflect.Func && elem.NumIn() == 2 && elem.NumOut() == 0 &&
		elem.In(0) == ctxType && elem.In(1) == nodeType
}

func (s *Socket) OwnPN() types.JID { return s.di().GetOwnID() }

func (s *Socket) OwnLID() types.JID { return s.di().GetOwnLID() }

func (s *Socket) AccountDeviceIdentityNode() (waBinary.Node, bool) {
	if s.cli.Store == nil || s.cli.Store.Account == nil {
		return waBinary.Node{}, false
	}
	return s.di().MakeDeviceIdentityNode(), true
}

func (s *Socket) SendNode(ctx context.Context, node waBinary.Node) error {
	return s.di().SendNode(ctx, node)
}

func (s *Socket) Query(ctx context.Context, node waBinary.Node) (*waBinary.Node, error) {
	id, _ := node.Attrs["id"].(string)
	if id == "" {
		return nil, s.di().SendNode(ctx, node)
	}
	di := s.di()
	ch := di.WaitResponse(id)
	if err := di.SendNode(ctx, node); err != nil {
		di.CancelResponse(id, ch)
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(15 * time.Second):
		di.CancelResponse(id, ch)
		return nil, nil
	case <-ctx.Done():
		di.CancelResponse(id, ch)
		return nil, ctx.Err()
	}
}

func (s *Socket) GetUSyncDevices(ctx context.Context, jids []types.JID) ([]types.JID, error) {
	return s.cli.GetUserDevices(ctx, jids)
}

func (s *Socket) AssertSessions(ctx context.Context, jids []types.JID, force bool) error {
	return nil
}

func (s *Socket) CreateParticipantNodes(ctx context.Context, devices []types.JID, callKey []byte, encAttrs waBinary.Attrs) ([]waBinary.Node, bool, error) {
	plaintext, err := signaling.EncodeCallKeyMessage(callKey)
	if err != nil {
		return nil, false, err
	}
	id := s.cli.GenerateMessageID()
	return s.di().EncryptMessageForDevices(ctx, devices, id, plaintext, plaintext, encAttrs)
}

func (s *Socket) DecryptCallKey(ctx context.Context, from types.JID, encChild *waBinary.Node) ([]byte, error) {
	typ, _ := encChild.Attrs["type"].(string)
	isPreKey := typ == "pkmsg"
	plaintext, _, err := s.di().DecryptDM(ctx, encChild, from, isPreKey, time.Now())
	if err != nil {
		return nil, err
	}
	return signaling.DecodeCallKeyPlaintext(plaintext)
}

func (s *Socket) GetTCToken(ctx context.Context, jid types.JID) ([]byte, error) {
	if s.cli.Store == nil || s.cli.Store.PrivacyTokens == nil {
		return nil, nil
	}
	for _, cand := range []types.JID{s.ResolveLIDForPN(ctx, jid).ToNonAD(), jid.ToNonAD()} {
		if cand.IsEmpty() {
			continue
		}
		tok, err := s.cli.Store.PrivacyTokens.GetPrivacyToken(ctx, cand)
		if err != nil {
			return nil, err
		}
		if tok != nil && len(tok.Token) > 0 {
			return tok.Token, nil
		}
	}
	return nil, nil
}

func (s *Socket) ResolveLIDForPN(ctx context.Context, pn types.JID) types.JID {
	if pn.Server == types.HiddenUserServer {
		return pn
	}
	if s.cli.Store != nil && s.cli.Store.LIDs != nil {
		if lid, err := s.cli.Store.LIDs.GetLIDForPN(ctx, pn); err == nil && !lid.IsEmpty() {
			return lid
		}
	}
	if info, err := s.cli.GetUserInfo(ctx, []types.JID{pn}); err == nil {
		if lid := info[pn].LID; !lid.IsEmpty() {
			return lid
		}
	}
	return pn
}
