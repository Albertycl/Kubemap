package query

import (
	"encoding/json"

	"kubemap/agent/cluster/chain"
	"kubemap/agent/cluster/netpol"
	"kubemap/pkg/protocol"
)

// objectChain answers object.chain (KM-35): the whole chain of one object,
// or the objects whose ID contains a text. It reads only the cache and the
// analyses built on it.
func (h *Handler) objectChain(req *protocol.Request) (*protocol.Response, error) {
	in := h.analyse()
	snap, err := netpol.Load(h.Cache)
	if err != nil {
		snap = nil // policies are left out, with a note
	}
	var res *protocol.ObjectChain
	if q := req.Chain.Query; q != "" {
		res = &protocol.ObjectChain{Matches: chain.Find(in.Graph, snap, q)}
	} else {
		ci := chain.Inputs{Graph: in.Graph, Routes: in.Routes, Deps: in.Dependencies, Apps: in.Apps, Netpol: snap}
		var ok bool
		if res, ok = chain.Build(ci, req.Chain.ID); !ok {
			return nil, perr(protocol.CodeNotFound, "no object %s in the graph", req.Chain.ID)
		}
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "the chain is %d bytes", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}
