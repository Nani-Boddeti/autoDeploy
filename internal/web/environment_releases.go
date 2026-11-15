package web

import (
	"context"
	"unicode/utf8"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// maxCleanupDetailRunes caps the cleanup detail shown per row so an agent
// summary (stored up to 2 KiB) cannot blow up the compact table.
const maxCleanupDetailRunes = 200

// EnvironmentReleaseReader is the optional read surface behind the "Releases"
// table of the route page (step 21C). *postgres.OperationsReadRepository
// satisfies it; it is discovered on Config.OperationsReader so no new wiring
// is needed, and an OperationsReader without it simply omits the section.
type EnvironmentReleaseReader interface {
	ListEnvironmentReleases(ctx context.Context, principal auth.Principal, environmentID string) ([]postgres.EnvironmentRelease, error)
}

// releaseRow is one row of the route page's Releases table. Cleanup reuses the
// deployment page's cleanupView (label, tone class, detail text).
type releaseRow struct {
	ID, State, Tone, Strategy, Created string
	Cleanup                            *cleanupView
}

// environmentReleaseRows returns the rows for environmentID (empty when the
// reader is unavailable or the read fails: the section is then omitted and the
// rest of the page renders as before).
func (h *Handler) environmentReleaseRows(ctx context.Context, p auth.Principal, environmentID string) []releaseRow {
	reader, ok := h.config.OperationsReader.(EnvironmentReleaseReader)
	if !ok {
		return nil
	}
	releases, err := reader.ListEnvironmentReleases(ctx, p, environmentID)
	if err != nil {
		return nil
	}
	rows := make([]releaseRow, 0, len(releases))
	for _, rel := range releases {
		row := releaseRow{ID: rel.ID, State: rel.State, Tone: stateTone(rel.State), Strategy: rel.Strategy, Created: formatTimestamp(rel.CreatedAt), Cleanup: newCleanupView(rel.Cleanup)}
		if row.Cleanup != nil {
			row.Cleanup.Detail = truncateRunes(row.Cleanup.Detail, maxCleanupDetailRunes)
		}
		rows = append(rows, row)
	}
	return rows
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
