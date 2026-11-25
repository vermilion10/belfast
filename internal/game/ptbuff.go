package game

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/gamedata"
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
