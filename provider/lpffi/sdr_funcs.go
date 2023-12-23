package lpffi

import (
	"context"
	"encoding/json"
	"fmt"
	ffi "github.com/filecoin-project/filecoin-ffi"
	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/provider/lpproof"
	"github.com/filecoin-project/lotus/storage/paths"
	"github.com/filecoin-project/lotus/storage/pipeline/lib/nullreader"
	"github.com/filecoin-project/lotus/storage/sealer/proofpaths"
	"github.com/filecoin-project/lotus/storage/sealer/storiface"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"
	"io"
	"os"
	"path/filepath"
)

var log = logging.Logger("lpffi")

/*
type ExternPrecommit2 func(ctx context.Context, sector storiface.SectorRef, cache, sealed string, pc1out storiface.PreCommit1Out) (sealedCID cid.Cid, unsealedCID cid.Cid, err error)

	type ExternalSealer struct {
		PreCommit2 ExternPrecommit2
	}
*/
type SealCalls struct {
	sectors *storageProvider

	/*// externCalls cointain overrides for calling alternative sealing logic
	externCalls ExternalSealer*/
}

func NewSealCalls(st paths.Store, ls *paths.Local, si paths.SectorIndex) *SealCalls {
	return &SealCalls{
		sectors: &storageProvider{
			storage:    st,
			localStore: ls,
			sindex:     si,
		},
	}
}

type storageProvider struct {
	storage    paths.Store
	localStore *paths.Local
	sindex     paths.SectorIndex
}

func (l *storageProvider) AcquireSector(ctx context.Context, sector storiface.SectorRef, existing storiface.SectorFileType, allocate storiface.SectorFileType, sealing storiface.PathType) (storiface.SectorPaths, func(), error) {
	paths, storageIDs, err := l.storage.AcquireSector(ctx, sector, existing, allocate, sealing, storiface.AcquireMove)
	if err != nil {
		return storiface.SectorPaths{}, nil, err
	}

	releaseStorage, err := l.localStore.Reserve(ctx, sector, allocate, storageIDs, storiface.FSOverheadSeal)
	if err != nil {
		return storiface.SectorPaths{}, nil, xerrors.Errorf("reserving storage space: %w", err)
	}

	log.Debugf("acquired sector %d (e:%d; a:%d): %v", sector, existing, allocate, paths)

	return paths, func() {
		releaseStorage()

		for _, fileType := range storiface.PathTypes {
			if fileType&allocate == 0 {
				continue
			}

			sid := storiface.PathByType(storageIDs, fileType)
			if err := l.sindex.StorageDeclareSector(ctx, storiface.ID(sid), sector.ID, fileType, true); err != nil {
				log.Errorf("declare sector error: %+v", err)
			}
		}
	}, nil
}

func (sb *SealCalls) GenerateSDR(ctx context.Context, sector storiface.SectorRef, ticket abi.SealRandomness, commKcid cid.Cid) error {
	paths, releaseSector, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTNone, storiface.FTCache, storiface.PathSealing)
	if err != nil {
		return xerrors.Errorf("acquiring sector paths: %w", err)
	}
	defer releaseSector()

	// prepare SDR params
	commp, err := commcid.CIDToDataCommitmentV1(commKcid)
	if err != nil {
		return xerrors.Errorf("computing commK: %w", err)
	}

	replicaID, err := sector.ProofType.ReplicaId(sector.ID.Miner, sector.ID.Number, ticket, commp)
	if err != nil {
		return xerrors.Errorf("computing replica id: %w", err)
	}

	// generate new sector key
	err = ffi.GenerateSDR(
		sector.ProofType,
		paths.Cache,
		replicaID,
	)
	if err != nil {
		return xerrors.Errorf("generating SDR %d (%s): %w", sector.ID.Number, paths.Unsealed, err)
	}

	return nil
}

func (sb *SealCalls) TreeD(ctx context.Context, sector storiface.SectorRef, size abi.PaddedPieceSize, data io.Reader) (cid.Cid, error) {
	maybeUns := storiface.FTNone
	// todo sectors with data

	paths, releaseSector, err := sb.sectors.AcquireSector(ctx, sector, maybeUns, storiface.FTCache, storiface.PathSealing)
	if err != nil {
		return cid.Undef, xerrors.Errorf("acquiring sector paths: %w", err)
	}
	defer releaseSector()

	log.Errorw("oest.idos.hbisor.bpisro.pisro.bpisro.bxsrobpyxsrbpoyxsrgbopyx treed", "paths", paths.Cache)

	return lpproof.BuildTreeD(data, filepath.Join(paths.Cache, proofpaths.TreeDName), size)
}

