package game

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/gamedata"
	"github.com/ggmolly/belfast/internal/logger"
	"github.com/ggmolly/belfast/internal/protobuf"
)

// The boss rush events (BossRushActivity, ACTIVITY_TYPE_BOSSRUSH): a series
// of battles fought one after the other, started by BossRushTracingCommand
// (CS_11202 cmd 1: arg1 the series, arg2 the mode) and closed by
// BossRushSettlementCommand (CS_11202 cmd 2). The battles in between are
// CS_40001 / CS_40003 with system SYSTEM_BOSS_RUSH (12), or
// SYSTEM_BOSS_RUSH_EX (13) for the extra series, which costs nothing and
// awards no exp (battle_cost_template).
//
// The client keeps the series being fought itself (GetBossRushRuntime); the
// server keeps it in the activity's data2 (series), data3 (stages won) and
// data4 (mode). The cleared series are BossRushActivity's data2_list
// (single mode) and data3_list (multiple mode), kept in activity key-value
// lists; data1_list counts the bonus uses of each series of config_data.
//
// Built from the Lua alone (never captured). GUESS: a stage won gives the
// ship and commander exp of any battle and no drops; a series cleared gives
// its pass_awards_display entries that have an amount (the event PT).

const (
	systemBossRush   = 12 // SYSTEM_BOSS_RUSH
	systemBossRushEX = 13 // SYSTEM_BOSS_RUSH_EX

	bossRushCmdTrace  = 1
	bossRushCmdSettle = 2

	bossRushModeSingle = 1 // BossRushSeriesData.MODE.SINGLE

	// activity key-value lists of the cleared series
	bossRushSingleList = 2 // data2_list
	bossRushMultiList  = 3 // data3_list
)

// activity_series_enemy row: a boss rush series
type bossRushSeriesConfig struct {
	ExpeditionID []uint32        `json:"expedition_id"`
	PassAwards   json.RawMessage `json:"pass_awards_display"`
	PreChapter   uint32          `json:"pre_chapter"`
}

var bossRushSeriesConfigs = sync.OnceValues(func() (map[uint32]bossRushSeriesConfig, error) {
	return gamedata.Table[bossRushSeriesConfig]("activity_series_enemy")
})

// passAwards are the series' rewards that have an amount: the others are
// only shown as possible drops.
func (series bossRushSeriesConfig) passAwards() [][]uint32 {
	var awards [][]uint32
	if json.Unmarshal(series.PassAwards, &awards) != nil {
		return nil
	}
	return slices.DeleteFunc(awards, func(award []uint32) bool { return len(award) < 3 || award[2] == 0 })
}

// bossRushCleared are the series cleared in single and multiple mode.
func bossRushCleared(lists map[uint32][]*protobuf.KEYVALUE_P11) (single, multi []uint32) {
	for _, entry := range lists[bossRushSingleList] {
		single = append(single, entry.GetKey())
	}
	for _, entry := range lists[bossRushMultiList] {
		multi = append(multi, entry.GetKey())
	}
	slices.Sort(single)
	slices.Sort(multi)
	return single, multi
}

