package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/ipfs/go-cid"
	"github.com/urfave/cli/v2"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	ffi "github.com/filecoin-project/filecoin-ffi"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	actorstypes "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/go-state-types/builtin"
	market8 "github.com/filecoin-project/go-state-types/builtin/v8/market"
	adt8 "github.com/filecoin-project/go-state-types/builtin/v8/util/adt"
	market9 "github.com/filecoin-project/go-state-types/builtin/v9/market"
	miner9 "github.com/filecoin-project/go-state-types/builtin/v9/miner"
	adt9 "github.com/filecoin-project/go-state-types/builtin/v9/util/adt"
	verifreg9 "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/blockstore"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/actors/adt"
	lbuiltin "github.com/filecoin-project/lotus/chain/actors/builtin"
	"github.com/filecoin-project/lotus/chain/actors/builtin/datacap"
	"github.com/filecoin-project/lotus/chain/actors/builtin/market"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/actors/builtin/verifreg"
	"github.com/filecoin-project/lotus/chain/consensus/filcns"
	"github.com/filecoin-project/lotus/chain/state"
	"github.com/filecoin-project/lotus/chain/stmgr"
	"github.com/filecoin-project/lotus/chain/store"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/vm"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/node/repo"
	"github.com/filecoin-project/lotus/storage/sealer/ffiwrapper"
)

type LRUMigrationCache struct {
	cache *lru.TwoQueueCache
}

func NewLRUMigrationCache(size int) *LRUMigrationCache {
	cache, err := lru.New2Q(size)
	if err != nil {
		panic(err)
	}

	return &LRUMigrationCache{
		cache: cache,
	}
}

func (mc *LRUMigrationCache) Write(key string, value cid.Cid) error {
	mc.cache.Add(key, value)
	return nil
}
func (mc *LRUMigrationCache) Read(key string) (bool, cid.Cid, error) {
	val, ok := mc.cache.Get(key)
	if ok {
		return true, val.(cid.Cid), nil
	}
	return false, cid.Undef, nil
}
func (mc *LRUMigrationCache) Load(key string, loadFunc func() (cid.Cid, error)) (cid.Cid, error) {
	val, ok := mc.cache.Get(key)
	if ok {
		return val.(cid.Cid), nil
	}

	c, err := loadFunc()
	if err != nil {
		return cid.Undef, err
	}

	mc.cache.Add(key, c)

	return c, nil
}

