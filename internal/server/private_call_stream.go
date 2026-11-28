package server

import (
	"context"
	"log/slog"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func (h *personHub) publishPrivateCall(update store.PrivateCallUpdate) {
	frame := personFrame{callID: update.ID, callRevision: update.Revision}
	h.enqueue(update.CallerID, frame)
	h.enqueue(update.CalleeID, frame)
}

func (p *personStream) privateCallActor() store.PrivateCallActor {
	return store.PrivateCallActor{PrincipalID: p.principalID, CredentialID: p.credID}
}

func (p *personStream) sendPrivateCallSnapshot(ctx context.Context) bool {
	readCtx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	snap, err := p.s.store.CurrentPrivateCall(readCtx, p.privateCallActor(), p.s.now)
	cancel()
	if err != nil {
		return p.closePrivateCallReadError(ctx, err)
	}
	p.call = snap.Call
	return p.sendDoc(ctx, snap)
}

func (p *personStream) sendPrivateCallUpdate(ctx context.Context, id string, revision int64) bool {
	seen := p.call != nil && p.call.ID == id
	if seen && p.call.Revision >= revision {
		return true
	}
	call, err := p.privateCallForDelivery(ctx, id, revision, seen)
	if err != nil {
		return p.closePrivateCallReadError(ctx, err)
	}
	if call == nil {
		return true
	}
	if !seen && call.State.Terminal() && p.call != nil && !p.call.State.Terminal() {
		// A later live attempt has already replaced the old call on this socket.
		return true
	}
	callee := call.CalleeID == p.principalID
	if seen && callee && p.call.State == schema.PrivateCallStateRinging && call.Revision == 2 {
		stop := schema.PrivateCallStopRingingV1{
			Schema: schema.PrivateCallStopRingingSchemaV1, CallID: id, Revision: revision, State: call.State,
		}
		if !p.sendDoc(ctx, stop) {
			return false
		}
		// Authorization and the revision may have changed during the write.
		call, err = p.privateCallForDelivery(ctx, id, revision, seen)
		if err != nil {
			return p.closePrivateCallReadError(ctx, err)
		}
		if call == nil {
			return true
		}
	}
	if call.State == schema.PrivateCallStateRinging && callee {
		if !p.sendDoc(ctx, schema.PrivateCallRingingV1{Schema: schema.PrivateCallRingingSchemaV1, Call: *call}) {
			return false
		}
	} else if !p.sendDoc(ctx, schema.PrivateCallStateV1{Schema: schema.PrivateCallStateSchemaV1, Call: *call}) {
		return false
	}
	p.call = call
	return true
}

func (p *personStream) privateCallForDelivery(ctx context.Context, id string, revision int64, seen bool) (*schema.PrivateCallV1, error) {
	readCtx, cancel := context.WithTimeout(ctx, wsRecheckTimeout)
	defer cancel()
	return p.s.store.PrivateCallForDelivery(readCtx, p.privateCallActor(), id, revision, seen, p.s.now())
}

func (p *personStream) closePrivateCallReadError(ctx context.Context, err error) bool {
	if p.stillAllowed(ctx) {
		if ctx.Err() == nil {
			slog.ErrorContext(ctx, "person stream: call access check failed", "principal", p.principalID, "error", err)
		}
		closeWS(ctx, p.conn, schema.SocketCloseError, websocket.StatusInternalError, "access check failed")
	}
	return false
}
