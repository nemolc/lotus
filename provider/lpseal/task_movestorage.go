package lpseal

import (
	"context"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/lotus/lib/harmony/harmonydb"
	"github.com/filecoin-project/lotus/lib/harmony/harmonytask"
	"github.com/filecoin-project/lotus/lib/harmony/resources"
	"github.com/filecoin-project/lotus/provider/lpffi"
	"github.com/filecoin-project/lotus/storage/sealer/storiface"
)

type MoveStorageTask struct {
	sp *SealPoller
	sc *lpffi.SealCalls
	db *harmonydb.DB

	max int
}

func NewMoveStorageTask(sp *SealPoller, sc *lpffi.SealCalls, db *harmonydb.DB, max int) *MoveStorageTask {
	return &MoveStorageTask{
		max: max,
		sp:  sp,
		sc:  sc,
		db:  db,
	}
}

func (m *MoveStorageTask) Do(taskID harmonytask.TaskID, data harmonytask.AcceptData, stillOwned func() bool) (done bool, err error) {
	var tasks []struct {
		SpID         int64 `db:"sp_id"`
		SectorNumber int64 `db:"sector_number"`
		RegSealProof int64 `db:"reg_seal_proof"`
	}

	ctx := context.Background()

	err = m.db.Select(ctx, &tasks, `
		SELECT sp_id, sector_number, reg_seal_proof FROM sectors_sdr_pipeline WHERE task_id_move_storage = $1`, taskID)
	if err != nil {
		return false, xerrors.Errorf("getting task: %w", err)
	}
	if len(tasks) != 1 {
		return false, xerrors.Errorf("expected one task")
	}
	task := tasks[0]

	sector := storiface.SectorRef{
		ID: abi.SectorID{
			Miner:  abi.ActorID(task.SpID),
			Number: abi.SectorNumber(task.SectorNumber),
		},
		ProofType: abi.RegisteredSealProof(task.RegSealProof),
	}

	err = m.sc.MoveStorage(ctx, sector)
	if err != nil {
		return false, xerrors.Errorf("moving storage: %w", err)
	}

	_, err = m.db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_move_storage = true WHERE task_id_move_storage = $1`, taskID)
	if err != nil {
		return false, xerrors.Errorf("updating task: %w", err)
	}

	return true, nil
}

func (m *MoveStorageTask) CanAccept(ids []harmonytask.TaskID, engine *harmonytask.TaskEngine) (*harmonytask.TaskID, harmonytask.AcceptData, error) {

	ctx := context.Background()
	/*

			var tasks []struct {
				TaskID       harmonytask.TaskID `db:"task_id_finalize"`
				SpID         int64              `db:"sp_id"`
				SectorNumber int64              `db:"sector_number"`
				StorageID    string             `db:"storage_id"`
			}

			indIDs := make([]int64, len(ids))
			for i, id := range ids {
				indIDs[i] = int64(id)
			}
			err := m.db.Select(ctx, &tasks, `
				select p.task_id_move_storage, p.sp_id, p.sector_number, l.storage_id from sectors_sdr_pipeline p
				    inner join sector_location l on p.sp_id=l.miner_id and p.sector_number=l.sector_num
				    where task_id_move_storage in ($1) and l.sector_filetype=4`, indIDs)
			if err != nil {
				return nil, xerrors.Errorf("getting tasks: %w", err)
			}

			ls, err := m.sc.LocalStorage(ctx)
			if err != nil {
				return nil, xerrors.Errorf("getting local storage: %w", err)
			}

			acceptables := map[harmonytask.TaskID]bool{}

			for _, t := range ids {
				acceptables[t] = true
			}

			for _, t := range tasks {

			}

			todo some smarts
		     * yield a schedule cycle/s if we have moves already in progress
	*/

	////
	ls, err := m.sc.LocalStorage(ctx)
	if err != nil {
		return nil, nil, xerrors.Errorf("getting local storage: %w", err)
	}
	var haveStorage bool
	for _, l := range ls {
		if l.CanStore {
			haveStorage = true
			break
		}
	}

	if !haveStorage {
		return nil, nil, nil
	}

	id := ids[0]
	return &id, nil, nil
}

func (m *MoveStorageTask) TypeDetails() harmonytask.TaskTypeDetails {
	return harmonytask.TaskTypeDetails{
		Max:  m.max,
		Name: "MoveStorage",
		Cost: resources.Resources{
			Cpu: 1,
			Gpu: 0,
			Ram: 128 << 20,
		},
		MaxFailures: 10,
	}
}

func (m *MoveStorageTask) Adder(taskFunc harmonytask.AddTaskFunc) {
	m.sp.pollers[pollerMoveStorage].Set(taskFunc)
}

var _ harmonytask.TaskInterface = &MoveStorageTask{}