var migrationsCmd = &cli.Command{
	Name:        "migrate-nv17",
	Description: "Run the nv17 migration",
	ArgsUsage:   "[block to look back from]",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "repo",
			Value: "~/.lotus",
		},
	},
	Action: func(cctx *cli.Context) error {
		ctx := context.TODO()

		if cctx.NArg() != 1 {
			return lcli.IncorrectNumArgs(cctx)
		}

		blkCid, err := cid.Decode(cctx.Args().First())
		if err != nil {
			return fmt.Errorf("failed to parse input: %w", err)
		}

		fsrepo, err := repo.NewFS(cctx.String("repo"))
		if err != nil {
			return err
		}

		lkrepo, err := fsrepo.Lock(repo.FullNode)
		if err != nil {
			return err
		}

		defer lkrepo.Close() //nolint:errcheck

		bs, err := lkrepo.Blockstore(ctx, repo.UniversalBlockstore)
		if err != nil {
			return fmt.Errorf("failed to open blockstore: %w", err)
		}

		defer func() {
			if c, ok := bs.(io.Closer); ok {
				if err := c.Close(); err != nil {
					log.Warnf("failed to close blockstore: %s", err)
				}
			}
		}()

		mds, err := lkrepo.Datastore(context.Background(), "/metadata")
		if err != nil {
			return err
		}

		cs := store.NewChainStore(bs, bs, mds, filcns.Weight, nil)
		defer cs.Close() //nolint:errcheck

		sm, err := stmgr.NewStateManager(cs, filcns.NewTipSetExecutor(), vm.Syscalls(ffiwrapper.ProofVerifier), filcns.DefaultUpgradeSchedule(), nil)
		if err != nil {
			return err
		}

		//cache := nv15.NewMemMigrationCache()

		blk, err := cs.GetBlock(ctx, blkCid)
		if err != nil {
			return err
		}

		migrationTs, err := cs.LoadTipSet(ctx, types.NewTipSetKey(blk.Parents...))
		if err != nil {
			return err
		}

		startTime := time.Now()
		/*ts1, err := cs.GetTipsetByHeight(ctx, blk.Height-180, migrationTs, false)
		if err != nil {
			return err
		}

		err = filcns.PreUpgradeActorsV9(ctx, sm, cache, ts1.ParentState(), ts1.Height()-1, ts1)
		if err != nil {
			return err
		}

		fmt.Println("completed round 1, took ", time.Since(startTime))
		startTime = time.Now()

		newCid1, err := filcns.UpgradeActorsV9(ctx, sm, cache, nil, blk.ParentStateRoot, blk.Height-1, migrationTs)
		if err != nil {
			return err
		}
		fmt.Println("completed round actual (with cache), took ", time.Since(startTime))

		fmt.Println("new cid", newCid1)
		*/
		//newCid2, err := filcns.UpgradeActorsV9(ctx, sm, NewLRUMigrationCache(1000), nil, blk.ParentStateRoot, blk.Height-1, migrationTs)
		newCid2, err := cid.Decode("bafy2bzaceashrj5bldj2osjkq5h6lcuzhgs77zcdixapd2gf2242httvdp7zc")
		if err != nil {
			return err
		}
		fmt.Println("completed round actual (without cache), took ", time.Since(startTime))

		fmt.Println("new cid", newCid2)
		msg := types.Message{}
		err = json.Unmarshal([]byte(`{
  "Version": 0,
  "To": "f05",
  "From": "f3sf6gf4lcb27varcyq6jqryvs75hhbsz5gzqlsgykpftf5ce4php4hiwrkogmfz2ussy7tcauayhyiid5bxeq",
  "Nonce": 222,
  "Value": "0",
  "GasLimit": 10000000000,
  "GasFeeCap": "630212788",
  "GasPremium": "54680130",
  "Method": 4,
  "Params": "gYiCi9gqWCgAAYHiA5IgIPHf8BolgPZ0Q3IqEogBPB7vO8MEBGsPC/SchvKPyJgnGwAAAAQAAAAA9VUB6J8v4dq/CulV/OueVB1bByxtQopEAOGzdXg1dUFZSGlBNUlnSVBIZjhCb2xnUFowUTNJcUVvZ0JQQjd2TzhNRUJHc1BDX1NjaHZLUHlKZ24aACLsbhoAOeI4QEgADRvtiYj6J0BYQgE1LyU/ykhHZmfbJcbXGRTNaNardfLqBpoDyU92BlOaj2jbFepcQYbx1XtBIeL/QyyXNTNlRDt5K3as/s8Szq8KAIKL2CpYKAABgeIDkiAgeexChsPgFZRP++vhdLiIMa68AhO7rpdW0NB+8KjTaz4aQAAAAPVVAeifL+HavwrpVfzrnlQdWwcsbUKKRADhs3V4NXVBWUhpQTVJZ0lIbnNRb2JENEJXVVRfdnI0WFM0aURHdXZBSVR1NjZYVnREUWZ2Q28wMnMtGgAi7EwaADniOEBHANG9L7Sf4EBYQgEVrYDDUkmvNRq1qy4sf/VPFzbuOXQWMVM64NEV/slMTnDlI3ty/hko0wT/B+s0HJqt3OFfV2c4l6576KYBk7EpAYKL2CpYKAABgeIDkiAg6A/sRfnfVk/V4rgMxppzFK2M4DbkeVNj1/qFSDqgkSAaACAAAPVVAeifL+HavwrpVfzrnlQdWwcsbUKKRADhs3V4NXVBWUhpQTVJZ0lPZ1A3RVg1MzFaUDFlSzRETWFhY3hTdGpPQTI1SGxUWTlmNmhVZzZvSkVnGgAi7EwaADniOEBGAGjel9pOQFhCAclIovU5tCXQvAiFQdLrHK2RHF5G6LIzb3rQJC7ottPdbWmHAO8N8gehJIeC4oNNHUxLmzWzOz6lm3HOOEa/8/kAgovYKlgoAAGB4gOSICDZJhIBLB642H64Wz4GLbqutrxHfe23jXZF5jDRJFU9GBsAAAABAAAAAPVVAeifL+HavwrpVfzrnlQdWwcsbUKKRADhs3V4NXVBWUhpQTVJZ0lOa21FZ0VzSHJqWWZyaGJQZ1l0dXE2MnZFZDk3YmVOZGtYbU1ORWtWVDBZGgAi7G8aADniOEBIAANG+3LBjKlAWEIBhrCQ834Rc2SUHWaRD+wVO5nQXzn/BLLAI8QXahZE9nRMMVDNdsEVRFSQCko91Pwd2OJaXqZoNRNqO4CBYQaC9QGCi9gqWCgAAYHiA5IgIKNRCtxvTtqU35jzNSrvNAPmX0jltV6Uhl/yqxSWFRcxGwAAAAQAAAAA9VUB6J8v4dq/CulV/OueVB1bByxtQopEAOGzdXg1dUFZSGlBNUlnSUtOUkN0eHZUdHFVMzVqek5TcnZOQVBtWDBqbHRWNlVobF95cXhTV0ZSY3gaACLsTBoAOeI4QEgADRvS+0n+DEBYQgFJ4GzibgpTB2sS6DXXpSHiNOH8M75Er9Xvv5/m9ostmysmVBFgchX9wf90ALsd2ex6ZERA+7DZVdUQAKf0Vh1AAIKL2CpYKAABgeIDkiAg5fWWcm3lux+TP6toRckxkt5yaYm87KNP7fuRn9SHbDQbAAAABAAAAAD1VQHony/h2r8K6VX8655UHVsHLG1CikQA4bN1eDV1QVlIaUE1SWdJT1gxbG5KdDVic2Zrei1yYUVYSk1aTGVjbW1Kdk95alQtMzdrWl9VaDJ3MBoAIuxMGgA54jhASAANG9L7Sf4MQFhCAWzHaLiigFah1//TyqyMVVB2FsRveoerTCFdgdndZ0K1JzN+reU0XBND9ZhHixeQIbtv+/AtyD9Dv/mYfFfDv2QAgovYKlgoAAGB4gOSICBddwDd+++csTbTAg1zf+Xty0D5yKEVRp34+El//fPCJxsAAAAEAAAAAPVVAeifL+HavwrpVfzrnlQdWwcsbUKKRADhs3V4NXVBWUhpQTVJZ0lGMTNBTjM3NzV5eE50TUNEWE5fNWUzTFFQbklvUlZHbmZqNFNYXzk4OEluGgAi7EwaADniOEBIAA0b0vtJ/gxAWEIBt1+RV8mGLglLneAUpm8Ep2TP/scBcWvt7x55dC2Q3yNSqX/CsD8tXz+sTepds+srt10aGCVvhCNlHnJjJ/Yb2wGCi9gqWCgAAYHiA5IgIGDG1cbxbnIYRPB5y2Tc8wvyH4ay3BRrxtYW4wkR29kbGwAAAAIAAAAA9VUB6J8v4dq/CulV/OueVB1bByxtQopEAOGzdXg1dUFZSGlBNUlnSUdERzFjYnhibklZUlBCNXkyVGM4d3Z5SDRheTNCUnJ4dFlXNHdrUjI5a2IaACLsbhoAOeI4QEgABo325YMZVEBYQgEGJm/v9ZjNmm5g8GabsTMc093D6EjQdzYzLcYw8Vd2TxSraAl60HKsmMfiEOCvP9C6pIGR8BhWNyrprbadpWfCAA=="
}`), &msg)
		if err != nil {
			return err
		}

		res, err := sm.CallAtStateAndVersion(ctx, &msg, migrationTs, newCid2, network.Version17)
		fmt.Printf("%+v, %+v\n", res, err)
		if err != nil {
			fmt.Printf("GasUsed: %v\n", res.MsgRct)
		}
		//	printInternalExecutions(0, []types.ExecutionTrace{res.ExecutionTrace})

		/*
			if newCid1 != newCid2 {
				return xerrors.Errorf("got different results with and without the cache: %s, %s", newCid1,
					newCid2)
			}

			err = checkStateInvariants(ctx, blk.ParentStateRoot, newCid1, bs)
			if err != nil {
				return err
			}
		*/

		return nil
	},
}

