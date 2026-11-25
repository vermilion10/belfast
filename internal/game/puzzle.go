package game

import (
	"context"
	"slices"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/gamedata"
	"github.com/ggmolly/belfast/internal/protobuf"
)

// The hidden pieces of an easter egg event (PuzzleActivity,
// ACTIVITY_TYPE_PUZZLA, config_id 3): buttons hidden on some screens
// (EasterEggView), one per piece of config_data, shown until the piece is
// in the activity's data2_list (EasterEggTools.IsEnabled). A click sends
// CS_11202 cmd 2 (PuzzleActivity.CMD_ACTIVATE), arg1 the piece; on result 0
// the client adds it to data2_list itself. The pieces found are kept in an
// activity key-value list and sent as data2_list.
//
// Each piece stands for a random skin of a list only its description has
// (item_virtual_data_statistics), a duplicate turning into gems the tables
// don't count: no award is given for it.

const (
	activityTypePuzzle = 21 // ActivityConst.ACTIVITY_TYPE_PUZZLA
	puzzleCmdActivate  = 2  // PuzzleActivity.CMD_ACTIVATE
	puzzleFoundList    = 2  // the activity key-value list of the pieces found
)

// puzzleFound are the pieces found, sent as ACTIVITYINFO.data2_list.
func puzzleFound(lists map[uint32][]*protobuf.KEYVALUE_P11) []uint32 {
	var found []uint32
	for _, entry := range lists[puzzleFoundList] {
		found = append(found, entry.GetKey())
	}
	slices.Sort(found)
	return found
}

// puzzleOperation answers a click on a hidden piece. A piece not of the
// activity, or found already, is refused (result 1).
func (game *Game) puzzleOperation(client *connection.Client, config activityConfig, req *protobuf.CS_11202) error {
	ctx := context.Background()
	piece := req.GetArg1()
	err := game.tx(ctx, client, func(tx *Tx) error {
		if req.GetCmd() != puzzleCmdActivate || !slices.Contains(gamedata.IDs(config.ConfigData), piece) {
			return errRefused
		}
		lists, err := activityKeyValues(ctx, tx.Queries, client.CommanderID)
		if err != nil {
			return err
		}
		if slices.Contains(puzzleFound(lists[config.ID]), piece) {
			return errRefused
		}
		return tx.AddActivityKeyValue(ctx, db.AddActivityKeyValueParams{
			CommanderID: client.CommanderID, ActivityID: config.ID, ListKey: puzzleFoundList, ItemKey: piece, Value: 1,
		})
	})
	if refused(err) {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	if err != nil {
		return err
	}
	return client.Send(&protobuf.SC_11203{Result: result(nil)})
}
