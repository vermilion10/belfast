package game

import (
	"context"
	"slices"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/protobuf"
)

// ActivityCrusingOPCommand's CS_11202 cmds on a cruise pass
// (CrusingMediator): claim everything reached, one base level, one bonus
// level, and claim everything again from the pass page's button.
const (
	cruiseCmdClaimAll    = 1
	cruiseCmdClaimBase   = 2
	cruiseCmdClaimBonus  = 3
	cruiseCmdClaimAllBtn = 4
)

// cruiseBonusClaimList is the activity key-value list holding the bonus
// track levels claimed (the client's data2_list), one item per level pt.
const cruiseBonusClaimList = 1

// cruiseBonusClaims are the pt of the bonus track levels claimed, sent as
// ACTIVITYINFO.data2_list.
func cruiseBonusClaims(lists map[uint32][]*protobuf.KEYVALUE_P11) []uint32 {
	var claims []uint32
	for _, entry := range lists[cruiseBonusClaimList] {
		claims = append(claims, entry.GetKey())
	}
	slices.Sort(claims)
	return claims
}

// cruisePoints is the commander's cruise pass pt outside a transaction.
func cruisePoints(ctx context.Context, queries *db.Queries, commanderID uint32) (uint32, error) {
	return taskCounter(ctx, queries, commanderID, TaskSubCruisePt, 0)
}

// cruiseOperation answers ActivityCrusingOPCommand: the client adds the
// awards and records the claimed levels itself on result 0. Data1 (the pt
// the page shows) is kept in step with the TaskSubCruisePt counter; the
// bonus track needs data2 (bought) set. A refused claim gets result 1.
func (game *Game) cruiseOperation(client *connection.Client, config activityConfig, req *protobuf.CS_11202) error {
	ctx := context.Background()
	tables, err := adminActivityTables()
	if err != nil {
		return err
	}
	pass, ok := tables.passes[config.ID]
	if !ok {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	reply := &protobuf.SC_11203{Result: result(nil)}
	err = game.tx(ctx, client, func(tx *Tx) error {
		state, err := activityState(ctx, tx.Queries, client.CommanderID, config)
		if err != nil {
			return err
		}
		pt, err := tx.cruisePt()
		if err != nil {
			return err
		}
		state.Data1 = pt
		lists, err := activityKeyValues(ctx, tx.Queries, client.CommanderID)
		if err != nil {
			return err
		}
		bonusClaimed := cruiseBonusClaims(lists[config.ID])
		paid := state.Data2 == 1

		var drops [][]uint32
		claimBase := func(level int) {
			target := pass.Target[level]
			if target > pt || slices.Contains(state.Data1List, target) || level >= len(pass.Award) {
				return
			}
			drops = append(drops, tables.passAwards[pass.Award[level]].Drop)
			state.Data1List = append(state.Data1List, target)
		}
		var newBonus []uint32
		claimBonus := func(level int) {
			target := pass.Target[level]
			if !paid || target > pt || slices.Contains(bonusClaimed, target) || level >= len(pass.AwardPay) {
				return
			}
			drops = append(drops, tables.passAwards[pass.AwardPay[level]].Drop)
			bonusClaimed = append(bonusClaimed, target)
			newBonus = append(newBonus, target)
		}

		switch req.GetCmd() {
		case cruiseCmdClaimAll, cruiseCmdClaimAllBtn:
			for level := range pass.Target {
				claimBase(level)
				claimBonus(level)
			}
		case cruiseCmdClaimBase, cruiseCmdClaimBonus:
			level := slices.Index(pass.Target, req.GetArg1())
			if level < 0 {
				return errRefused
			}
			if req.GetCmd() == cruiseCmdClaimBase {
				claimBase(level)
			} else {
				claimBonus(level)
			}
		default:
			return errRefused
		}
		if len(drops) == 0 {
			return errRefused
		}
		reply.AwardList = mergeDrops(configDrops(drops))
		if err := tx.grantDrops(reply.AwardList); err != nil {
			return err
		}
		slices.Sort(state.Data1List)
		if err := tx.SaveActivity(ctx, db.SaveActivityParams(state)); err != nil {
			return err
		}
		for _, target := range newBonus {
			if err := tx.AddActivityKeyValue(ctx, db.AddActivityKeyValueParams{
				CommanderID: client.CommanderID, ActivityID: config.ID, ListKey: cruiseBonusClaimList, ItemKey: target, Value: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if refused(err) {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	if err != nil {
		return err
	}
	return client.Send(reply)
}
