package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// settingOrphans prefixes the per-cluster report of namespaces without a server
// row (models.OrphanReport): one row per cluster, so that two clusters' passes
// neither overwrite nor clear each other's.
const settingOrphans = "gc.orphans."

// ReportOrphans records what a controller found on a cluster and reports
// whether it differs from what was recorded, so a caller says it aloud once per
// change and not once per pass. An empty set clears the report. When the set is
// the one already recorded, the first-seen time is kept and nothing is written.
func (s *Store) ReportOrphans(rep models.OrphanReport) (changed bool, err error) {
	key := settingOrphans + strconv.FormatUint(uint64(rep.ClusterID), 10)
	raw, err := s.GetSetting(key)
	if err != nil {
		return false, err
	}
	var prev models.OrphanReport
	havePrev := raw != "" && json.Unmarshal([]byte(raw), &prev) == nil
	if len(rep.Names) == 0 {
		if raw == "" {
			return false, nil
		}
		return true, s.db.Delete(&models.Setting{}, "key = ?", key).Error
	}
	if havePrev && reflect.DeepEqual(prev.Names, rep.Names) && prev.Refused == rep.Refused &&
		prev.Managed == rep.Managed && prev.GraceSeconds == rep.GraceSeconds {
		return false, nil
	}
	if rep.Since.IsZero() {
		rep.Since = time.Now().UTC()
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return false, err
	}
	return true, s.SetSetting(key, string(b))
}

// ListOrphanReports returns the current report of every cluster that has one.
func (s *Store) ListOrphanReports() ([]models.OrphanReport, error) {
	var rows []models.Setting
	if err := s.db.Where("key LIKE ?", settingOrphans+"%").Order("key asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]models.OrphanReport, 0, len(rows))
	for _, row := range rows {
		var rep models.OrphanReport
		if err := json.Unmarshal([]byte(row.Value), &rep); err != nil {
			return nil, fmt.Errorf("orphan report %s: %w", strings.TrimPrefix(row.Key, settingOrphans), err)
		}
		out = append(out, rep)
	}
	return out, nil
}
