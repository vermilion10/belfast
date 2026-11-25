package db

import (
	"context"
	"database/sql"
	"errors"
)

// profileStatsSchema is a commander's battle record as the profile shows it
// (DETAIL_INFO and SC_11003): sorties and their wins, exercises and their
// wins, commissions, and an owned-ships figure that replaces the dock count
// when not 0. The server doesn't count these itself, a commander without a
// row reads 0. It is created on open rather than by a numbered migration so
// it never takes a migration number from upstream.
const profileStatsSchema = `CREATE TABLE IF NOT EXISTS commander_profile_stats (
	commander_id         INTEGER PRIMARY KEY REFERENCES commanders (id) ON DELETE CASCADE,
	ship_count           INTEGER NOT NULL DEFAULT 0,
	attack_count         INTEGER NOT NULL DEFAULT 0,
	win_count            INTEGER NOT NULL DEFAULT 0,
	pvp_attack_count     INTEGER NOT NULL DEFAULT 0,
	pvp_win_count        INTEGER NOT NULL DEFAULT 0,
	collect_attack_count INTEGER NOT NULL DEFAULT 0
)`

// ensureProfileStats creates commander_profile_stats when it is missing.
func ensureProfileStats(conn *sql.DB) error {
	_, err := conn.Exec(profileStatsSchema)
	return err
}

// ProfileStats is a commander's commander_profile_stats row.
type ProfileStats struct {
	ShipCount          uint32
	AttackCount        uint32
	WinCount           uint32
	PvpAttackCount     uint32
	PvpWinCount        uint32
	CollectAttackCount uint32
}

// GetProfileStats reads a commander's battle record, zero without a row.
func (q *Queries) GetProfileStats(ctx context.Context, commanderID uint32) (ProfileStats, error) {
	var stats ProfileStats
	err := q.db.QueryRowContext(ctx, `SELECT ship_count, attack_count, win_count, pvp_attack_count, pvp_win_count, collect_attack_count
FROM commander_profile_stats WHERE commander_id = ?`, commanderID).Scan(
		&stats.ShipCount, &stats.AttackCount, &stats.WinCount, &stats.PvpAttackCount, &stats.PvpWinCount, &stats.CollectAttackCount)
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileStats{}, nil
	}
	return stats, err
}
