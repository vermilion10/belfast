package game

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/protobuf"
)

// The research catch-up of the commander manual (CommanderManualLayer's tech
// page, ACTIVITY_TYPE_FRESH_TEC_CATCHUP): config_data[3] is a list of
// phases, [tasks, final task] each. data1 is the phase being done (0 none
// yet), data1_list the phases left before, data2 1 once phase 1 is redone
// (it is shown twice: phase 0 its first run, phase 1 the second). The
// CS_11202 cmds (CommanderManualMediator.ON_TRIGGER):
//   - 1, arg1 a phase: start it, once the phase being done had its final
//     task claimed; its tasks are given (SC_20003);
//   - 2: every task of the phase claimed, its final task is given;
//   - 3: redo phase 1, its tasks and final task given again.
// The client updates data1, data1_list and data2 itself on result 0. The
// phases' claimed tasks are sent among SC_20001's (techCatchupTask), as the
// page counts them.

const activityTypeTecCatchup = 71 // ActivityConst.ACTIVITY_TYPE_FRESH_TEC_CATCHUP

const (
	tecCatchupCmdStart  = 1
	tecCatchupCmdFinal  = 2
	tecCatchupCmdRedo1  = 3
	tecCatchupPhasesArg = 2 // config_data index of the phases
)

type tecCatchupPhase struct {
	tasks []uint32
	final uint32
}

// tecCatchupPhases reads a catch-up activity's phases, 1-based.
func tecCatchupPhases(config activityConfig) []tecCatchupPhase {
	var data []json.RawMessage
	if json.Unmarshal(config.ConfigData, &data) != nil || len(data) <= tecCatchupPhasesArg {
		return nil
	}
	var raw [][]json.RawMessage
	if json.Unmarshal(data[tecCatchupPhasesArg], &raw) != nil {
		return nil
	}
	phases := make([]tecCatchupPhase, 0, len(raw))
	for _, entry := range raw {
		var phase tecCatchupPhase
		if len(entry) >= 2 && json.Unmarshal(entry[0], &phase.tasks) == nil && json.Unmarshal(entry[1], &phase.final) == nil {
			phases = append(phases, phase)
		}
	}
	return phases
}

// techCatchupTask tells whether a task belongs to a catch-up activity's
// phases.
func techCatchupTask(id uint32) bool {
	for _, config := range activityConfigs {
		if config.Type != activityTypeTecCatchup {
			continue
		}
		for _, phase := range tecCatchupPhases(config) {
			if phase.final == id || slices.Contains(phase.tasks, id) {
				return true
			}
		}
	}
	return false
}

// tecCatchupOperation answers the catch-up's cmds. A refused one gets
// result 1 (activity_op_error).
func (game *Game) tecCatchupOperation(client *connection.Client, config activityConfig, req *protobuf.CS_11202) error {
	ctx := context.Background()
	now := uint32(time.Now().Unix())
	phases := tecCatchupPhases(config)
	err := game.tx(ctx, client, func(tx *Tx) error {
		state, err := activityState(ctx, tx.Queries, client.CommanderID, config)
		if err != nil {
			return err
		}
		give := func(ids []uint32) error {
			added, err := acceptTasks(ctx, tx.Queries, client.CommanderID, ids, now)
			if err != nil {
				return err
			}
			if added != nil && len(added.Info) > 0 {
				tx.push(added)
			}
			return nil
		}
		// the phase being done is over once its final task is claimed
		currentDone := func() (bool, error) {
			if state.Data1 == 0 || int(state.Data1) > len(phases) {
				return true, nil
			}
			return tx.tasksClaimed([]uint32{phases[state.Data1-1].final})
		}
		switch req.GetCmd() {
		case tecCatchupCmdStart:
			phase := req.GetArg1()
			if phase < 1 || int(phase) > len(phases) {
				return errRefused
			}
			if done, err := currentDone(); err != nil || !done {
				return orRefused(err)
			}
			if !slices.Contains(state.Data1List, state.Data1) {
				state.Data1List = append(state.Data1List, state.Data1)
			}
			state.Data1 = phase
			if err := give(phases[phase-1].tasks); err != nil {
				return err
			}
		case tecCatchupCmdFinal:
			if state.Data1 == 0 || int(state.Data1) > len(phases) {
				return errRefused
			}
			current := phases[state.Data1-1]
			if done, err := tx.tasksClaimed(current.tasks); err != nil || !done {
				return orRefused(err)
			}
			if err := give([]uint32{current.final}); err != nil {
				return err
			}
		case tecCatchupCmdRedo1:
			if len(phases) == 0 || state.Data2 == 1 {
				return errRefused
			}
			if done, err := currentDone(); err != nil || !done {
				return orRefused(err)
			}
			if !slices.Contains(state.Data1List, state.Data1) {
				state.Data1List = append(state.Data1List, state.Data1)
			}
			state.Data1, state.Data2 = 1, 1
			first := phases[0]
			for _, id := range append(slices.Clone(first.tasks), first.final) {
				if err := tx.DeleteCommanderTask(ctx, db.DeleteCommanderTaskParams{CommanderID: client.CommanderID, TaskID: id}); err != nil {
					return err
				}
			}
			if err := give(first.tasks); err != nil {
				return err
			}
		default:
			return errRefused
		}
		return tx.SaveActivity(ctx, db.SaveActivityParams(state))
	})
	if refused(err) {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	if err != nil {
		return err
	}
	return client.Send(&protobuf.SC_11203{Result: result(nil)})
}

// orRefused is err, or errRefused when there is none.
func orRefused(err error) error {
	if err != nil {
		return err
	}
	return errRefused
}
