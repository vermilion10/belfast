package game

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/ggmolly/belfast/internal/connection"
	"github.com/ggmolly/belfast/internal/db"
	"github.com/ggmolly/belfast/internal/protobuf"
)

// activityTypeBuildShip is ActivityConst.ACTIVITY_TYPE_BUILDSHIP_1, an
// event's limited construction: config_id is its ship_data_create_material
// pool, config_data its rate-up ships, [template, rate in 1/10000].
const activityTypeBuildShip = 1

// activityBuildRateScale is the denominator of a rate-up ship's rate
// (200 is the "2%" of its rate_tip).
const activityBuildRateScale = 10000

// activityBuildOperation answers ActivityOperationCommand on a limited
// construction (CS_11202): arg1 builds in the activity's pool, paid with
// use_gold gold and number_1 use_item each (arg2 1 would pay with an event
// build ticket, there is none). Each build draws a rate-up ship at its rate
// first, then from the pool as a normal build (pickBuildShip). On result 0
// the client takes the costs, adds the reply's builds and counts them in
// the activity's data1, which the server keeps too. Any refusal shows
// activity_build_end_tip on the client, whatever its reason.
func (game *Game) activityBuildOperation(client *connection.Client, config activityConfig, req *protobuf.CS_11202) error {
	ctx := context.Background()
	now := time.Now().Unix()
	pool, ok := buildPools[config.ConfigID]
	count := req.GetArg1()
	var rateUps [][2]uint32
	_ = json.Unmarshal(config.ConfigData, &rateUps)
	reply := &protobuf.SC_11203{Result: result(nil)}
	err := errRefused
	if ok && req.GetArg2() == 0 && count >= 1 && count <= maxBuilds {
		err = game.tx(ctx, client, func(tx *Tx) error {
			builds, err := tx.ListBuilds(ctx, client.CommanderID)
			if err != nil {
				return err
			}
			if len(builds)+int(count) > maxBuilds {
				return errRefused
			}
			if err := tx.SpendResource(ResourceGold, pool.Gold*count); err != nil {
				return err
			}
			if err := tx.SpendItem(pool.Item, pool.ItemCount*count); err != nil {
				return err
			}
			for range count {
				template, duration, ok := pickActivityBuildShip(pool, rateUps)
				if !ok {
					return errRefused
				}
				start := buildStart(builds, now)
				build, err := tx.CreateBuild(ctx, db.CreateBuildParams{
					CommanderID: client.CommanderID,
					PoolID:      config.ConfigID,
					TemplateID:  template,
					Duration:    duration,
					FinishTime:  start + int64(duration),
				})
				if err != nil {
					return err
				}
				builds = append(builds, build)
				reply.Build = append(reply.Build, buildInfo(build))
			}
			state, err := activityState(ctx, tx.Queries, client.CommanderID, config)
			if err != nil {
				return err
			}
			state.Data1 += count
			if err := tx.SaveActivity(ctx, db.SaveActivityParams(state)); err != nil {
				return err
			}
			tx.event(TaskEvent{SubType: TaskSubBuild, Count: count})
			return tx.addBuildPoints(int64(pool.ExchangeCount) * int64(count))
		})
	}
	if refused(err) {
		return client.Send(&protobuf.SC_11203{Result: result(errRefused)})
	}
	if err != nil {
		return err
	}
	return client.Send(reply)
}

// pickActivityBuildShip draws a limited construction's ship: each rate-up
// ship at its rate, otherwise a normal draw from the pool.
func pickActivityBuildShip(pool buildPool, rateUps [][2]uint32) (template uint32, duration uint32, ok bool) {
	roll := rand.Uint32N(activityBuildRateScale)
	for _, rateUp := range rateUps {
		if roll < rateUp[1] {
			if _, known := ownableShips[rateUp[0]]; known {
				return rateUp[0], buildTimes[rateUp[0]], true
			}
			break
		}
		roll -= rateUp[1]
	}
	return pickBuildShip(pool)
}
