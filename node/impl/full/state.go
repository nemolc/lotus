package full

import (
	"context"
	"fmt"
	"strconv"

	"github.com/filecoin-project/go-lotus/chain"
	"github.com/filecoin-project/go-lotus/chain/gen"
	"github.com/filecoin-project/go-lotus/chain/stmgr"
	"github.com/filecoin-project/go-lotus/chain/store"
	"github.com/filecoin-project/go-lotus/chain/types"
	"github.com/filecoin-project/go-lotus/chain/vm"
	"github.com/filecoin-project/go-lotus/chain/wallet"
	"github.com/filecoin-project/go-lotus/lib/bufbstore"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-lotus/api"
	"github.com/filecoin-project/go-lotus/chain/actors"
	"github.com/filecoin-project/go-lotus/chain/address"
	"github.com/filecoin-project/go-lotus/chain/state"

	"github.com/ipfs/go-hamt-ipld"
	cbor "github.com/ipfs/go-ipld-cbor"
	"go.uber.org/fx"
)

type StateAPI struct {
	fx.In

	// TODO: the wallet here is only needed because we have the MinerCreateBlock
	// API attached to the state API. It probably should live somewhere better
	Wallet *wallet.Wallet

	StateManager *stmgr.StateManager
	Chain        *store.ChainStore
}

func (a *StateAPI) StateMinerSectors(ctx context.Context, addr address.Address) ([]*api.SectorInfo, error) {
	ts := a.StateManager.ChainStore().GetHeaviestTipSet()

	stc, err := a.StateManager.TipSetState(ts.Cids())
	if err != nil {
		return nil, err
	}

	cst := hamt.CSTFromBstore(a.StateManager.ChainStore().Blockstore())

	st, err := state.LoadStateTree(cst, stc)
	if err != nil {
		return nil, err
	}

	act, err := st.GetActor(addr)
	if err != nil {
		return nil, err
	}

	var minerState actors.StorageMinerActorState
	if err := cst.Get(ctx, act.Head, &minerState); err != nil {
		return nil, err
	}

	nd, err := hamt.LoadNode(ctx, cst, minerState.Sectors)
	if err != nil {
		return nil, err
	}

	var sinfos []*api.SectorInfo
	// Note to self: the hamt isnt a great data structure to use here... need to implement the sector set
	err = nd.ForEach(ctx, func(k string, val interface{}) error {
		sid, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return err
		}

		bval, ok := val.([]byte)
		if !ok {
			return fmt.Errorf("expected to get bytes in sector set hamt")
		}

		var comms [][]byte
		if err := cbor.DecodeInto(bval, &comms); err != nil {
			return err
		}

		sinfos = append(sinfos, &api.SectorInfo{
			SectorID: sid,
			CommR:    comms[0],
			CommD:    comms[1],
		})
		return nil
	})
	return sinfos, nil
}

func (a *StateAPI) StateMinerProvingSet(ctx context.Context, addr address.Address) ([]*api.SectorInfo, error) {
	ts := a.Chain.GetHeaviestTipSet()

	stc, err := a.StateManager.TipSetState(ts.Cids())
	if err != nil {
		return nil, err
	}

	cst := hamt.CSTFromBstore(a.Chain.Blockstore())

	st, err := state.LoadStateTree(cst, stc)
	if err != nil {
		return nil, err
	}

	act, err := st.GetActor(addr)
	if err != nil {
		return nil, err
	}

	var minerState actors.StorageMinerActorState
	if err := cst.Get(ctx, act.Head, &minerState); err != nil {
		return nil, err
	}

	nd, err := hamt.LoadNode(ctx, cst, minerState.ProvingSet)
	if err != nil {
		return nil, err
	}

	var sinfos []*api.SectorInfo
	// Note to self: the hamt isnt a great data structure to use here... need to implement the sector set
	err = nd.ForEach(ctx, func(k string, val interface{}) error {
		sid, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			return err
		}

		bval, ok := val.([]byte)
		if !ok {
			return fmt.Errorf("expected to get bytes in sector set hamt")
		}

		var comms [][]byte
		if err := cbor.DecodeInto(bval, &comms); err != nil {
			return err
		}

		sinfos = append(sinfos, &api.SectorInfo{
			SectorID: sid,
			CommR:    comms[0],
			CommD:    comms[1],
		})
		return nil
	})
	return sinfos, nil
}

