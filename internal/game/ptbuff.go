package game

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/gamedata"
	"github.com/ggmolly/belfast/internal/protobuf"
)

// The event PT of a PT_BUFF_MARK2 activity (the Veritas Academy's Romantic
// Grail): an item_virtual_data_statistics row of virtual_type 103 whose
// link_id is the activity. The client reads the PT held from the
// activity's data4 (Drop's DROP_TYPE_VITEM count, virtual_type 103) and the
// PT earned so far, which its PT rewards need, from data1
// (PTBuffActivity:GetTotalPtCount); a drop adds to data4 itself
// (PlayerConst's DROP_TYPE_VITEM, virtual_type 103). The PT held are the
// virtual item belfast stores; the PT earned are kept in data1 as drops
// come in, and both are sent from there at login.

// virtualTypeActivityPt is item_virtual_data_statistics.virtual_type of an
// activity's own PT.
const virtualTypeActivityPt = 103

// ptBuffItems maps each PT_BUFF_MARK2 activity's PT item to the activity,
// and the activity back to its item.
var ptBuffItems = sync.OnceValues(func() (map[uint32]uint32, error) {
	items, err := gamedata.Table[struct {
		VirtualType uint32 `json:"virtual_type"`
		LinkID      uint32 `json:"link_id"`
	}]("item_virtual_data_statistics")
	if err != nil {
		return nil, err
	}
	links := map[uint32]uint32{}
	for id, item := range items {
		if item.VirtualType != virtualTypeActivityPt {
			continue
		}
		if config, ok := activityConfigs[item.LinkID]; ok && config.Type == activityTypePTBuffMark2 {
			links[id] = item.LinkID
		}
	}
	return links, nil
})

// ptBuffItem is the PT item of a PT_BUFF_MARK2 activity, 0 without one.
func ptBuffItem(activityID uint32) (uint32, error) {
	links, err := ptBuffItems()
	if err != nil {
		return 0, err
	}
	for item, activity := range links {
		if activity == activityID {
			return item, nil
		}
	}
	return 0, nil
}

// addPtEarned counts PT item drops toward their activity's PT earned.
func (tx *Tx) addPtEarned(itemID, amount uint32) error {
	links, err := ptBuffItems()
	if err != nil {
		return err
	}
	activityID, ok := links[itemID]
	if !ok {
		return nil
	}
	config, ok := activityConfigs[activityID]
	if !ok {
		return nil
	}
	state, err := activityState(tx.ctx, tx.Queries, tx.commanderID, config)
	if err != nil {
		return err
	}
	// the PT held before this drop were all earned, if data1 didn't count
	// them yet (earned before it was kept)
	held, err := tx.GetVirtualItem(tx.ctx, db.GetVirtualItemParams{CommanderID: tx.commanderID, ItemID: itemID})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	state.Data1 = max(state.Data1, held-min(held, amount)) + amount
	return tx.SaveActivity(tx.ctx, db.SaveActivityParams(state))
}

// ptBuffState is a PT_BUFF_MARK2 activity's PT earned (data1) and held
// (data4) for its ACTIVITYINFO. PT held before data1 counted the drops
// count as earned.
func ptBuffState(ctx context.Context, queries *db.Queries, commanderID uint32, config activityConfig, state db.CommanderActivity) (earned, held uint32, err error) {
	item, err := ptBuffItem(config.ID)
	if err != nil || item == 0 {
		return state.Data1, state.Data4, err
	}
	held, err = queries.GetVirtualItem(ctx, db.GetVirtualItemParams{CommanderID: commanderID, ItemID: item})
	if errors.Is(err, sql.ErrNoRows) {
		held, err = 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return max(state.Data1, held), held, nil
}

// ActivityNewPtOPCommand's CS_11202 cmds on a PT activity: claim the
// milestone of PT target arg1, or every milestone up to arg1 PT.
const (
	ptCmdClaim    = 1
	ptCmdClaimAll = 4
)

// activity_event_pt row: each milestone's PT target and reward
type eventPtAwards struct {
	Target     []uint32   `json:"target"`
	DropClient [][]uint32 `json:"drop_client"`
	DayUnlock  []uint32   `json:"day_unlock"`
}

var eventPtAwardConfigs = sync.OnceValues(func() (map[uint32]eventPtAwards, error) {
	return gamedata.Table[eventPtAwards]("activity_event_pt")
})

// ptBuffOperation answers ActivityNewPtOPCommand's milestone claims on a
// PT_BUFF_MARK2 activity: a milestone needs its target in PT earned, and is
// claimed once (data1_list, the targets claimed, which the client adds to
// itself on result 0). The reward is its drop_client. Milestones locked to a
// later day of the event (day_unlock) are refused. A refused claim gets
// result 1.
func (game *Game) ptBuffOperation(client *connection.Client, config activityConfig, req *protobuf.CS_11202) error {
	ctx, now := context.Background(), time.Now()
	configs, err := eventPtAwardConfigs()
	if err != nil {
		return err
	}
	pt, ok := configs[config.ID]
	if !ok {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	reply := &protobuf.SC_11203{Result: result(nil)}
	err = game.tx(ctx, client, func(tx *Tx) error {
		state, err := activityState(ctx, tx.Queries, client.CommanderID, config)
		if err != nil {
			return err
		}
		earned, _, err := ptBuffState(ctx, tx.Queries, client.CommanderID, config, state)
		if err != nil {
			return err
		}
		state.Data1 = earned
		day := uint32(0)
		if !config.Time.Start.IsZero() {
			day = uint32(now.Sub(config.Time.Start)/(24*time.Hour)) + 1
		}
		var drops [][]uint32
		claim := func(i int) {
			target := pt.Target[i]
			if target > earned || slices.Contains(state.Data1List, target) || i >= len(pt.DropClient) {
				return
			}
			if i < len(pt.DayUnlock) && pt.DayUnlock[i] > day {
				return
			}
			drops = append(drops, pt.DropClient[i])
			state.Data1List = append(state.Data1List, target)
		}
		switch req.GetCmd() {
		case ptCmdClaim:
			i := slices.Index(pt.Target, req.GetArg1())
			if i < 0 {
				return errRefused
			}
			claim(i)
		case ptCmdClaimAll:
			for i, target := range pt.Target {
				if target > req.GetArg1() {
					break
				}
				claim(i)
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
		return tx.SaveActivity(ctx, db.SaveActivityParams(state))
	})
	if refused(err) {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	if err != nil {
		return err
	}
	return client.Send(reply)
}