func (sb *SealCalls) TreeRC(ctx context.Context, sector storiface.SectorRef, unsealed cid.Cid) (cid.Cid, cid.Cid, error) {
	p1o, err := sb.makePhase1Out(unsealed, sector.ProofType)
	if err != nil {
		return cid.Undef, cid.Undef, xerrors.Errorf("make phase1 output: %w", err)
	}

	log.Errorw("phase1 output", "p1o", p1o)

	paths, releaseSector, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTCache, storiface.FTSealed, storiface.PathSealing)
	if err != nil {
		return cid.Undef, cid.Undef, xerrors.Errorf("acquiring sector paths: %w", err)
	}
	defer releaseSector()

	{
		// create sector-sized file at paths.Sealed; PC2 transforms it into a sealed sector in-place
		ssize, err := sector.ProofType.SectorSize()
		if err != nil {
			return cid.Undef, cid.Undef, xerrors.Errorf("getting sector size: %w", err)
		}

		// paths.Sealed is a string filepath
		f, err := os.Create(paths.Sealed)
		if err != nil {
			return cid.Undef, cid.Undef, xerrors.Errorf("creating sealed sector file: %w", err)
		}
		if err := f.Truncate(int64(ssize)); err != nil {
			return cid.Undef, cid.Undef, xerrors.Errorf("truncating sealed sector file: %w", err)
		}

		if os.Getenv("SEAL_WRITE_UNSEALED") == "1" {
			// expliticly write zeros to unsealed sector
			_, err := io.CopyN(f, nullreader.NewNullReader(abi.UnpaddedPieceSize(ssize)), int64(ssize))
			if err != nil {
				return cid.Undef, cid.Undef, xerrors.Errorf("writing zeros to sealed sector file: %w", err)
			}
		}
	}

	return ffi.SealPreCommitPhase2(p1o, paths.Cache, paths.Sealed)
}

func (sb *SealCalls) makePhase1Out(unsCid cid.Cid, spt abi.RegisteredSealProof) ([]byte, error) {
	commd, err := commcid.CIDToDataCommitmentV1(unsCid)
	if err != nil {
		return nil, xerrors.Errorf("make uns cid: %w", err)
	}

	type Config struct {
		ID            string `json:"id"`
		Path          string `json:"path"`
		RowsToDiscard int    `json:"rows_to_discard"`
		Size          int    `json:"size"`
	}

	type Labels struct {
		H      *string  `json:"_h"` // proofs want this..
		Labels []Config `json:"labels"`
	}

	var phase1Output struct {
		CommD           [32]byte           `json:"comm_d"`
		Config          Config             `json:"config"` // TreeD
		Labels          map[string]*Labels `json:"labels"`
		RegisteredProof string             `json:"registered_proof"`
	}

	copy(phase1Output.CommD[:], commd)

	phase1Output.Config.ID = "tree-d"
	phase1Output.Config.Path = "/placeholder"
	phase1Output.Labels = map[string]*Labels{}

	switch spt {
	case abi.RegisteredSealProof_StackedDrg2KiBV1_1, abi.RegisteredSealProof_StackedDrg2KiBV1_1_Feat_SyntheticPoRep:
		phase1Output.Config.RowsToDiscard = 0
		phase1Output.Config.Size = 127
		phase1Output.Labels["StackedDrg2KiBV1"] = &Labels{}
		phase1Output.RegisteredProof = "StackedDrg2KiBV1_1"

		for i := 0; i < 2; i++ {
			phase1Output.Labels["StackedDrg2KiBV1"].Labels = append(phase1Output.Labels["StackedDrg2KiBV1"].Labels, Config{
				ID:            fmt.Sprintf("layer-%d", i+1),
				Path:          "/placeholder",
				RowsToDiscard: 0,
				Size:          64,
			})
		}
	default:
		panic("todo")
	}

	return json.Marshal(phase1Output)
}