func (a *StateAPI) StateMinerPower(ctx context.Context, maddr address.Address, ts *types.TipSet) (api.MinerPower, error) {
	var err error
	enc, err := actors.SerializeParams(&actors.PowerLookupParams{maddr})
	if err != nil {
		return api.MinerPower{}, err
	}

	var mpow types.BigInt

	if maddr != address.Undef {
		ret, err := stmgr.Call(ctx, a.StateManager, &types.Message{
			From:   maddr,
			To:     actors.StorageMarketAddress,
			Method: actors.SMAMethods.PowerLookup,
			Params: enc,
		}, ts)
		if err != nil {
			return api.MinerPower{}, xerrors.Errorf("failed to get miner power from chain: %w", err)
		}
		if ret.ExitCode != 0 {
			return api.MinerPower{}, xerrors.Errorf("failed to get miner power from chain (exit code %d)", ret.ExitCode)
		}

		mpow = types.BigFromBytes(ret.Return)
	}

	ret, err := stmgr.Call(ctx, a.StateManager, &types.Message{
		From:   actors.StorageMarketAddress,
		To:     actors.StorageMarketAddress,
		Method: actors.SMAMethods.GetTotalStorage,
	}, ts)
	if err != nil {
		return api.MinerPower{}, xerrors.Errorf("failed to get total power from chain: %w", err)
	}
	if ret.ExitCode != 0 {
		return api.MinerPower{}, xerrors.Errorf("failed to get total power from chain (exit code %d)", ret.ExitCode)
	}

	tpow := types.BigFromBytes(ret.Return)

	return api.MinerPower{
		MinerPower: mpow,
		TotalPower: tpow,
	}, nil
}

func (a *StateAPI) StateMinerWorker(ctx context.Context, m address.Address, ts *types.TipSet) (address.Address, error) {
	ret, err := stmgr.Call(ctx, a.StateManager, &types.Message{
		From:   m,
		To:     m,
		Method: actors.MAMethods.GetWorkerAddr,
	}, ts)
	if err != nil {
		return address.Undef, xerrors.Errorf("failed to get miner worker addr: %w", err)
	}

	if ret.ExitCode != 0 {
		return address.Undef, xerrors.Errorf("failed to get miner worker addr (exit code %d)", ret.ExitCode)
	}

	w, err := address.NewFromBytes(ret.Return)
	if err != nil {
		return address.Undef, xerrors.Errorf("GetWorkerAddr returned malformed address: %w", err)
	}

	return w, nil
}

func (a *StateAPI) StateCall(ctx context.Context, msg *types.Message, ts *types.TipSet) (*types.MessageReceipt, error) {
	return stmgr.Call(ctx, a.StateManager, msg, ts)
}

func (a *StateAPI) stateForTs(ts *types.TipSet) (*state.StateTree, error) {
	if ts == nil {
		ts = a.Chain.GetHeaviestTipSet()
	}

	st, err := a.StateManager.TipSetState(ts.Cids())
	if err != nil {
		return nil, err
	}

	buf := bufbstore.NewBufferedBstore(a.Chain.Blockstore())
	cst := hamt.CSTFromBstore(buf)
	return state.LoadStateTree(cst, st)
}

func (a *StateAPI) StateGetActor(ctx context.Context, actor address.Address, ts *types.TipSet) (*types.Actor, error) {
	state, err := a.stateForTs(ts)
	if err != nil {
		return nil, err
	}

	return state.GetActor(actor)
}

func (a *StateAPI) StateReadState(ctx context.Context, act *types.Actor, ts *types.TipSet) (*api.ActorState, error) {
	state, err := a.stateForTs(ts)
	if err != nil {
		return nil, err
	}

	blk, err := state.Store.Blocks.GetBlock(ctx, act.Head)
	if err != nil {
		return nil, err
	}

	oif, err := vm.DumpActorState(act.Code, blk.RawData())
	if err != nil {
		return nil, err
	}

	return &api.ActorState{
		Balance: act.Balance,
		State:   oif,
	}, nil
}

// This is on StateAPI because miner.Miner requires this, and MinerAPI requires miner.Miner
func (a *StateAPI) MinerCreateBlock(ctx context.Context, addr address.Address, parents *types.TipSet, tickets []*types.Ticket, proof types.ElectionProof, msgs []*types.SignedMessage, ts uint64) (*chain.BlockMsg, error) {
	fblk, err := gen.MinerCreateBlock(ctx, a.StateManager, a.Wallet, addr, parents, tickets, proof, msgs, ts)
	if err != nil {
		return nil, err
	}

	var out chain.BlockMsg
	out.Header = fblk.Header
	for _, msg := range fblk.BlsMessages {
		out.BlsMessages = append(out.BlsMessages, msg.Cid())
	}
	for _, msg := range fblk.SecpkMessages {
		out.SecpkMessages = append(out.SecpkMessages, msg.Cid())
	}

	return &out, nil
}