// bossRushOperation answers BossRushTracingCommand and
// BossRushSettlementCommand. A refused one gets result 1.
func (game *Game) bossRushOperation(client *connection.Client, config activityConfig, req *protobuf.CS_11202) error {
	ctx := context.Background()
	configs, err := bossRushSeriesConfigs()
	if err != nil {
		return err
	}
	reply := &protobuf.SC_11203{Result: result(nil)}
	err = game.tx(ctx, client, func(tx *Tx) error {
		state, err := activityState(ctx, tx.Queries, client.CommanderID, config)
		if err != nil {
			return err
		}
		lists, err := activityKeyValues(ctx, tx.Queries, client.CommanderID)
		if err != nil {
			return err
		}
		single, multi := bossRushCleared(lists[config.ID])
		active := gamedata.IDs(config.ConfigData)

		switch req.GetCmd() {
		case bossRushCmdTrace:
			id := req.GetArg1()
			series, ok := configs[id]
			if !ok || !slices.Contains(active, id) || len(series.ExpeditionID) == 0 {
				return errRefused
			}
			if series.PreChapter != 0 && !slices.Contains(single, series.PreChapter) && !slices.Contains(multi, series.PreChapter) {
				return errRefused
			}
			state.Data2, state.Data3, state.Data4 = id, 0, req.GetArg2()
		case bossRushCmdSettle:
			id := state.Data2
			series, ok := configs[id]
			if !ok {
				return errRefused
			}
			if state.Data3 >= uint32(len(series.ExpeditionID)) {
				list, cleared := bossRushMultiList, multi
				if state.Data4 == bossRushModeSingle {
					list, cleared = bossRushSingleList, single
				}
				if !slices.Contains(cleared, id) {
					if err := tx.AddActivityKeyValue(ctx, db.AddActivityKeyValueParams{
						CommanderID: client.CommanderID, ActivityID: config.ID, ListKey: uint32(list), ItemKey: id, Value: 1,
					}); err != nil {
						return err
					}
				}
				// BossRushActivity:AddUsedBonus: a count per series of config_data
				if index := slices.Index(active, id); index >= 0 {
					for len(state.Data1List) <= index {
						state.Data1List = append(state.Data1List, 0)
					}
					state.Data1List[index]++
				}
				reply.AwardList = configDrops(series.passAwards())
				if err := tx.grantDrops(reply.AwardList); err != nil {
					return err
				}
			}
			state.Data2, state.Data3, state.Data4 = 0, 0, 0
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
	return client.Send(reply)
}

// bossRushStage finds the open boss rush whose traced series holds
// expedition, with its state.
func bossRushStage(ctx context.Context, queries *db.Queries, commanderID, expedition uint32, now time.Time) (db.CommanderActivity, bool, error) {
	configs, err := bossRushSeriesConfigs()
	if err != nil {
		return db.CommanderActivity{}, false, err
	}
	for _, config := range openActivities(now) {
		if config.Type != activityTypeBossRush {
			continue
		}
		state, err := activityState(ctx, queries, commanderID, config)
		if err != nil {
			return db.CommanderActivity{}, false, err
		}
		if series, ok := configs[state.Data2]; ok && slices.Contains(series.ExpeditionID, expedition) {
			return state, true, nil
		}
	}
	return db.CommanderActivity{}, false, nil
}

// beginBossRush starts a stage of the traced series: the normal series
// cost their start oil and morale, the extra one nothing.
func (game *Game) beginBossRush(client *connection.Client, req *protobuf.CS_40001) error {
	ctx := context.Background()
	_, ok, err := bossRushStage(ctx, game.DB.Queries, client.CommanderID, req.GetData(), time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return client.Send(&protobuf.SC_40002{Result: result(errRefused)})
	}
	if req.GetSystem() == systemBossRush {
		byID, err := shipsByID(ctx, game.DB.Queries, client.CommanderID, req.GetShipIdList())
		if err != nil {
			return err
		}
		err = game.tx(ctx, client, func(tx *Tx) error {
			if err := tx.SpendResource(ResourceOil, battleStartOil(byID)); err != nil {
				return err
			}
			for _, ship := range byID {
				ship.Energy -= min(ship.Energy, battleConsumeEnergy)
				if err := saveShipBattleState(ctx, tx.Queries, ship); err != nil {
					return err
				}
			}
			return nil
		})
		if refused(err) {
			return client.Send(&protobuf.SC_40002{Result: result(err)})
		}
		if err != nil {
			return err
		}
	}
	return client.Send(&protobuf.SC_40002{Result: result(nil), Key: proto.Uint32(rand.Uint32N(10000))})
}

// finishBossRush settles a stage: a win (score above C) counts toward the
// series, and on SYSTEM_BOSS_RUSH gives exp and costs the end oil.
func (game *Game) finishBossRush(client *connection.Client, req *protobuf.CS_40003) error {
	ctx, now := context.Background(), time.Now()
	state, ok, err := bossRushStage(ctx, game.DB.Queries, client.CommanderID, req.GetData(), now)
	if err != nil {
		return err
	}
	expedition, known := expeditionTemplates[req.GetData()]
	if !ok || !known {
		return client.Send(&protobuf.SC_40004{Result: result(errRefused)})
	}
	score := min(req.GetScore(), scoreS)
	win := score > scoreC
	reply := &protobuf.SC_40004{Result: result(nil)}
	stats := req.GetStatistics()
	statShips := make([]uint32, len(stats))
	for i, stat := range stats {
		statShips[i] = stat.GetShipId()
	}
	byID, err := shipsByID(ctx, game.DB.Queries, client.CommanderID, statShips)
	if err != nil {
		return err
	}
	exp := req.GetSystem() == systemBossRush
	var ships battleShips
	if exp {
		mvp := battleMVP(stats, byID)
		reply.Mvp = proto.Uint32(mvp)
		flagship := uint32(0)
		if len(stats) > 0 {
			flagship = stats[0].GetShipId()
		}
		ships = settleBattleShips(stats, byID, func(ship db.Ship) uint32 {
			return shipBattleExp(expedition, 1, score, ship, ship.ID == flagship, ship.ID == mvp)
		}, win, flagship, mvp)
		reply.ShipExpList = ships.exps
	}
	commander, err := game.DB.GetCommander(ctx, client.CommanderID)
	if err != nil {
		return err
	}
	err = game.tx(ctx, client, func(tx *Tx) error {
		if win {
			tx.event(TaskEvent{SubType: TaskSubWin, Count: 1})
			if kills := uint32(len(req.GetKillIdList())); kills > 0 {
				tx.event(TaskEvent{SubType: TaskSubDefeatEnemy, Count: kills})
			}
			state.Data3++
			if err := tx.SaveActivity(ctx, db.SaveActivityParams(state)); err != nil {
				return err
			}
		}
		if !exp {
			return nil
		}
		tx.event(ships.events...)
		if ships.techLevel {
			tx.techPointsChanged()
		}
		for _, ship := range ships.updated {
			if err := saveShipBattleState(ctx, tx.Queries, ship); err != nil {
				return err
			}
		}
		if err := tx.addClassProficiency(ships.proficiency, now); err != nil {
			return err
		}
		if win {
			endOil := battleEndOil(byID)
			if err := tx.SpendResource(ResourceOil, endOil); err == ErrNotEnough {
				logger.WithFields("BossRush", logger.FieldValue("oil", endOil)).Warn("not enough oil left for the end of a boss rush battle")
			} else if err != nil {
				return err
			}
		}
		reply.PlayerExp = proto.Uint32(playerBattleExp(expedition, score, uint32(len(ships.updated)), commander.Level))
		return addCommanderExp(tx, reply.GetPlayerExp())
	})
	if err != nil {
		return err
	}
	return client.Send(reply)
}