func printInternalExecutions(depth int, trace []types.ExecutionTrace) {
	if depth == 0 {
		fmt.Println("depth\tFrom\tTo\tValue\tMethod\tGasUsed\tParams\tExitCode\tReturn")
	}
	for _, im := range trace {
		fmt.Printf("%d\t%s\t%s\t%s\t%d\t%d\t%x\t%d\t%x\n", depth, im.Msg.From, im.Msg.To, im.Msg.Value, im.Msg.Method, im.MsgRct.GasUsed, im.Msg.Params, im.MsgRct.ExitCode, im.MsgRct.Return)
		printInternalExecutions(depth+1, im.Subcalls)
	}
}

func checkStateInvariants(ctx context.Context, v8StateRoot cid.Cid, v9StateRoot cid.Cid, bs blockstore.Blockstore) error {
	actorStore := store.ActorStore(ctx, blockstore.NewTieredBstore(bs, blockstore.NewMemorySync()))

	stateTreeV8, err := state.LoadStateTree(actorStore, v8StateRoot)
	if err != nil {
		return err
	}

	stateTreeV9, err := state.LoadStateTree(actorStore, v9StateRoot)
	if err != nil {
		return err
	}

	err = checkDatacaps(stateTreeV8, stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	err = checkPendingVerifiedDeals(stateTreeV8, stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	err = checkAllMinersUnsealedCID(stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	return nil
}

func checkDatacaps(stateTreeV8 *state.StateTree, stateTreeV9 *state.StateTree, actorStore adt.Store) error {
	verifregDatacaps, err := getVerifreg8Datacaps(stateTreeV8, actorStore)
	if err != nil {
		return err
	}

	newDatacaps, err := getDatacap9Datacaps(stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	if len(verifregDatacaps) != len(newDatacaps) {
		return xerrors.Errorf("size of datacap maps do not match. verifreg: %d, datacap: %d", len(verifregDatacaps), len(newDatacaps))
	}

	for addr, oldDcap := range verifregDatacaps {
		dcap, ok := newDatacaps[addr]
		if !ok {
			return xerrors.Errorf("datacap for address: %s not found in datacap state", addr)
		}
		if !dcap.Equals(oldDcap) {
			return xerrors.Errorf("datacap for address: %s do not match. verifreg: %d, datacap: %d", addr, oldDcap, dcap)
		}
	}

	return nil
}

func getVerifreg8Datacaps(stateTreeV8 *state.StateTree, actorStore adt.Store) (map[address.Address]abi.StoragePower, error) {
	verifregStateV8, err := getVerifregActorV8(stateTreeV8, actorStore)
	if err != nil {
		return nil, xerrors.Errorf("failed to get verifreg actor state: %w", err)
	}

	var verifregDatacaps = make(map[address.Address]abi.StoragePower)
	err = verifregStateV8.ForEachClient(func(addr address.Address, dcap abi.StoragePower) error {
		verifregDatacaps[addr] = dcap
		return nil
	})
	if err != nil {
		return nil, err
	}

	return verifregDatacaps, nil
}

func getDatacap9Datacaps(stateTreeV9 *state.StateTree, actorStore adt.Store) (map[address.Address]abi.StoragePower, error) {
	datacapStateV9, err := getDatacapActorV9(stateTreeV9, actorStore)
	if err != nil {
		return nil, xerrors.Errorf("failed to get datacap actor state: %w", err)
	}

	var datacaps = make(map[address.Address]abi.StoragePower)
	err = datacapStateV9.ForEachClient(func(addr address.Address, dcap abi.StoragePower) error {
		datacaps[addr] = dcap
		return nil
	})
	if err != nil {
		return nil, err
	}

	return datacaps, nil
}

func checkPendingVerifiedDeals(stateTreeV8 *state.StateTree, stateTreeV9 *state.StateTree, actorStore adt.Store) error {
	marketActorV9, err := getMarketActorV9(stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	verifregActorV9, err := getVerifregActorV9(stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	verifregStateV9, err := getVerifregStateV9(stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	marketStateV8, err := getMarketStateV8(stateTreeV8, actorStore)
	if err != nil {
		return err
	}

	marketStateV9, err := getMarketStateV9(stateTreeV9, actorStore)
	if err != nil {
		return err
	}

	pendingProposalsV8, err := adt8.AsSet(actorStore, marketStateV8.PendingProposals, builtin.DefaultHamtBitwidth)
	if err != nil {
		return xerrors.Errorf("failed to load pending proposals: %w", err)
	}

	dealProposalsV8, err := market8.AsDealProposalArray(actorStore, marketStateV8.Proposals)
	if err != nil {
		return xerrors.Errorf("failed to get proposals: %w", err)
	}

	var numPendingVerifiedDeals = 0
	var proposal market8.DealProposal
	err = dealProposalsV8.ForEach(&proposal, func(dealID int64) error {
		// If not verified, do nothing
		if !proposal.VerifiedDeal {
			return nil
		}

		pcid, err := proposal.Cid()
		if err != nil {
			return err
		}

		isPending, err := pendingProposalsV8.Has(abi.CidKey(pcid))
		if err != nil {
			return xerrors.Errorf("failed to check pending: %w", err)
		}

		// Nothing to do for not-pending deals
		if !isPending {
			return nil
		}

		numPendingVerifiedDeals++
		// Checks if allocation ID is in market map
		allocationId, err := marketActorV9.GetAllocationIdForPendingDeal(abi.DealID(dealID))
		if err != nil {
			return err
		}

		// Checks if allocation is in verifreg
		allocation, found, err := verifregActorV9.GetAllocation(proposal.Client, allocationId)
		if !found {
			return xerrors.Errorf("allocation %d not found for address %s", allocationId, proposal.Client)
		}
		if err != nil {
			return err
		}

		err = compareProposalToAllocation(proposal, *allocation)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("Pending Verified deals in market v8: %d\n", numPendingVerifiedDeals)

	numAllocationIds, err := countAllocationIds(actorStore, marketStateV9)
	if err != nil {
		return err
	}
	fmt.Printf("Allocation IDs in market v9: %d\n", numAllocationIds)

	if numAllocationIds != numPendingVerifiedDeals {
		return xerrors.Errorf("number of allocation IDsf: %d did not match the number of pending verified deals: %d", numAllocationIds, numPendingVerifiedDeals)
	}

	numAllocations, err := countAllocations(verifregStateV9, actorStore)
	if err != nil {
		return err
	}
	fmt.Printf("Allocations in verifreg v9: %d\n", numAllocations)

	if numAllocations != numPendingVerifiedDeals {
		return xerrors.Errorf("number of allocations: %d did not match the number of pending verified deals: %d", numAllocations, numPendingVerifiedDeals)
	}

	nextAllocationId := int(verifregStateV9.NextAllocationId)
	fmt.Printf("Next Allocation ID: %d\n", nextAllocationId)

	if numAllocations+1 != nextAllocationId {
		return xerrors.Errorf("number of allocations + 1: %d did not match the next allocation ID: %d", numAllocations+1, nextAllocationId)
	}

	return nil
}

func compareProposalToAllocation(prop market8.DealProposal, alloc verifreg9.Allocation) error {
	if prop.PieceCID != alloc.Data {
		return xerrors.Errorf("piece cid mismatch between proposal and allocation: %s, %s", prop.PieceCID, alloc.Data)
	}

	proposalClientID, err := address.IDFromAddress(prop.Client)
	if err != nil {
		return xerrors.Errorf("couldnt get ID from address")
	}
	if proposalClientID != uint64(alloc.Client) {
		return xerrors.Errorf("client id mismatch between proposal and allocation: %s, %s", proposalClientID, alloc.Client)
	}

	proposalProviderID, err := address.IDFromAddress(prop.Provider)
	if err != nil {
		return xerrors.Errorf("couldnt get ID from address")
	}
	if proposalProviderID != uint64(alloc.Provider) {
		return xerrors.Errorf("provider id mismatch between proposal and allocation: %s, %s", proposalProviderID, alloc.Provider)
	}

	if prop.PieceSize != alloc.Size {
		return xerrors.Errorf("piece size mismatch between proposal and allocation: %s, %s", prop.PieceSize, alloc.Size)
	}

	if alloc.TermMax != 540*builtin.EpochsInDay {
		return xerrors.Errorf("allocation term should be 540 days. Got %d epochs", alloc.TermMax)
	}

	if prop.EndEpoch-prop.StartEpoch != alloc.TermMin {
		return xerrors.Errorf("allocation term mismatch between proposal and allocation: %d, %d", prop.EndEpoch-prop.StartEpoch, alloc.TermMin)
	}

	return nil
}

func getMarketStateV8(stateTreeV8 *state.StateTree, actorStore adt.Store) (market8.State, error) {
	marketV8, err := stateTreeV8.GetActor(market.Address)
	if err != nil {
		return market8.State{}, err
	}

	var marketStateV8 market8.State
	if err = actorStore.Get(actorStore.Context(), marketV8.Head, &marketStateV8); err != nil {
		return market8.State{}, xerrors.Errorf("failed to get market actor state: %w", err)
	}

	return marketStateV8, nil
}

func getMarketStateV9(stateTreeV9 *state.StateTree, actorStore adt.Store) (market9.State, error) {
	marketV9, err := stateTreeV9.GetActor(market.Address)
	if err != nil {
		return market9.State{}, err
	}

	var marketStateV9 market9.State
	if err = actorStore.Get(actorStore.Context(), marketV9.Head, &marketStateV9); err != nil {
		return market9.State{}, xerrors.Errorf("failed to get market actor state: %w", err)
	}

	return marketStateV9, nil
}

func getMarketActorV9(stateTreeV9 *state.StateTree, actorStore adt.Store) (market.State, error) {
	marketV9, err := stateTreeV9.GetActor(market.Address)
	if err != nil {
		return nil, err
	}

	return market.Load(actorStore, marketV9)
}

func getVerifregActorV8(stateTreeV8 *state.StateTree, actorStore adt.Store) (verifreg.State, error) {
	verifregV8, err := stateTreeV8.GetActor(verifreg.Address)
	if err != nil {
		return nil, err
	}

	return verifreg.Load(actorStore, verifregV8)
}

func getVerifregActorV9(stateTreeV9 *state.StateTree, actorStore adt.Store) (verifreg.State, error) {
	verifregV9, err := stateTreeV9.GetActor(verifreg.Address)
	if err != nil {
		return nil, err
	}

	return verifreg.Load(actorStore, verifregV9)
}

func getVerifregStateV9(stateTreeV9 *state.StateTree, actorStore adt.Store) (verifreg9.State, error) {
	verifregV9, err := stateTreeV9.GetActor(verifreg.Address)
	if err != nil {
		return verifreg9.State{}, err
	}

	var verifregStateV9 verifreg9.State
	if err = actorStore.Get(actorStore.Context(), verifregV9.Head, &verifregStateV9); err != nil {
		return verifreg9.State{}, xerrors.Errorf("failed to get verifreg actor state: %w", err)
	}

	return verifregStateV9, nil
}

func getDatacapActorV9(stateTreeV9 *state.StateTree, actorStore adt.Store) (datacap.State, error) {
	datacapV9, err := stateTreeV9.GetActor(datacap.Address)
	if err != nil {
		return nil, err
	}

	return datacap.Load(actorStore, datacapV9)
}

func checkAllMinersUnsealedCID(stateTreeV9 *state.StateTree, store adt.Store) error {
	return stateTreeV9.ForEach(func(addr address.Address, actor *types.Actor) error {
		if !lbuiltin.IsStorageMinerActor(actor.Code) {
			return nil // no need to check
		}

		err := checkMinerUnsealedCID(actor, stateTreeV9, store)
		if err != nil {
			fmt.Println("failure for miner ", addr)
			return err
		}
		return nil
	})
}

func checkMinerUnsealedCID(act *types.Actor, stateTreeV9 *state.StateTree, store adt.Store) error {
	minerCodeCid, found := actors.GetActorCodeID(actorstypes.Version9, actors.MinerKey)
	if !found {
		return xerrors.Errorf("could not find code cid for miner actor")
	}
	if minerCodeCid != act.Code {
		return nil // no need to check
	}

	marketActorV9, err := getMarketActorV9(stateTreeV9, store)
	if err != nil {
		return err
	}
	dealProposals, err := marketActorV9.Proposals()
	if err != nil {
		return err
	}

	m, err := miner.Load(store, act)
	if err != nil {
		return err
	}

	err = m.ForEachPrecommittedSector(func(info miner9.SectorPreCommitOnChainInfo) error {
		dealIDs := info.Info.DealIDs

		if len(dealIDs) == 0 {
			return nil // Nothing to check here
		}

		pieceCids := make([]abi.PieceInfo, len(dealIDs))
		for i, dealId := range dealIDs {
			dealProposal, found, err := dealProposals.Get(dealId)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}

			pieceCids[i] = abi.PieceInfo{
				Size:     dealProposal.PieceSize,
				PieceCID: dealProposal.PieceCID,
			}
		}

		if len(pieceCids) == 0 {
			return nil
		}

		if info.Info.UnsealedCid == nil {
			return xerrors.Errorf("nil unsealed CID for sector with deals")
		}

		pieceCID, err := ffi.GenerateUnsealedCID(abi.RegisteredSealProof_StackedDrg64GiBV1_1, pieceCids)
		if err != nil {
			return err
		}

		if pieceCID != *info.Info.UnsealedCid {
			return xerrors.Errorf("calculated piece CID %s did not match unsealed CID in precommitted sector info: %s", pieceCID, *info.Info.UnsealedCid)
		}

		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func countAllocations(verifregState verifreg9.State, store adt.Store) (int, error) {
	var count = 0

	actorToHamtMap, err := adt9.AsMap(store, verifregState.Allocations, builtin.DefaultHamtBitwidth)
	if err != nil {
		return 0, xerrors.Errorf("couldn't get outer map: %x", err)
	}

	var innerHamtCid cbg.CborCid
	err = actorToHamtMap.ForEach(&innerHamtCid, func(key string) error {
		innerMap, err := adt9.AsMap(store, cid.Cid(innerHamtCid), builtin.DefaultHamtBitwidth)
		if err != nil {
			return xerrors.Errorf("couldn't get outer map: %x", err)
		}

		var allocation verifreg9.Allocation
		err = innerMap.ForEach(&allocation, func(key string) error {
			count++
			return nil
		})
		if err != nil {
			return xerrors.Errorf("couldn't iterate over inner map: %x", err)
		}

		return nil
	})
	if err != nil {
		return 0, xerrors.Errorf("couldn't iterate over outer map: %x", err)
	}

	return count, nil
}

func countAllocationIds(store adt.Store, marketState market9.State) (int, error) {
	allocationIds, err := adt9.AsMap(store, marketState.PendingDealAllocationIds, builtin.DefaultHamtBitwidth)
	if err != nil {
		return 0, err
	}

	var numAllocationIds int
	_ = allocationIds.ForEach(nil, func(key string) error {
		numAllocationIds++
		return nil
	})

	return numAllocationIds, nil
}
